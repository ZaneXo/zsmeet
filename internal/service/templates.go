package service

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/minutes"
)

// 自定义模板的名字、写法说明最长多少字
const (
	maxTemplateName  = 30
	maxTemplateGuide = 2000
)

// Template 是一种纪要写法：内置的，或者管理员自己定义的。
type Template struct {
	minutes.Template
	Custom bool
}

// Templates 列出所有能用的纪要模板：先是内置的四种，再是自定义的（按创建时间）。
func (s *Service) Templates(ctx context.Context) ([]Template, error) {
	out := make([]Template, 0, len(minutes.Templates))
	for _, t := range minutes.Templates {
		out = append(out, Template{Template: t})
	}
	rows, err := s.q.ListCustomTemplates(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, Template{Template: minutes.Template{ID: r.ID.String(), Name: r.Name, Guide: r.Guide}, Custom: true})
	}
	return out, nil
}

// FindTemplate 按 ID 找纪要模板：内置的直接找，否则当成自定义模板的 ID 去数据库里找。找不到时 ok 为 false。
// 后台写纪要时也用它，所以收的是 Queries 而不是 Service。
func FindTemplate(ctx context.Context, q *sqlc.Queries, id string) (minutes.Template, bool, error) {
	if t, ok := minutes.Find(id); ok {
		return t, true, nil
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return minutes.Template{}, false, nil
	}
	r, err := q.GetCustomTemplate(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return minutes.Template{}, false, nil
	}
	if err != nil {
		return minutes.Template{}, false, err
	}
	return minutes.Template{ID: r.ID.String(), Name: r.Name, Guide: r.Guide}, true, nil
}

// checkTemplate 确认模板存在，不存在时返回「没有这个纪要模板」。
func (s *Service) checkTemplate(ctx context.Context, id string) error {
	_, ok, err := FindTemplate(ctx, s.q, id)
	if err != nil {
		return err
	}
	if !ok {
		return invalid("没有这个纪要模板，可能已经被管理员删掉了")
	}
	return nil
}

func cleanTemplate(name, guide string) (string, string, error) {
	name, guide = strings.TrimSpace(name), strings.TrimSpace(guide)
	switch {
	case name == "":
		return "", "", invalid("给模板起个名字")
	case utf8.RuneCountInString(name) > maxTemplateName:
		return "", "", invalid("模板名字最长 %d 个字", maxTemplateName)
	case guide == "":
		return "", "", invalid("写一下纪要要有哪几部分、每部分写什么")
	case utf8.RuneCountInString(guide) > maxTemplateGuide:
		return "", "", invalid("写法说明最长 %d 个字", maxTemplateGuide)
	}
	return name, guide, nil
}

// CreateTemplate 新建一个自定义纪要模板。只有管理员能调（接口层检查）。
func (s *Service) CreateTemplate(ctx context.Context, user sqlc.User, name, guide string) (Template, error) {
	name, guide, err := cleanTemplate(name, guide)
	if err != nil {
		return Template{}, err
	}
	r, err := s.q.CreateCustomTemplate(ctx, sqlc.CreateCustomTemplateParams{Name: name, Guide: guide, CreatedBy: &user.ID})
	if err != nil {
		return Template{}, err
	}
	return Template{Template: minutes.Template{ID: r.ID.String(), Name: r.Name, Guide: r.Guide}, Custom: true}, nil
}

var errNoTemplate = notFound("没有这个自定义模板，可能已经被删掉了（内置的模板不能改）")

// UpdateTemplate 改一个自定义纪要模板。以后用它写的纪要按新的写法，已经写好的不变。
func (s *Service) UpdateTemplate(ctx context.Context, id uuid.UUID, name, guide string) (Template, error) {
	name, guide, err := cleanTemplate(name, guide)
	if err != nil {
		return Template{}, err
	}
	r, err := s.q.UpdateCustomTemplate(ctx, sqlc.UpdateCustomTemplateParams{ID: id, Name: name, Guide: guide})
	if errors.Is(err, pgx.ErrNoRows) {
		return Template{}, errNoTemplate
	}
	if err != nil {
		return Template{}, err
	}
	return Template{Template: minutes.Template{ID: r.ID.String(), Name: r.Name, Guide: r.Guide}, Custom: true}, nil
}

// DeleteTemplate 删掉一个自定义纪要模板。已经用它写好的纪要不受影响；还选着它的会议以后重新生成时用通用模板。
func (s *Service) DeleteTemplate(ctx context.Context, id uuid.UUID) error {
	n, err := s.q.DeleteCustomTemplate(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return errNoTemplate
	}
	return nil
}
