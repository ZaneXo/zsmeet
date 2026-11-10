// Package testdb 给需要真实 Postgres 的测试准备数据库。
//
// 这些测试默认跳过。用 make test 会自动起一个测试用的 Postgres；
// 也可以自己设置 ZSMEET_TEST_DATABASE_URL 指向任意一个能建库的 Postgres。
package testdb

import (
	"context"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZaneXo/zsmeet/internal/db"
)

// 每个测试包用自己的数据库（go test 会并行跑不同的包），同一个包里只迁移一次
var migrated sync.Map

// Open 返回一个表结构最新、数据清空的数据库。name 用来区分测试包。
// 同一个包里的测试共用这个库，所以不要并行（不要调 t.Parallel）。
func Open(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("ZSMEET_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("没有设置 ZSMEET_TEST_DATABASE_URL，跳过需要数据库的测试（用 make test 会自动准备）")
	}
	// 不用 t.Context()：它在清理函数运行前就取消了，而连接池要活到测试结束
	ctx := context.Background()

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("ZSMEET_TEST_DATABASE_URL 不是合法的地址：%v", err)
	}
	dbName := "zsmeet_test_" + name
	if _, done := migrated.Load(dbName); !done {
		conn, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Fatalf("连不上测试数据库：%v", err)
		}
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", dbName).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()); err != nil {
				t.Fatal(err)
			}
		}
		conn.Close(ctx)
	}

	u.Path = "/" + dbName
	pool, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, done := migrated.LoadOrStore(dbName, true); !done {
		if err := db.Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, "TRUNCATE users, sessions, settings, meetings, river_job, login_failures CASCADE"); err != nil {
		t.Fatal(err)
	}
	return pool
}
