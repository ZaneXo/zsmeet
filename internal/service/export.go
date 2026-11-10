package service

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
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
// 能看这场会议的人都能导出。
func (s *Service) Export(ctx context.Context, user sqlc.User, id uuid.UUID, what, format string) (ExportFile, error) {
	m, err := s.GetMeeting(ctx, user, id)
	if err != nil {
		return ExportFile{}, err
	}
	names := transcript.ParseNames(m.Speakers)
	info := export.Info(m.CreatedAt, int(m.DurationSec), m.OwnerName, names)

	var markdown, suffix string
	switch what {
	case "minutes":
		if m.CurrentVersion == 0 {
			return ExportFile{}, conflict("这场会议还没有纪要，等生成以后再导出")
		}
		minutes, err := s.GetMinutes(ctx, user, id, 0)
		if err != nil {
			return ExportFile{}, err
		}
		info = append(info, fmt.Sprintf("纪要版本：第 %d 版", minutes.Version))
		markdown, suffix = export.MinutesMarkdown(m.Title, info, minutes.Content), "纪要"
		if format == "txt" {
			format = "md" // 纪要本来就是 Markdown 文本
		}
	case "transcript":
		segs, err := s.Transcript(ctx, user, id)
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
