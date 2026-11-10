package api

import (
	"context"
	"net/http"
	"net/url"
	"path"

	"github.com/danielgtaylor/huma/v2"
)

type ExportInput struct {
	MeetingID
	What   string `query:"what" enum:"minutes,transcript" default:"minutes" doc:"导出纪要（当前版本）还是转写"`
	Format string `query:"format" enum:"docx,md,txt" default:"docx" doc:"Word、Markdown 或纯文本（纯文本只用于转写，纪要选它时给 Markdown）"`
}

type ExportOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition"`
	Body               []byte
}

var exportOp = huma.Operation{
	OperationID: "export-meeting",
	Method:      http.MethodGet,
	Path:        "/api/meetings/{id}/export",
	Summary:     "导出纪要或转写",
	Description: "返回文件本身（作为下载）。开头是会议信息：时间、时长、创建者、说话人。还没有纪要或转写时返回 409。",
	Tags:        []string{"会议"},
	Security:    loggedIn,
}

func (h *Handlers) Export(ctx context.Context, in *ExportInput) (*ExportOutput, error) {
	id, err := parseID(in.ID)
	if err != nil {
		return nil, err
	}
	file, err := h.svc.Export(ctx, currentUser(ctx), id, in.What, in.Format)
	if err != nil {
		return nil, toHTTP(err)
	}
	return &ExportOutput{
		ContentType: file.ContentType,
		// 中文文件名放在 filename* 里（RFC 6266），老浏览器退回到 filename 的英文名
		ContentDisposition: `attachment; filename="zsmeet-export` + path.Ext(file.Name) + `"; filename*=UTF-8''` + url.PathEscape(file.Name),
		Body:               file.Data,
	}, nil
}
