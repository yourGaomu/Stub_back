package links

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/yourGaomu/short_links/back/internal/config"
)

func TestPostgresConcurrentAccess(t *testing.T) {
	// 仅显式测试连接可启用；不加载 .env，不读取 DATABASE_URL，不启动 API。
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过真实 PostgreSQL 并发测试")
	}
	connectionConfig, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("TEST_DATABASE_URL 格式无效")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	adminDB := stdlib.OpenDB(*connectionConfig)
	defer adminDB.Close()
	schema := "link_access_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("无法创建独立测试 schema: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := adminDB.ExecContext(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("清理测试 schema 失败: %v", err)
		}
	}()
	connectionConfig.RuntimeParams["search_path"] = schema
	connectionConfig.RuntimeParams["statement_timeout"] = "10000"
	connectionConfig.RuntimeParams["lock_timeout"] = "5000"
	db := stdlib.OpenDB(*connectionConfig)
	defer db.Close()
	db.SetMaxOpenConns(12)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS("../../platform/postgres/migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	userID, workspaceID := uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users(user_id,email,password_hash,role,email_verified_at) VALUES($1,'concurrent@example.com','unused','admin',now())`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workspaces(workspace_id,owner_user_id) VALUES($1,$2)`, workspaceID, userID); err != nil {
		t.Fatal(err)
	}
	s := NewService(db, config.Config{}, nil)
	for _, tc := range []struct {
		name        string
		maxVisits   int64
		cidrs       string
		wantAllowed int
	}{
		{"one_remaining", 1, "{}", 1},
		{"ip_denied_preserves_visit", 1, "{10.0.0.0/8}", 0},
		{"zero_limit", 0, "{}", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			linkID := uuid.New()
			code := "concurrent-" + tc.name
			if _, err := db.ExecContext(ctx, `INSERT INTO short_links(link_id,workspace_id,target_type,target_url,max_visits,allowed_ip_cidrs,status,created_by) VALUES($1,$2,'url','https://example.com',$3,$4::cidr[],'enabled',$5)`, linkID, workspaceID, tc.maxVisits, tc.cidrs, userID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO short_link_codes(code,link_id,status) VALUES($1,$2,'active')`, code, linkID); err != nil {
				t.Fatal(err)
			}
			const attempts = 12
			start := make(chan struct{})
			errors := make(chan error, attempts)
			results := make(chan PublicResult, attempts)
			var workers sync.WaitGroup
			for range attempts {
				workers.Go(func() {
					<-start
					result, err := s.Access(ctx, code, RequestMeta{SourceIP: "192.0.2.10"}, "", false)
					if err != nil {
						errors <- err
						return
					}
					results <- result
				})
			}
			close(start)
			workers.Wait()
			close(errors)
			close(results)
			for err := range errors {
				t.Error(err)
			}
			allowed := 0
			for result := range results {
				if result.StatusCode == 302 {
					allowed++
				} else if result.StatusCode != 403 || result.Location != "" {
					t.Errorf("unexpected access result: %+v", result)
				}
			}
			var visits, logs, consumed, deniedConsumed int
			if err := db.QueryRowContext(ctx, `SELECT visits_used FROM short_links WHERE link_id=$1`, linkID).Scan(&visits); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE consumes_visit),count(*) FILTER(WHERE result='denied' AND consumes_visit) FROM short_link_access_logs WHERE link_id=$1`, linkID).Scan(&logs, &consumed, &deniedConsumed); err != nil {
				t.Fatal(err)
			}
			if allowed != tc.wantAllowed || visits != tc.wantAllowed || logs != attempts || consumed != tc.wantAllowed || deniedConsumed != 0 {
				t.Fatalf("allowed=%d visits=%d logs=%d consumes=%d deniedConsumes=%d; want allowed/visits/consumes=%d logs=%d", allowed, visits, logs, consumed, deniedConsumed, tc.wantAllowed, attempts)
			}
		})
	}
}
