package cloudasr

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/correct"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

const (
	// MaxChunkSec 是一次发给在线识别的最长秒数：接口上限是 120 秒，留余量合到 100 秒（16k 16位 WAV 约 3.2 MB，也在 8 MiB 以内）。
	MaxChunkSec = 100.0
	// HardMaxChunkSec 是接口允许的硬上限附近：极端情况下单个语音区间超过它时不再外发，直接用本机识别。
	HardMaxChunkSec = 118.0
	// DefaultConcurrency 是单场会议默认同时发几个在线识别请求。
	DefaultConcurrency = 4
	// MinMatchRatio 是对齐时「对上的字 / 在线识别总字数」的最低比例；低于它说明两边差别太大，这一段退回用本机识别。
	MinMatchRatio = 0.5
	// 单段对齐矩阵的最大格子数，防止异常输入占太多内存
	maxDPMatrix = 4_000_000
)

// Result 是一场录音走完在线识别 + 对齐 + 退回后的结果。
type Result struct {
	Segments  []transcript.Segment
	Chunks    int   // 一共切了多少段
	Succeeded int   // 在线接口成功返回了多少段
	Aligned   int   // 最终采用在线识别文字的有多少段（其余段退回本机识别）
	AuthErr   error // 遇到 401/402/403 时非空，整场退回本机识别
	Err       error // 第一个请求错误（日志和提示用）
}

// Transcribe 按 VAD 的停顿把录音切成不超过 MaxChunkSec 秒的段，并发发给 p 识别，
// 再用 spans 里本机 SenseVoice 的字级时间给每段文字定时间、切句；单段失败或对不上时那一段退回用 SenseVoice，
// 遇到 401/402/403 时取消剩余请求、整场退回 SenseVoice。
func Transcribe(
	ctx context.Context,
	p Provider,
	samples []float32,
	spans []transcript.Speech,
	concurrency int,
	progress func(done, total int),
) Result {
	chunks := ChunkSpans(spans, MaxChunkSec)
	if len(chunks) == 0 {
		return Result{}
	}
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type chunkOut struct {
		segs      []transcript.Segment
		succeeded bool
		aligned   bool
		err       error
	}
	outs := make([]chunkOut, len(chunks))
	limit := make(chan struct{}, concurrency)
	var (
		doneCount atomic.Int32
		authOnce  sync.Once
		authErr   error
		wg        sync.WaitGroup
	)

	totalSamples := len(samples)
	for i, group := range chunks {
		wg.Go(func() {
			defer func() {
				d := int(doneCount.Add(1))
				if progress != nil {
					progress(d, len(chunks))
				}
			}()

			start, end := group[0].Start, group[len(group)-1].End
			if end-start > HardMaxChunkSec {
				outs[i] = chunkOut{segs: fallbackSegments(group)}
				return
			}
			select {
			case limit <- struct{}{}:
			case <-reqCtx.Done():
				outs[i] = chunkOut{segs: fallbackSegments(group), err: reqCtx.Err()}
				return
			}
			defer func() { <-limit }()

			from := min(max(int(start*audio.SampleRate), 0), totalSamples)
			to := min(max(int(end*audio.SampleRate), from), totalSamples)
			if to <= from {
				outs[i] = chunkOut{segs: fallbackSegments(group)}
				return
			}

			text, err := p.Recognize(reqCtx, samples[from:to])
			if err != nil {
				if IsAuthError(err) {
					authOnce.Do(func() {
						authErr = err
						cancel()
					})
				}
				outs[i] = chunkOut{segs: fallbackSegments(group), err: err}
				return
			}
			segs, ok := Align(group, text)
			outs[i] = chunkOut{segs: segs, succeeded: true, aligned: ok}
		})
	}
	wg.Wait()

	if authErr != nil {
		return Result{
			Segments: fallbackSegments(spans),
			Chunks:   len(chunks),
			AuthErr:  authErr,
			Err:      authErr,
		}
	}

	var res Result
	res.Chunks = len(chunks)
	for _, o := range outs {
		if o.succeeded {
			res.Succeeded++
		}
		if o.aligned {
			res.Aligned++
		}
		if o.err != nil && res.Err == nil && !errorsIsCancel(ctx, o.err) {
			res.Err = o.err
		}
		res.Segments = append(res.Segments, o.segs...)
	}
	return res
}

func errorsIsCancel(parent context.Context, err error) bool {
	return parent.Err() == nil && errors.Is(err, context.Canceled)
}

// ChunkSpans 把相邻的语音区间连同中间的停顿合到最长 maxSec 秒一段；只在停顿处切，不拆单个语音区间。
// maxSec <= 0 时每个语音区间单独成段。
func ChunkSpans(spans []transcript.Speech, maxSec float64) [][]transcript.Speech {
	var out [][]transcript.Speech
	for i := 0; i < len(spans); {
		start := spans[i].Start
		j := i + 1
		for maxSec > 0 && j < len(spans) && spans[j].End-start <= maxSec {
			j++
		}
		out = append(out, spans[i:j])
		i = j
	}
	return out
}

func fallbackSegments(spans []transcript.Speech) []transcript.Segment {
	var out []transcript.Segment
	for _, sp := range spans {
		out = append(out, slices.Clone(sp.Segments)...)
	}
	return out
}

// Align 把在线识别在 spans 这段录音上返回的文字 text 对齐到 SenseVoice 的字级时间上，再按标点切成句子。
// 对上的字不到在线识别文字的一半（或者两边对不上）时退回用 spans 里 SenseVoice 原本的句子，ok 为 false。
func Align(spans []transcript.Speech, text string) (segs []transcript.Segment, ok bool) {
	if len(spans) == 0 {
		return nil, false
	}
	trimmed := strings.TrimSpace(text)
	svUnits := speechUnits(spans)
	if trimmed == "" {
		// 本机也没识别出字（比如纯噪音区间）：两边一致，算对齐成功
		if len(svUnits) == 0 {
			return nil, true
		}
		return fallbackSegments(spans), false
	}

	surface, dsUnits := parseSurface(trimmed)
	if len(dsUnits) == 0 {
		if len(svUnits) == 0 {
			return nil, true
		}
		return fallbackSegments(spans), false
	}
	if len(svUnits) == 0 || len(svUnits)*len(dsUnits) > maxDPMatrix {
		return fallbackSegments(spans), false
	}

	times, matched := alignTimes(svUnits, dsUnits, spans[0].Start, spans[len(spans)-1].End)
	if !enoughMatch(len(svUnits), len(dsUnits), matched) {
		return fallbackSegments(spans), false
	}

	tokens := assignTokenTimes(surface, times, spans[len(spans)-1].End)
	segs = transcript.Sentences(tokens, 0, spans[len(spans)-1].End)
	clampAcrossSilences(segs, surface, times, spans)
	return segs, true
}

// enoughMatch 判断对上的字够不够给在线识别结果定时间轴：
//   - 常规情况：在线识别出的字至少一半在 SenseVoice 里对得上；
//   - 小声发言恢复：会议里离麦克风远的人说话时 SenseVoice 常整句漏掉（dsUnits 比 svUnits 多一两倍），
//     只要本机听清的部分本身足够长（>=8 字）、规模差距不超过 5 倍且其中至少 60% 按顺序对得上，
//     就说明是同一段录音，漏掉的字靠前后锚点插值定时间，不退回漏字的本机结果。
func enoughMatch(svLen, dsLen, matched int) bool {
	if float64(matched) >= float64(dsLen)*MinMatchRatio {
		return true
	}
	return svLen >= 8 && dsLen <= svLen*5 && float64(matched) >= float64(svLen)*0.6
}

type timedUnit struct {
	unit string
	time float64
}

// speechUnits 把 spans 里本机识别出的 Token 展平为带绝对秒数的对齐单位（数字已换成汉字读法、英文转小写）。
func speechUnits(spans []transcript.Speech) []timedUnit {
	var out []timedUnit
	for _, sp := range spans {
		toks := sp.Tokens
		if len(toks) == 0 && len(sp.Segments) > 0 {
			toks = synthTokens(sp)
		}
		var b strings.Builder
		var byteTime []float64
		for _, tok := range toks {
			piece := strings.ReplaceAll(tok.Text, "▁", " ")
			if piece == "" {
				continue
			}
			t := min(max(sp.Start+tok.Start, sp.Start), sp.End)
			b.WriteString(piece)
			for range len(piece) {
				byteTime = append(byteTime, t)
			}
		}
		out = append(out, extractTimedUnits(b.String(), byteTime)...)
	}
	return out
}

// synthTokens 在模型没返回字级时间时，把句子的每个字均匀摊在句子区间里兜底。
func synthTokens(sp transcript.Speech) []transcript.Token {
	var out []transcript.Token
	for _, seg := range sp.Segments {
		runes := []rune(seg.Text)
		if len(runes) == 0 {
			continue
		}
		dur := max(seg.End-seg.Start, 0)
		for i, r := range runes {
			t := (seg.Start + dur*float64(i)/float64(len(runes))) - sp.Start
			out = append(out, transcript.Token{Text: string(r), Start: max(t, 0)})
		}
	}
	return out
}

// extractTimedUnits 先把 s 里的数字换成汉字读法（时间在数字首尾之间插值），再按 correct.Tokens 一样的规则拆成单位。
func extractTimedUnits(s string, byteTime []float64) []timedUnit {
	if s == "" {
		return nil
	}
	var exp strings.Builder
	var expTime []float64
	appendRun := func(sub string, t0, t1 float64) {
		runes := []rune(sub)
		for idx, r := range runes {
			t := t0
			if len(runes) > 1 {
				t = t0 + (t1-t0)*float64(idx)/float64(len(runes)-1)
			}
			rs := string(r)
			exp.WriteString(rs)
			for range len(rs) {
				expTime = append(expTime, t)
			}
		}
	}

	locs := numberPattern.FindAllStringIndex(s, -1)
	last := 0
	for _, loc := range locs {
		if last < loc[0] {
			exp.WriteString(s[last:loc[0]])
			expTime = append(expTime, byteTime[last:loc[0]]...)
		}
		t0, t1 := byteTime[loc[0]], byteTime[loc[1]-1]
		appendRun(readNumber(s[loc[0]:loc[1]], s[loc[1]:]), t0, t1)
		last = loc[1]
	}
	if last < len(s) {
		exp.WriteString(s[last:])
		expTime = append(expTime, byteTime[last:]...)
	}

	str := exp.String()
	var out []timedUnit
	var word strings.Builder
	wordTime := 0.0
	flush := func() {
		if word.Len() > 0 {
			out = append(out, timedUnit{unit: word.String(), time: wordTime})
			word.Reset()
		}
	}
	for i := 0; i < len(str); {
		r, size := utf8.DecodeRuneInString(str[i:])
		t := expTime[i]
		switch {
		case isCJK(r):
			flush()
			out = append(out, timedUnit{unit: string(r), time: t})
		case r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)) || r == '\'' && word.Len() > 0:
			if word.Len() == 0 {
				wordTime = t
			}
			word.WriteRune(unicode.ToLower(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if word.Len() == 0 {
				wordTime = t
			}
			word.WriteString(strings.ToLower(string(widthFold(r))))
		default:
			flush()
		}
		i += size
	}
	flush()
	return out
}

// surfacePiece 是在线识别原文切出来的一小块（保留原本的数字、大小写、标点和空格），
// firstUnit 是它在 dsUnits 里对应的第一个对齐单位的下标（标点和空格为 -1）。
type surfacePiece struct {
	text      string
	firstUnit int
	unitCount int
}

// parseSurface 把在线识别返回的文字切成交给 transcript.Sentences 的表面 Token，同时抽出对齐单位。
func parseSurface(s string) ([]surfacePiece, []string) {
	numLocs := numberPattern.FindAllStringIndex(s, -1)
	numIdx := 0

	var pieces []surfacePiece
	var units []string
	addContent := func(raw, spoken string) {
		u := correct.Tokens(spoken)
		if len(u) == 0 {
			pieces = append(pieces, surfacePiece{text: raw, firstUnit: -1})
			return
		}
		pieces = append(pieces, surfacePiece{text: raw, firstUnit: len(units), unitCount: len(u)})
		units = append(units, u...)
	}

	for i := 0; i < len(s); {
		for numIdx < len(numLocs) && numLocs[numIdx][0] < i {
			numIdx++
		}
		if numIdx < len(numLocs) && numLocs[numIdx][0] == i {
			end := numLocs[numIdx][1]
			raw := s[i:end]
			addContent(raw, readNumber(raw, s[end:]))
			i = end
			numIdx++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case unicode.IsSpace(r):
			j := i + size
			for j < len(s) {
				nr, nsize := utf8.DecodeRuneInString(s[j:])
				if !unicode.IsSpace(nr) {
					break
				}
				j += nsize
			}
			pieces = append(pieces, surfacePiece{text: " ", firstUnit: -1})
			i = j
		case isCJK(r):
			raw := s[i : i+size]
			addContent(raw, raw)
			i += size
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			j := i + size
			for j < len(s) {
				// 遇到数字串的起点时停下，交给上面的 numberPattern 按读法拆（比如 Qwen3 → qwen + 三）
				for numIdx < len(numLocs) && numLocs[numIdx][0] < j {
					numIdx++
				}
				if numIdx < len(numLocs) && numLocs[numIdx][0] == j {
					break
				}
				nr, nsize := utf8.DecodeRuneInString(s[j:])
				if unicode.IsLetter(nr) && !isCJK(nr) {
					j += nsize
					continue
				}
				if nr == '\'' && j+nsize < len(s) {
					after, _ := utf8.DecodeRuneInString(s[j+nsize:])
					if unicode.IsLetter(after) && !isCJK(after) {
						j += nsize
						continue
					}
				}
				break
			}
			raw := s[i:j]
			addContent(raw, raw)
			i = j
		default:
			pieces = append(pieces, surfacePiece{text: s[i : i+size], firstUnit: -1})
			i += size
		}
	}
	return pieces, units
}

const (
	opDiag uint8 = iota // 对上或替换：借用 SenseVoice 这个字的时间
	opDel               // SenseVoice 多出来的字：丢掉
	opIns               // 在线识别多出来的字：在前后有时间的字之间均匀摊开
)

// alignTimes 做编辑距离对齐，返回 dsUnits 每个单位分到的时间，以及严格对上（完全相同）的单位数。
func alignTimes(sv []timedUnit, ds []string, chunkStart, chunkEnd float64) (times []float64, matched int) {
	n, m := len(sv), len(ds)
	stride := m + 1
	ops := make([]uint8, (n+1)*stride)
	prev := make([]int, stride)
	cur := make([]int, stride)
	for j := 1; j <= m; j++ {
		prev[j] = j
		ops[j] = opIns
	}
	for i := 1; i <= n; i++ {
		cur[0] = i
		row := i * stride
		ops[row] = opDel
		for j := 1; j <= m; j++ {
			cost := 1
			if sv[i-1].unit == ds[j-1] {
				cost = 0
			}
			diag := prev[j-1] + cost
			del := prev[j] + 1
			ins := cur[j-1] + 1
			// 代价并列时优先走对角线（替换直接借时间，比一删一插更准）
			switch {
			case diag <= del && diag <= ins:
				cur[j] = diag
				ops[row+j] = opDiag
			case del <= ins:
				cur[j] = del
				ops[row+j] = opDel
			default:
				cur[j] = ins
				ops[row+j] = opIns
			}
		}
		prev, cur = cur, prev
	}

	times = make([]float64, m)
	anchored := make([]bool, m)
	for i, j := n, m; i > 0 || j > 0; {
		switch ops[i*stride+j] {
		case opDiag:
			times[j-1] = sv[i-1].time
			anchored[j-1] = true
			if sv[i-1].unit == ds[j-1] {
				matched++
			}
			i--
			j--
		case opDel:
			i--
		case opIns:
			j--
		}
	}

	// 给没有对上 SenseVoice 的插入字均匀插值时间
	for j := 0; j < m; {
		if anchored[j] {
			j++
			continue
		}
		end := j + 1
		for end < m && !anchored[end] {
			end++
		}
		k := end - j
		switch {
		case j == 0 && end == m:
			// 整段没有一个锚点（正常已被 MinMatchRatio 拦住，防御性兜底）
			for x := range k {
				times[j+x] = chunkStart + (chunkEnd-chunkStart)*float64(x)/float64(k)
			}
		case j == 0:
			// 落在段的开头：从 chunkStart 摊到第一个有时间的字
			t1 := max(times[end], chunkStart)
			for x := range k {
				times[j+x] = chunkStart + (t1-chunkStart)*float64(x)/float64(k)
			}
		case end == m:
			// 落在段的结尾：从最后一个有时间的字摊到 chunkEnd（留最后一份给句尾时长）
			t0 := times[j-1]
			t1 := max(chunkEnd, t0)
			for x := range k {
				times[j+x] = t0 + (t1-t0)*float64(x+1)/float64(k+1)
			}
		default:
			// 落在两个有时间的字之间
			t0 := times[j-1]
			t1 := max(times[end], t0)
			for x := range k {
				times[j+x] = t0 + (t1-t0)*float64(x+1)/float64(k+1)
			}
		}
		j = end
	}
	return times, matched
}

// assignTokenTimes 把单位的时间赋回表面 Token：标点和空格取后面最近一个实词的时间（末尾的取 chunkEnd），
// 这样 transcript.Sentences 在句号处断句时，上一句的 End 正好停在下一句开头。
func assignTokenTimes(pieces []surfacePiece, unitTimes []float64, chunkEnd float64) []transcript.Token {
	out := make([]transcript.Token, len(pieces))
	nextTime := chunkEnd
	for i := len(pieces) - 1; i >= 0; i-- {
		p := pieces[i]
		if p.firstUnit >= 0 {
			nextTime = unitTimes[p.firstUnit]
		}
		out[i] = transcript.Token{Text: p.text, Start: nextTime}
	}
	return out
}

// clampAcrossSilences 收紧跨过 VAD 静音空档的句尾时间：
// 一句话最后一个字落在某个语音区间 sp 里，而 Sentences 把它的 End 拉到了下一个语音区间（跨过了中间的静音）时，
// 把 End 收回到 sp.End，免得静音后另一个人刚开口的声音被算进上一句。
func clampAcrossSilences(segs []transcript.Segment, pieces []surfacePiece, unitTimes []float64, spans []transcript.Speech) {
	if len(segs) <= 1 || len(spans) <= 1 {
		return
	}
	var contentTimes []float64
	for _, p := range pieces {
		if p.firstUnit >= 0 {
			for k := range p.unitCount {
				contentTimes = append(contentTimes, unitTimes[p.firstUnit+k])
			}
		}
	}
	for i := range len(segs) - 1 {
		nextStart := segs[i+1].Start
		lastT := segs[i].Start
		for _, t := range contentTimes {
			if t >= segs[i].Start && t < nextStart {
				lastT = max(lastT, t)
			}
		}
		for _, sp := range spans {
			if sp.End >= lastT {
				if segs[i].End > sp.End {
					segs[i].End = max(sp.End, segs[i].Start)
				}
				break
			}
		}
	}
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

func widthFold(r rune) rune {
	if r >= '！' && r <= '～' {
		return r - '！' + '!'
	}
	return r
}
