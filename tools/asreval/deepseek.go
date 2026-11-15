package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/cloudasr"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

type limitedProvider struct {
	inner cloudasr.Provider
	limit chan struct{}
	mu    sync.Mutex
	took  time.Duration
}

func newLimitedProvider(f flags) *limitedProvider {
	return &limitedProvider{
		inner: &cloudasr.DeepSeek{URL: f.deepseekURL, APIKey: f.deepseekKey},
		limit: make(chan struct{}, max(f.deepseekConcurrency, 1)),
	}
}

func (p *limitedProvider) Recognize(ctx context.Context, samples []float32) (string, error) {
	select {
	case p.limit <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-p.limit }()
	t := time.Now()
	txt, err := p.inner.Recognize(ctx, samples)
	p.mu.Lock()
	p.took += time.Since(t)
	p.mu.Unlock()
	return txt, err
}

// recordingProvider 旁路记下每个请求返回的原文，让「合到100秒（不对齐）」和「线上（对齐+退回）」共用同一批接口返回，
// 既不重复发请求，也不会因为两次请求结果略有随机差异而干扰对齐前后的对比。
type recordingProvider struct {
	inner cloudasr.Provider
	mu    sync.Mutex
	texts map[[32]byte]string
}

func (r *recordingProvider) Recognize(ctx context.Context, samples []float32) (string, error) {
	txt, err := r.inner.Recognize(ctx, samples)
	if err == nil {
		key := hashSamples(samples)
		r.mu.Lock()
		r.texts[key] = txt
		r.mu.Unlock()
	}
	return txt, err
}

func hashSamples(samples []float32) [32]byte {
	h := sha256.New()
	var buf [4]byte
	for _, s := range samples {
		binary.LittleEndian.PutUint32(buf[:], math.Float32bits(s))
		h.Write(buf[:])
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// deepseekOnlineASR 跑和线上完全一致的流程：VAD 区间合到 MaxChunkSec → 调 DeepSeek → 对齐回 SenseVoice 字级时间再按标点分句（对不上退回 SenseVoice）。
// 同时用同一批接口返回拼出不对齐的对照结果 unaligned（每段保持约 100 秒整段），直接对比对齐前后的 CER 和 cpCER。
func deepseekOnlineASR(ctx context.Context, f flags, clips []clip, speeches [][]transcript.Speech) (unaligned, online result, err error) {
	const name = "deepseek-asr(线上)"
	rawName := fmt.Sprintf("deepseek-asr(合到%.0f秒)", cloudasr.MaxChunkSec)
	unaligned = result{name: rawName, segs: make([][]transcript.Segment, len(clips))}
	online = result{name: name, segs: make([][]transcript.Segment, len(clips))}
	provider := newLimitedProvider(f)

	var (
		mu                         sync.Mutex
		chunks, succeeded, aligned int
		firstErr                   error
		wg                         sync.WaitGroup
	)
	log.Printf("%s：识别并对齐时间轴…", name)
	started := time.Now()
	for i, c := range clips {
		wg.Go(func() {
			rec := &recordingProvider{inner: provider, texts: make(map[[32]byte]string)}
			res := cloudasr.Transcribe(ctx, rec, c.samples, speeches[i], max(f.deepseekConcurrency, 1), nil)

			var rawSegs []transcript.Segment
			for _, group := range cloudasr.ChunkSpans(speeches[i], cloudasr.MaxChunkSec) {
				start, end := group[0].Start, group[len(group)-1].End
				from := min(max(int(start*audio.SampleRate), 0), len(c.samples))
				to := min(max(int(end*audio.SampleRate), from), len(c.samples))
				txt := ""
				if to > from {
					txt = rec.texts[hashSamples(c.samples[from:to])]
				}
				rawSegs = append(rawSegs, transcript.Segment{Start: start, End: end, Speaker: -1, Text: txt})
			}

			mu.Lock()
			defer mu.Unlock()
			unaligned.segs[i] = rawSegs
			online.segs[i] = res.Segments
			chunks += res.Chunks
			succeeded += res.Succeeded
			aligned += res.Aligned
			if res.Err != nil && firstErr == nil {
				firstErr = res.Err
			}
		})
	}
	wg.Wait()
	wall := time.Since(started)
	online.wall, online.took = wall, provider.took
	unaligned.wall, unaligned.took = wall, provider.took
	if err := ctx.Err(); err != nil {
		return unaligned, online, err
	}
	fallback := chunks - aligned
	if firstErr != nil {
		log.Printf("%s：共 %d 段，成功 %d 段，时间对齐 %d 段，退回本机 %d 段（首个错误：%v）", name, chunks, succeeded, aligned, fallback, firstErr)
	} else {
		log.Printf("%s：共 %d 段，全部请求成功，时间对齐 %d 段，退回本机 %d 段", name, chunks, aligned, fallback)
	}
	return unaligned, online, nil
}

// deepseekASR 用 DeepSeek 在线识别直接按时间段发请求、不对齐回字级时间：
// chunk 为 0 时一句一个请求；否则把相邻的句子连同中间的停顿合到最长 chunk 秒再发。
func deepseekASR(ctx context.Context, f flags, clips []clip, from result, chunk float64) (result, error) {
	name := "deepseek-asr(按句)"
	if chunk > 0 {
		name = fmt.Sprintf("deepseek-asr(合到%.0f秒)", chunk)
	}
	type job struct{ clip, seg int }
	r := result{name: name, segs: make([][]transcript.Segment, len(clips))}
	var jobs []job
	for i, c := range clips {
		for _, s := range spans(from.segs[i], chunk, c.seconds()) {
			r.segs[i] = append(r.segs[i], transcript.Segment{Start: s[0], End: s[1], Speaker: -1})
			jobs = append(jobs, job{i, len(r.segs[i]) - 1})
		}
	}
	log.Printf("%s：%d 个请求…", name, len(jobs))

	provider := newLimitedProvider(f)
	var (
		mu     sync.Mutex
		failed int
		first  error
		wg     sync.WaitGroup
	)
	started := time.Now()
	for _, j := range jobs {
		wg.Go(func() {
			seg := &r.segs[j.clip][j.seg]
			c := clips[j.clip]
			fromIdx, toIdx := int(seg.Start*audio.SampleRate), min(int(seg.End*audio.SampleRate), len(c.samples))
			txt, err := provider.Recognize(ctx, c.samples[fromIdx:toIdx])
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				if first == nil {
					first = err
				}
				return
			}
			seg.Text = txt
		})
	}
	wg.Wait()
	r.wall = time.Since(started)
	r.took = provider.took
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if failed > 0 {
		log.Printf("%s：%d 个请求里失败 %d 个（算作没识别出字），第一个错误：%v", name, len(jobs), failed, first)
	} else {
		log.Printf("%s：%d 个请求全部成功", name, len(jobs))
	}
	return r, nil
}

// spans 是要发给接口的时间段。chunk 为 0 时就是每一句；否则从一句的开头起，往后合并到不超过 chunk 秒。
func spans(segs []transcript.Segment, chunk, total float64) [][2]float64 {
	var out [][2]float64
	for i := 0; i < len(segs); {
		start, end := segs[i].Start, segs[i].End
		j := i + 1
		for chunk > 0 && j < len(segs) && segs[j].End-start <= chunk {
			end = segs[j].End
			j++
		}
		out = append(out, [2]float64{start, min(end, total)})
		i = j
	}
	return out
}

// deepseekReplay 读之前 -out 保存下来的「deepseek-asr(合到N秒)」目录（每段录音一个同名 .txt，一行对应一段请求返回的文字），
// 不发网络请求，直接重跑 cloudasr.Align 时间对齐和退回逻辑，方便离线检验和调参。
func deepseekReplay(f flags, clips []clip, from result, speeches [][]transcript.Speech) (unaligned, online result, err error) {
	rawName := fmt.Sprintf("deepseek-asr(合到%.0f秒)", f.deepseekChunk)
	const onlineName = "deepseek-asr(线上)"
	unaligned = result{name: rawName, segs: make([][]transcript.Segment, len(clips)), postOnly: true}
	online = result{name: onlineName, segs: make([][]transcript.Segment, len(clips)), postOnly: true}

	started := time.Now()
	var totalChunks, alignedChunks int
	for i, c := range clips {
		stem := strings.TrimSuffix(c.name, filepath.Ext(c.name))
		path := filepath.Join(f.deepseekReplay, stem+".txt")
		raw, err := os.ReadFile(path)
		if err != nil {
			return unaligned, online, fmt.Errorf("读回放文件 %s：%w", path, err)
		}
		trimmed := strings.TrimSuffix(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
		var lines []string
		if trimmed != "" {
			lines = strings.Split(trimmed, "\n")
		}
		spList := spans(from.segs[i], f.deepseekChunk, c.seconds())
		if len(lines) != len(spList) {
			return unaligned, online, fmt.Errorf("回放 %s：文件有 %d 段，当前切出 %d 段（请确认 -filter 与生成该目录时一致）", path, len(lines), len(spList))
		}
		for k, s := range spList {
			totalChunks++
			txt := lines[k]
			unaligned.segs[i] = append(unaligned.segs[i], transcript.Segment{Start: s[0], End: s[1], Speaker: -1, Text: txt})
			group := sliceSpeeches(speeches[i], s[0], s[1])
			segs, ok := cloudasr.Align(group, txt)
			if ok {
				alignedChunks++
			} else {
				log.Printf("%s：退回本机 %s [%.1f-%.1fs]", onlineName, c.name, s[0], s[1])
			}
			online.segs[i] = append(online.segs[i], segs...)
		}
	}
	took := time.Since(started)
	online.took, online.wall = took, took
	log.Printf("%s（离线回放）：共 %d 段，时间对齐 %d 段，退回本机 %d 段", onlineName, totalChunks, alignedChunks, totalChunks-alignedChunks)
	return unaligned, online, nil
}

// sliceSpeeches 取出 speeches 里落在 [start, end] 这一段里的语音和字级时间。
func sliceSpeeches(speeches []transcript.Speech, start, end float64) []transcript.Speech {
	const eps = 1e-4
	var out []transcript.Speech
	for _, sp := range speeches {
		if sp.End <= start+eps || sp.Start >= end-eps {
			continue
		}
		if sp.Start >= start-eps && sp.End <= end+eps {
			out = append(out, sp)
			continue
		}
		subStart := max(sp.Start, start)
		subEnd := min(sp.End, end)
		var segs []transcript.Segment
		for _, seg := range sp.Segments {
			mid := (seg.Start + seg.End) / 2
			if mid >= start-eps && mid <= end+eps {
				segs = append(segs, seg)
			}
		}
		var toks []transcript.Token
		for _, tok := range sp.Tokens {
			absT := sp.Start + tok.Start
			if absT >= subStart-eps && absT <= subEnd+eps {
				toks = append(toks, transcript.Token{Text: tok.Text, Start: max(absT-subStart, 0)})
			}
		}
		if len(segs) > 0 || len(toks) > 0 {
			out = append(out, transcript.Speech{Start: subStart, End: subEnd, Tokens: toks, Segments: segs})
		}
	}
	return out
}
