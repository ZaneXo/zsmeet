// asrdata 把公开的语音识别数据集整理成 asreval 要的样子：一个目录里每段录音（16k 单声道 wav）
// 配一个同名的 .txt 标准文字，有热词的再配一个同名的 .hotwords.txt。
//
//	go run ./tools/asrdata textgrid -audio 录音目录 -textgrid 标注目录 -out 输出目录 [-channel 0]
//	go run ./tools/asrdata contextasr -jsonl ContextASR-Dialogue_Mandarin.jsonl -audio 解压出的录音目录 -n 60 -out 输出目录
//
// textgrid：AliMeeting（OpenSLR 119）、AISHELL-4（OpenSLR 111）这类会议数据集。录音是多声道的麦克风阵列，
// 取其中一路（默认第 0 路），就和用一个麦克风录会议一样；标注是 Praat 的 TextGrid，每位说话人一层，
// 所有人的话按开始时间排好拼成标准文字。标注文件名是录音文件名的前缀（R8001_M8004.TextGrid 对 R8001_M8004_MS801.wav）。
//
// contextasr：ContextASR-Bench（Hugging Face 上的 MrSupW/ContextASR-Bench），每条带一份专有名词列表，
// 当热词用。录音按 tar 分包放，先下载一两个分包解压；从已有录音的条目里按编号均匀挑 n 条。
// 录音是语音合成的，不是真人录的。
package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "textgrid":
		err = textgridCmd(os.Args[2:])
	case "contextasr":
		err = contextASRCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法：asrdata textgrid|contextasr [参数]，加 -h 看参数")
	os.Exit(2)
}

func textgridCmd(args []string) error {
	fs := flag.NewFlagSet("textgrid", flag.ExitOnError)
	audioDir := fs.String("audio", "", "录音目录（wav、flac）")
	gridDir := fs.String("textgrid", "", "TextGrid 标注目录")
	out := fs.String("out", "", "输出目录")
	channel := fs.Int("channel", 0, "取第几路声道（从 0 数）")
	fs.Parse(args)
	if *audioDir == "" || *gridDir == "" || *out == "" {
		fs.Usage()
		os.Exit(2)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	audios, err := os.ReadDir(*audioDir)
	if err != nil {
		return err
	}
	grids, err := filepath.Glob(filepath.Join(*gridDir, "*.TextGrid"))
	if err != nil {
		return err
	}
	if len(grids) == 0 {
		return fmt.Errorf("%s 里没有 .TextGrid", *gridDir)
	}
	slices.Sort(grids)
	for _, g := range grids {
		base := strings.TrimSuffix(filepath.Base(g), filepath.Ext(g))
		i := slices.IndexFunc(audios, func(e os.DirEntry) bool {
			ext := strings.ToLower(filepath.Ext(e.Name()))
			return strings.HasPrefix(e.Name(), base) && (ext == ".wav" || ext == ".flac")
		})
		if i < 0 {
			log.Printf("跳过 %s：找不到对应的录音", base)
			continue
		}
		intervals, err := parseTextGrid(g)
		if err != nil {
			return fmt.Errorf("%s：%w", g, err)
		}
		slices.SortStableFunc(intervals, func(a, b interval) int { return cmp.Compare(a.start, b.start) })
		var lines []string
		for _, iv := range intervals {
			if t := cleanText(iv.text); t != "" {
				lines = append(lines, t)
			}
		}
		if err := os.WriteFile(filepath.Join(*out, base+".txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			return err
		}
		src := filepath.Join(*audioDir, audios[i].Name())
		dst := filepath.Join(*out, base+".wav")
		if err := ffmpeg("-i", src, "-af", fmt.Sprintf("pan=mono|c0=c%d", *channel), "-ar", "16000", "-c:a", "pcm_s16le", dst); err != nil {
			return fmt.Errorf("%s：%w", src, err)
		}
		log.Printf("%s：%d 句", base, len(lines))
	}
	return nil
}

type interval struct {
	start, end float64
	text       string
}

var (
	numberLine = regexp.MustCompile(`^(xmin|xmax) = ([0-9.eE+-]+)$`)
	textLine   = regexp.MustCompile(`^text = "(.*)"$`)
)

// parseTextGrid 读 Praat 的长格式 TextGrid，返回所有层里的所有区间（没有文字的也在内）。
// 文字里的 "" 是转义的双引号。
func parseTextGrid(path string) ([]interval, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []interval
	var cur *interval
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\uFEFF"))
		switch {
		case strings.HasPrefix(line, "intervals ["):
			out = append(out, interval{})
			cur = &out[len(out)-1]
		case strings.HasPrefix(line, "item ["):
			cur = nil
		case cur == nil:
		case numberLine.MatchString(line):
			m := numberLine.FindStringSubmatch(line)
			v, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				return nil, err
			}
			if m[1] == "xmin" {
				cur.start = v
			} else {
				cur.end = v
			}
		case textLine.MatchString(line):
			cur.text = strings.ReplaceAll(textLine.FindStringSubmatch(line)[1], `""`, `"`)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("没有读出任何区间，不是长格式的 TextGrid？")
	}
	return out, nil
}

// 标注里表示噪音、笑声、听不清等的记号（<%>、<$>、<sil>、[laughter] 之类），不是说的话
var marks = regexp.MustCompile(`<[^>]*>|\[[^\]]*\]`)

func cleanText(s string) string {
	return strings.TrimSpace(marks.ReplaceAllString(s, ""))
}

func ffmpeg(args ...string) error {
	cmd := exec.Command("ffmpeg", append([]string{"-nostdin", "-loglevel", "error", "-y"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg：%v：%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func contextASRCmd(args []string) error {
	fs := flag.NewFlagSet("contextasr", flag.ExitOnError)
	jsonl := fs.String("jsonl", "", "ContextASR-Bench 的 jsonl（比如 ContextASR-Dialogue_Mandarin.jsonl）")
	audioDir := fs.String("audio", "", "解压出的录音目录（录音在 Hugging Face 上按 tar 分包放，下载一两个分包解压就够）")
	n := fs.Int("n", 60, "挑多少条")
	out := fs.String("out", "", "输出目录")
	fs.Parse(args)
	if *jsonl == "" || *audioDir == "" || *out == "" {
		fs.Usage()
		os.Exit(2)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	// 解压出的录音可能在几层子目录里，按文件名找
	wavs := map[string]string{}
	err := filepath.WalkDir(*audioDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".wav") {
			wavs[strings.TrimSuffix(d.Name(), ".wav")] = path
		}
		return err
	})
	if err != nil {
		return err
	}

	type item struct {
		ID       string   `json:"uniq_id"`
		Text     string   `json:"text"`
		Entities []string `json:"entity_list"`
	}
	f, err := os.Open(*jsonl)
	if err != nil {
		return err
	}
	defer f.Close()
	var items []item
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 16<<20), 16<<20)
	for sc.Scan() {
		var it item
		if err := json.Unmarshal(sc.Bytes(), &it); err != nil {
			return err
		}
		// 只要录音已经下载了的
		if _, ok := wavs[it.ID]; ok {
			items = append(items, it)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("%s 里没有 jsonl 里列出的录音", *audioDir)
	}
	// 按编号排好、均匀地挑，每次挑出的都一样
	slices.SortFunc(items, func(a, b item) int { return cmp.Compare(a.ID, b.ID) })
	step := max(len(items)/max(*n, 1), 1)
	picked := 0
	for i := 0; i < len(items) && picked < *n; i += step {
		it := items[i]
		if err := ffmpeg("-i", wavs[it.ID], "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", filepath.Join(*out, it.ID+".wav")); err != nil {
			return fmt.Errorf("%s：%w", it.ID, err)
		}
		if err := os.WriteFile(filepath.Join(*out, it.ID+".txt"), []byte(it.Text+"\n"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*out, it.ID+".hotwords.txt"), []byte(strings.Join(it.Entities, "\n")+"\n"), 0o644); err != nil {
			return err
		}
		picked++
		log.Printf("%d/%d %s：%d 个专有名词", picked, *n, it.ID, len(it.Entities))
	}
	log.Printf("从已下载的 %d 条里挑了 %d 条", len(items), picked)
	return nil
}
