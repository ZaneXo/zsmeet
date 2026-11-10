package service

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/export"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

// ExportFile 是导出的一个文件。
type ExportFile struct {
	Name        string
	ContentType string
	Data        []byte
}

const docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// Export 导出纪要（当前版本）或转写。what 是 minutes / transcript，format 是 md / docx / txt（txt 只用于转写）。
// 团队里的人都能看全部会议，所以也都能导出。
func (s *Service) Export(ctx context.Context, id uuid.UUID, what, format string) (ExportFile, error) {
	m, err := s.GetMeeting(ctx, id)
	if err != nil {
		return ExportFile{}, err
	}
	names := transcript.ParseNames(m.Speakers)
	info := exportInfo(m, names)

	var markdown, suffix string
	switch what {
	case "minutes":
		if m.CurrentVersion == 0 {
			return ExportFile{}, conflict("这场会议还没有纪要，等生成以后再导出")
		}
		minutes, err := s.GetMinutes(ctx, id, 0)
		if err != nil {
			return ExportFile{}, err
		}
		info = append(info, fmt.Sprintf("纪要版本：第 %d 版", minutes.Version))
		markdown, suffix = export.MinutesMarkdown(m.Title, info, minutes.Content), "纪要"
		if format == "txt" {
			format = "md" // 纪要本来就是 Markdown 文本
		}
	case "transcript":
		segs, err := s.Transcript(ctx, id)
		if err != nil {
			return ExportFile{}, err
		}
		if len(segs) == 0 {
			return ExportFile{}, conflict("这场会议还没有转写，等识别完以后再导出")
		}
		suffix = "转写"
		if format == "txt" {
			return ExportFile{
				Name:        fileName(m.Title, suffix, "txt"),
				ContentType: "text/plain; charset=utf-8",
				Data:        []byte(export.TranscriptText(m.Title, info, segs, names)),
			}, nil
		}
		markdown = export.TranscriptMarkdown(m.Title+"（转写）", info, segs, names)
	default:
		return ExportFile{}, invalid("不认识的导出内容：%s", what)
	}

	switch format {
	case "md":
		return ExportFile{Name: fileName(m.Title, suffix, "md"), ContentType: "text/markdown; charset=utf-8", Data: []byte(markdown)}, nil
	case "docx":
		data, err := export.Docx(markdown)
		if err != nil {
			return ExportFile{}, err
		}
		return ExportFile{Name: fileName(m.Title, suffix, "docx"), ContentType: docxType, Data: data}, nil
	}
	return ExportFile{}, invalid("不认识的导出格式：%s", format)
}

// exportInfo 是导出文件开头的几行会议信息。
func exportInfo(m Meeting, names transcript.Names) []string {
	info := []string{"时间：" + m.CreatedAt.Local().Format("2006年1月2日 15:04")}
	if m.DurationSec > 0 {
		info = append(info, "时长："+humanDuration(int(m.DurationSec)))
	}
	info = append(info, "创建者："+m.OwnerName)
	if len(names) > 0 {
		var list []string
		for idx := range 100 {
			if name := names[idx]; name != "" {
				list = append(list, name)
			}
		}
		info = append(info, "说话人："+strings.Join(list, "、"))
	}
	return info
}

func humanDuration(sec int) string {
	switch {
	case sec >= 3600:
		return fmt.Sprintf("%d 小时 %d 分钟", sec/3600, sec%3600/60)
	case sec >= 60:
		return fmt.Sprintf("%d 分钟", sec/60)
	default:
		return fmt.Sprintf("%d 秒", sec)
	}
}

// fileName 用会议标题起文件名：去掉文件名里不能用的字符，太长的截短。
func fileName(title, suffix, ext string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`\/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(title))
	if utf8.RuneCountInString(clean) > 60 {
		clean = string([]rune(clean)[:60])
	}
	if clean == "" {
		clean = "会议"
	}
	return clean + "-" + suffix + "." + ext
}
