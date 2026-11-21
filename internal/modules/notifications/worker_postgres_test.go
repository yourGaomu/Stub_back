package notifications

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func TestWorkerPostgresShortlistBoundaries(t *testing.T) {
	f := newWorkerPostgresFixture(t)
	owner := f.addRecipient(t)
	at := workerFixtureTime.UTC()
	past, next := at.Add(-time.Microsecond), at.Add(time.Microsecond)
	soon, upper := at.Add(time.Hour), at.Add(24*time.Hour)
	late := upper.Add(time.Microsecond)
	const largeThreshold = 7_378_697_629_483_820_646 // ceil(4/5 * MaxInt64)

	var want []workerPostgresNotice
	var first workerPostgresNotice
	for i, tc := range []struct {
		link       workerPostgresLink
		wantCutoff *time.Time
		wantVisits bool
	}{
		{link: workerPostgresLink{until: &upper}, wantCutoff: &upper},
		{link: workerPostgresLink{expires: &late, until: &soon}, wantCutoff: &soon},
		{link: workerPostgresLink{expires: &soon, until: &late}, wantCutoff: &soon},
		{link: workerPostgresLink{expires: &next}, wantCutoff: &next},
		{link: workerPostgresLink{expires: &at}},
		{link: workerPostgresLink{expires: &soon, until: &past}},
		{link: workerPostgresLink{expires: &late}},
		{link: workerPostgresLink{expires: &soon, maxVisits: 5, used: 4, disabled: true}},
		{link: workerPostgresLink{maxVisits: -1, used: 100}},
		{link: workerPostgresLink{maxVisits: 0}},
		{link: workerPostgresLink{maxVisits: 1, used: 0}},
		{link: workerPostgresLink{maxVisits: 1, used: 1}, wantVisits: true},
		{link: workerPostgresLink{maxVisits: 5, used: 3}},
		{link: workerPostgresLink{maxVisits: 5, used: 4}, wantVisits: true},
		{link: workerPostgresLink{maxVisits: math.MaxInt64, used: largeThreshold - 1}},
		{link: workerPostgresLink{maxVisits: math.MaxInt64, used: largeThreshold}, wantVisits: true},
	} {
		// Ascending IDs ensure UUID order alone would not prioritize the earliest cutoff.
		tc.link.id = uuid.UUID{15: byte(i + 1)}
		link := f.addLink(t, owner, tc.link)
		if tc.wantCutoff != nil {
			notice := owner.expiry(link, *tc.wantCutoff)
			want = append(want, notice)
			if tc.wantCutoff.Equal(next) {
				first = notice
			}
		}
		if tc.wantVisits {
			want = append(want, owner.visits(link, 0))
		}
	}

	f.run(t, at, 1)
	f.assertNotices(t, first)
	// A limit of one can still advance past already-notified candidates.
	for range len(want) - 1 {
		f.run(t, at, 1)
	}
	f.assertNotices(t, want...)
	f.run(t, at, 32) // No out-of-bound candidate should be left to insert.
	f.assertNotices(t, want...)
}

func TestWorkerPostgresConcurrentDeduplication(t *testing.T) {
	f := newWorkerPostgresFixture(t)
	owner := f.addRecipient(t)
	at := workerFixtureTime.UTC()
	cutoff := at.Add(6*time.Hour + 120*time.Microsecond)
	expires := at.Add(48 * time.Hour)
	link := f.addLink(t, owner, workerPostgresLink{expires: &expires, until: &cutoff, maxVisits: 5, used: 4})
	want := []workerPostgresNotice{owner.expiry(link, cutoff), owner.visits(link, 0)}

	f.concurrentScans(t, at)
	f.assertNotices(t, want...)

	// A change to the later bound or further consumption in the same cycle is not a new event.
	f.exec(t, `UPDATE short_links SET expires_at=$2,visits_used=5 WHERE link_id=$1`, link, expires.Add(time.Hour))
	f.concurrentScans(t, at.Add(time.Minute))
	f.assertNotices(t, want...)

	// Distinct effective cutoffs within the same second must not collapse to one key.
	newCutoff := cutoff.Add(time.Microsecond)
	f.exec(t, `UPDATE short_links SET active_until=$2,visits_used=0,visits_cycle=visits_cycle+1 WHERE link_id=$1`, link, newCutoff)
	f.concurrentScans(t, at.Add(2*time.Minute))
	want = append(want, owner.expiry(link, newCutoff))
	f.assertNotices(t, want...)

	f.exec(t, `UPDATE short_links SET visits_used=4 WHERE link_id=$1`, link)
	f.concurrentScans(t, at.Add(3*time.Minute))
	f.assertNotices(t, append(want, owner.visits(link, 1))...)
}

func TestWorkerPostgresSkipsPassedCutoffAndConcurrentLinkUpdate(t *testing.T) {
	f := newWorkerPostgresFixture(t)
	owner := f.addRecipient(t)
	at := workerFixtureTime.UTC()
	cutoff := at.Add(time.Second)
	f.addLink(t, owner, workerPostgresLink{expires: &cutoff})
	clockCalls := 0
	if err := f.service.runOnce(f.ctx, func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return at
		}
		return cutoff
	}, 1); err != nil {
		t.Fatal(err)
	}
	if clockCalls != 2 {
		t.Fatalf("clock called %d times, want shortlist before cutoff and locked recheck at cutoff", clockCalls)
	}
	f.assertNotices(t)

	at = cutoff.Add(time.Second)
	future := at.Add(time.Hour)
	link := f.addLink(t, owner, workerPostgresLink{expires: &future})
	candidate := workerCandidate{linkID: link, recipientID: owner.userID, kind: KindExpiry24h}
	f.requireCandidate(t, at, candidate)
	update := f.begin(t)
	if _, err := update.ExecContext(f.ctx, `UPDATE short_links SET active_until=$2 WHERE link_id=$1`, link, at); err != nil {
		t.Fatal(err)
	}
	// The shortlist still sees the old committed deadline. The row lock must
	// skip rather than block or insert based on that state.
	f.requireCandidate(t, at, candidate)
	f.run(t, at, 1)
	f.assertNotices(t)
	if err := update.Commit(); err != nil {
		t.Fatal(err)
	}
	if created, err := f.service.createReminder(f.ctx, func() time.Time { return at }, candidate); err != nil || created {
		t.Fatalf("stale candidate: created=%t, err=%v", created, err)
	}
	f.run(t, at, 1)
	f.assertNotices(t)
}

func TestWorkerPostgresContinuesAfterLockedCandidate(t *testing.T) {
	f := newWorkerPostgresFixture(t)
	at := workerFixtureTime.UTC()
	first, second := f.addRecipient(t), f.addRecipient(t)
	firstDeadline, secondDeadline := at.Add(time.Hour), at.Add(2*time.Hour)
	firstLink := f.addLink(t, first, workerPostgresLink{expires: &firstDeadline})
	secondLink := f.addLink(t, second, workerPostgresLink{expires: &secondDeadline})
	writer := f.begin(t)
	if _, err := writer.ExecContext(f.ctx, `SELECT link_id FROM short_links WHERE link_id=$1 FOR UPDATE`, firstLink); err != nil {
		t.Fatal(err)
	}
	f.run(t, at, 1)
	f.assertNotices(t, second.expiry(secondLink, secondDeadline))
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	f.run(t, at, 1)
	f.assertNotices(t, first.expiry(firstLink, firstDeadline), second.expiry(secondLink, secondDeadline))
}

func TestWorkerPostgresSettingsUpdateAndRecipientIsolation(t *testing.T) {
	f := newWorkerPostgresFixture(t)
	at := workerFixtureTime.UTC()
	cutoff := at.Add(time.Hour)
	a, b := f.addRecipient(t), f.addRecipient(t)
	link := workerPostgresLink{until: &cutoff, maxVisits: 5, used: 4}
	aLink, bLink := f.addLink(t, a, link), f.addLink(t, b, link)
	candidate := workerCandidate{linkID: aLink, recipientID: a.userID, kind: KindExpiry24h}
	f.requireCandidate(t, at, candidate)

	// Model the first settings write for A while it is still uncommitted.
	// The shortlist sees A's old defaults, but B should get both reminders.
	writer := f.begin(t)
	if _, err := writer.ExecContext(f.ctx, `SELECT pg_advisory_xact_lock(`+recipientLockKey+`)`, a.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(f.ctx, `INSERT INTO notification_settings(user_id,expiry_enabled) VALUES($1,false)`, a.userID); err != nil {
		t.Fatal(err)
	}
	f.requireCandidate(t, at, candidate)
	f.run(t, at, 8)
	bExpiry, bVisits := b.expiry(bLink, cutoff), b.visits(bLink, 0)
	f.assertNotices(t, bExpiry, bVisits)
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}

	if created, err := f.service.createReminder(f.ctx, func() time.Time { return at }, candidate); err != nil || created {
		t.Fatalf("stale candidate: created=%t, err=%v", created, err)
	}
	f.run(t, at, 8)
	aVisits := a.visits(aLink, 0)
	f.assertNotices(t, bExpiry, bVisits, aVisits)

	expiryEnabled := true
	if _, err := f.service.UpdateSettings(f.ctx, a.userID, UpdateSettingsRequest{ExpiryEnabled: &expiryEnabled}); err != nil {
		t.Fatal(err)
	}
	f.run(t, at, 8)
	f.assertNotices(t, bExpiry, bVisits, aVisits, a.expiry(aLink, cutoff))
}

type workerPostgresFixture struct {
	ctx     context.Context
	db      *sql.DB
	service *Service
}

func newWorkerPostgresFixture(t *testing.T) workerPostgresFixture {
	t.Helper()
	// Only use the dedicated test connection; never load .env or DATABASE_URL.
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL worker test")
	}
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("invalid TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	schema := "notification_worker_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})

	cfg = cfg.Copy()
	cfg.RuntimeParams["search_path"] = schema
	cfg.RuntimeParams["statement_timeout"] = "10000"
	cfg.RuntimeParams["lock_timeout"] = "3000"
	cfg.RuntimeParams["timezone"] = "Asia/Shanghai" // Dedupe keys still use UTC.
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(10)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS("../../platform/postgres/migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	return workerPostgresFixture{ctx: ctx, db: db, service: NewService(db)}
}

type workerPostgresRecipient struct{ userID, workspaceID uuid.UUID }

type workerPostgresLink struct {
	id              uuid.UUID
	expires, until  *time.Time
	maxVisits, used int64
	disabled        bool
}

type workerPostgresNotice struct {
	owner     workerPostgresRecipient
	linkID    uuid.UUID
	kind, key string
}

func (owner workerPostgresRecipient) expiry(link uuid.UUID, cutoff time.Time) workerPostgresNotice {
	return workerPostgresNotice{owner, link, KindExpiry24h, "expiry:" + link.String() + ":" + cutoff.UTC().Format(time.RFC3339Nano)}
}

func (owner workerPostgresRecipient) visits(link uuid.UUID, cycle int) workerPostgresNotice {
	return workerPostgresNotice{owner, link, KindVisits80, fmt.Sprintf("visits:%s:%d", link, cycle)}
}

func (f workerPostgresFixture) addRecipient(t *testing.T) workerPostgresRecipient {
	t.Helper()
	owner := workerPostgresRecipient{uuid.New(), uuid.New()}
	f.exec(t, `INSERT INTO users(user_id,email,password_hash,role,email_verified_at) VALUES($1,$2,'unused','user',now())`,
		owner.userID, owner.userID.String()+"@example.com")
	f.exec(t, `INSERT INTO workspaces(workspace_id,owner_user_id) VALUES($1,$2)`, owner.workspaceID, owner.userID)
	return owner
}

func (f workerPostgresFixture) addLink(t *testing.T, owner workerPostgresRecipient, link workerPostgresLink) uuid.UUID {
	t.Helper()
	if link.id == uuid.Nil {
		link.id = uuid.New()
	}
	status := "enabled"
	if link.disabled {
		status = "disabled"
	}
	f.exec(t, `INSERT INTO short_links(link_id,workspace_id,target_type,target_url,created_by,status,expires_at,active_until,max_visits,visits_used)
		VALUES($1,$2,'url','https://example.com',$3,$4,$5,$6,$7,$8)`,
		link.id, owner.workspaceID, owner.userID, status, link.expires, link.until, link.maxVisits, link.used)
	return link.id
}

func (f workerPostgresFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.db.ExecContext(f.ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f workerPostgresFixture) begin(t *testing.T) *sql.Tx {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func (f workerPostgresFixture) run(t *testing.T, at time.Time, limit int) {
	t.Helper()
	if err := f.service.runOnce(f.ctx, func() time.Time { return at }, limit); err != nil {
		t.Fatal(err)
	}
}

func (f workerPostgresFixture) requireCandidate(t *testing.T, at time.Time, want workerCandidate) {
	t.Helper()
	candidates, err := f.service.shortlist(f.ctx, at, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate == want {
			return
		}
	}
	t.Fatalf("shortlist = %+v, want candidate %+v", candidates, want)
}

func (f workerPostgresFixture) assertNotices(t *testing.T, want ...workerPostgresNotice) {
	t.Helper()
	rows, err := f.db.QueryContext(f.ctx, `SELECT recipient_user_id,workspace_id,link_id,kind,dedupe_key FROM notifications`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make(map[workerPostgresNotice]int)
	for rows.Next() {
		var notice workerPostgresNotice
		if err := rows.Scan(&notice.owner.userID, &notice.owner.workspaceID, &notice.linkID, &notice.kind, &notice.key); err != nil {
			t.Fatal(err)
		}
		got[notice]++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	expected := make(map[workerPostgresNotice]int, len(want))
	for _, notice := range want {
		expected[notice]++
	}
	if !maps.Equal(got, expected) {
		t.Fatalf("notifications = %+v, want %+v", got, expected)
	}
}

func (f workerPostgresFixture) concurrentScans(t *testing.T, at time.Time) {
	t.Helper()
	const workers, scans = 8, 3
	start := make(chan struct{})
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for i := range workers {
		group.Go(func() {
			s := NewService(f.db)
			<-start
			for j := range scans {
				// Cutoff and cycle are unchanged across distinct scan times.
				scanAt := at.Add(time.Duration(i*scans+j) * time.Second)
				if err := s.runOnce(f.ctx, func() time.Time { return scanAt }, 8); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
