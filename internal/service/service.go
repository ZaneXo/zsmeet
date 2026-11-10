// Package service 是业务逻辑层：账号、会议、纪要版本、团队设置。
// 这一层不认识 HTTP，接口层（internal/api）只负责取参数、调这里、把结果转成响应。
package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/storage"
)

type Service struct {
	pool     *pgxpool.Pool
	q        *sqlc.Queries
	store    *storage.Local
	settings *settings.Store
	queue    *queue.Client
}

func New(pool *pgxpool.Pool, store *storage.Local, st *settings.Store, qc *queue.Client) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), store: store, settings: st, queue: qc}
}

// tx 在一个事务里执行 fn，fn 返回错误就回滚。
func (s *Service) tx(ctx context.Context, fn func(tx pgx.Tx, q *sqlc.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(tx, s.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Kind 是业务错误的种类，接口层按它决定 HTTP 状态码。
type Kind int

const (
	NotFound Kind = iota + 1
	Invalid
	Conflict
	Forbidden
	Unauthorized
)

// Error 是可以原样展示给用户的业务错误。其他类型的 error 都算内部错误。
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func notFound(msg string) error { return &Error{NotFound, msg} }

func invalid(format string, a ...any) error { return &Error{Invalid, fmt.Sprintf(format, a...)} }

func conflict(format string, a ...any) error { return &Error{Conflict, fmt.Sprintf(format, a...)} }
