package notifications

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

const (
	workerRecipientLockSQL = `SELECT pg_try_advisory_xact_lock(hashtextextended('notification-settings:' || $1::uuid::text, 0))`
	workerLinkLockSQL      = `SELECT link_id FROM short_links WHERE link_id=$1 AND created_by=$2 FOR SHARE SKIP LOCKED`
	workerInsertSQL        = `INSERT INTO notifications(notification_id,recipient_user_id,workspace_id,link_id,kind,title,summary,dedupe_key,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(dedupe_key) DO NOTHING`
)

var workerFixtureTime = time.Date(2026, 11, 21, 12, 30, 0, 0, time.FixedZone("UTC-05", -5*60*60))

func newWorkerTestService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		_ = db.Close()
	})
	return NewService(db), mock
}

func workerSQL(parts ...string) string {
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	return "^" + strings.Join(parts, ".*") + "$"
}

// These expectations are independent of the production query constants and
// pin the eligibility and dedupe clauses on both statements.
func workerEligibilitySQL(selectSQL, suffix string) string {
	return workerSQL(selectSQL,
		`LEFT JOIN notification_settings ns ON ns.user_id=sl.created_by`,
		`LEAST(sl.expires_at,sl.active_until)`,
		`'expiry:'||sl.link_id::text||':'||`,
		`to_char(d.deadline AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US')`,
		`COALESCE(ns.expiry_enabled,true) AND d.deadline>$1 AND d.deadline<=$2`,
		`'visits:'||sl.link_id::text||':'||sl.visits_cycle::text`,
		`COALESCE(ns.visits_enabled,true) AND sl.max_visits>0`,
		`sl.visits_used>=sl.max_visits-sl.max_visits/5`,
		`sl.status='enabled' AND NOT EXISTS ( SELECT 1 FROM notifications n WHERE n.dedupe_key=reminder.dedupe_key )`,
		suffix)
}

func expectWorkerShortlist(mock sqlmock.Sqlmock, at time.Time, limit int) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(workerEligibilitySQL(`SELECT sl.link_id,sl.created_by,reminder.kind`,
		`ORDER BY CASE WHEN reminder.kind='expiry_24h' THEN d.deadline END NULLS LAST,sl.link_id,reminder.kind LIMIT $3`)).
		WithArgs(at.UTC(), at.UTC().Add(24*time.Hour), limit)
}

func workerShortlistRows(candidates ...workerCandidate) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"link_id", "created_by", "kind"})
	for _, c := range candidates {
		rows.AddRow(c.linkID, c.recipientID, c.kind)
	}
	return rows
}

func expectWorkerRecipientLock(mock sqlmock.Sqlmock, c workerCandidate) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(workerSQL(workerRecipientLockSQL)).WithArgs(c.recipientID)
}

func expectWorkerLinkLock(mock sqlmock.Sqlmock, c workerCandidate) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(workerSQL(workerLinkLockSQL)).WithArgs(c.linkID, c.recipientID)
}

func expectWorkerLocks(mock sqlmock.Sqlmock, c workerCandidate) {
	expectWorkerRecipientLock(mock, c).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	expectWorkerLinkLock(mock, c).WillReturnRows(sqlmock.NewRows([]string{"link_id"}).AddRow(c.linkID))
}

func expectWorkerRecheck(mock sqlmock.Sqlmock, at time.Time, c workerCandidate) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(workerEligibilitySQL(
		`SELECT sl.workspace_id,d.deadline,sl.max_visits,sl.visits_used,reminder.dedupe_key, (SELECT sc.code FROM short_link_codes sc WHERE sc.link_id=sl.link_id AND sc.status='active')`,
		`AND sl.link_id=$3 AND sl.created_by=$4 AND reminder.kind=$5`)).
		WithArgs(at.UTC(), at.UTC().Add(24*time.Hour), c.linkID, c.recipientID, c.kind)
}

func workerRecheckRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"workspace_id", "deadline", "max_visits", "visits_used", "dedupe_key", "code"})
}

func TestWorkerRunOnceSkipsNonPositiveLimits(t *testing.T) {
	s, _ := newWorkerTestService(t)
	for _, limit := range []int{0, -1} {
		if err := s.RunOnce(t.Context(), limit); err != nil {
			t.Errorf("RunOnce(limit=%d) = %v, want nil without a query", limit, err)
		}
	}
}

func TestWorkerRunOnceRechecksAndCommitsEachCandidate(t *testing.T) {
	s, mock := newWorkerTestService(t)
	scanAt := workerFixtureTime
	expiryAt, visitsAt := scanAt.Add(time.Second), scanAt.Add(2*time.Second)
	expiry := workerCandidate{uuid.New(), uuid.New(), KindExpiry24h}
	visits := workerCandidate{uuid.New(), uuid.New(), KindVisits80}
	expectWorkerShortlist(mock, scanAt, 4).WillReturnRows(workerShortlistRows(expiry, visits)).RowsWillBeClosed()

	expiryWorkspace, visitsWorkspace := uuid.New(), uuid.New()
	deadline := scanAt.Add(2 * time.Hour)
	expiryKey := "expiry:" + expiry.linkID.String() + ":" + deadline.UTC().Format(time.RFC3339Nano)
	mock.ExpectBegin()
	expectWorkerLocks(mock, expiry)
	expectWorkerRecheck(mock, expiryAt, expiry).
		WillReturnRows(workerRecheckRows().AddRow(expiryWorkspace, deadline, -1, 0, expiryKey, "new-code"))
	mock.ExpectExec(workerSQL(workerInsertSQL)).WithArgs(sqlmock.AnyArg(), expiry.recipientID, expiryWorkspace, expiry.linkID, expiry.kind,
		"短链即将到期", "短链 /r/new-code 将在 24 小时内到达实际截止时间（2026-11-21T19:30:00Z）。", expiryKey, expiryAt.UTC()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	visitsKey := "visits:" + visits.linkID.String() + ":3"
	mock.ExpectBegin()
	expectWorkerLocks(mock, visits)
	expectWorkerRecheck(mock, visitsAt, visits).
		WillReturnRows(workerRecheckRows().AddRow(visitsWorkspace, nil, 5, 4, visitsKey, nil))
	mock.ExpectExec(workerSQL(workerInsertSQL)).WithArgs(sqlmock.AnyArg(), visits.recipientID, visitsWorkspace, visits.linkID, visits.kind,
		"短链访问次数提醒", "该短链 的访问次数已使用 4/5，达到 80% 提醒阈值。", visitsKey, visitsAt.UTC()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	clockCalls := 0
	clock := func() time.Time {
		at := scanAt.Add(time.Duration(clockCalls) * time.Second)
		clockCalls++
		return at
	}
	if err := s.runOnce(t.Context(), clock, 2); err != nil {
		t.Fatal(err)
	}
	if clockCalls != 3 {
		t.Errorf("clock called %d times, want once for shortlist and once per recheck", clockCalls)
	}
}

func TestWorkerRunOnceContinuesAfterLockedCandidate(t *testing.T) {
	s, mock := newWorkerTestService(t)
	at := workerFixtureTime
	locked := workerCandidate{uuid.New(), uuid.New(), KindExpiry24h}
	available := workerCandidate{uuid.New(), uuid.New(), KindExpiry24h}
	expectWorkerShortlist(mock, at, 2).WillReturnRows(workerShortlistRows(locked, available)).RowsWillBeClosed()

	mock.ExpectBegin()
	expectWorkerRecipientLock(mock, locked).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))
	mock.ExpectRollback()

	workspace := uuid.New()
	deadline := at.Add(time.Hour)
	key := "expiry:" + available.linkID.String() + ":" + deadline.UTC().Format(time.RFC3339Nano)
	mock.ExpectBegin()
	expectWorkerLocks(mock, available)
	expectWorkerRecheck(mock, at, available).WillReturnRows(workerRecheckRows().AddRow(workspace, deadline, -1, 0, key, "later-code"))
	mock.ExpectExec(workerSQL(workerInsertSQL)).WithArgs(sqlmock.AnyArg(), available.recipientID, workspace, available.linkID, available.kind,
		"短链即将到期", "短链 /r/later-code 将在 24 小时内到达实际截止时间（2026-11-21T18:30:00Z）。", key, at.UTC()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := s.runOnce(t.Context(), func() time.Time { return at }, 1); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRunOnceSkipsUnavailableCandidates(t *testing.T) {
	for _, tc := range []struct {
		name, kind   string
		busy, locked bool
	}{
		{name: "busy_recipient", kind: KindVisits80, busy: true},
		{name: "locked_link", kind: KindExpiry24h, locked: true},
		{name: "cutoff_passed", kind: KindExpiry24h},
		{name: "preference_disabled", kind: KindVisits80},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mock := newWorkerTestService(t)
			scanAt := workerFixtureTime
			c := workerCandidate{uuid.New(), uuid.New(), tc.kind}
			expectWorkerShortlist(mock, scanAt, 2).WillReturnRows(workerShortlistRows(c)).RowsWillBeClosed()
			mock.ExpectBegin()
			expectWorkerRecipientLock(mock, c).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(!tc.busy))
			wantClockCalls := 1
			if !tc.busy {
				rows := sqlmock.NewRows([]string{"link_id"})
				if !tc.locked {
					rows.AddRow(c.linkID)
				}
				expectWorkerLinkLock(mock, c).WillReturnRows(rows)
				if !tc.locked {
					// A fresh cutoff or preference can invalidate a shortlisted candidate.
					expectWorkerRecheck(mock, scanAt.Add(time.Second), c).WillReturnRows(workerRecheckRows())
					wantClockCalls++
				}
			}
			mock.ExpectRollback() // No INSERT or COMMIT is permitted on a skip.

			calls := 0
			clock := func() time.Time {
				at := scanAt.Add(time.Duration(calls) * time.Second)
				calls++
				return at
			}
			if err := s.runOnce(t.Context(), clock, 1); err != nil {
				t.Fatal(err)
			}
			if calls != wantClockCalls {
				t.Errorf("clock called %d times, want %d", calls, wantClockCalls)
			}
		})
	}
}

func TestWorkerRunOncePropagatesShortlistErrors(t *testing.T) {
	for _, stage := range []string{"query", "rows"} {
		t.Run(stage, func(t *testing.T) {
			s, mock := newWorkerTestService(t)
			at := workerFixtureTime
			failure := errors.New("shortlist " + stage + " failed")
			query := expectWorkerShortlist(mock, at, 4)
			if stage == "query" {
				query.WillReturnError(failure)
			} else {
				c := workerCandidate{uuid.New(), uuid.New(), KindExpiry24h}
				query.WillReturnRows(workerShortlistRows(c, c).RowError(1, failure)).RowsWillBeClosed()
			}
			if err := s.runOnce(t.Context(), func() time.Time { return at }, 2); !errors.Is(err, failure) {
				t.Fatalf("runOnce = %v, want %v without a transaction", err, failure)
			}
		})
	}
}

func TestWorkerRunOncePropagatesTransactionalErrors(t *testing.T) {
	for _, stage := range []string{"recheck", "insert", "commit"} {
		t.Run(stage, func(t *testing.T) {
			s, mock := newWorkerTestService(t)
			at := workerFixtureTime
			c := workerCandidate{uuid.New(), uuid.New(), KindVisits80}
			workspace := uuid.New()
			key := "visits:" + c.linkID.String() + ":0"
			failure := errors.New(stage + " failed")
			expectWorkerShortlist(mock, at, 2).WillReturnRows(workerShortlistRows(c)).RowsWillBeClosed()
			mock.ExpectBegin()
			expectWorkerLocks(mock, c)

			recheck := expectWorkerRecheck(mock, at, c)
			if stage == "recheck" {
				recheck.WillReturnError(failure)
			} else {
				recheck.WillReturnRows(workerRecheckRows().AddRow(workspace, nil, 10, 8, key, "test-code"))
				insert := mock.ExpectExec(workerSQL(workerInsertSQL)).WithArgs(sqlmock.AnyArg(), c.recipientID, workspace, c.linkID, c.kind,
					"短链访问次数提醒", "短链 /r/test-code 的访问次数已使用 8/10，达到 80% 提醒阈值。", key, at.UTC())
				if stage == "insert" {
					insert.WillReturnError(failure)
				} else {
					insert.WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			if stage == "commit" {
				mock.ExpectCommit().WillReturnError(failure)
			} else {
				mock.ExpectRollback()
			}

			if err := s.runOnce(t.Context(), func() time.Time { return at }, 1); !errors.Is(err, failure) {
				t.Fatalf("runOnce = %v, want %v", err, failure)
			}
		})
	}
}
