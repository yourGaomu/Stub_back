package logs

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestAnalyticsAttemptMetricsMatchSummaryAndBuckets(t *testing.T) {
	for _, rangeName := range []string{"24h", "7d"} {
		t.Run(rangeName, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			// 十个事件：两次放行（含历史 consumes=false）、四次主链路拒绝、两次挑战、两次口令错误。
			// 历史 consumes=true 的拒绝与 challenge 也不能改变新的尝试计数口径。
			metricSQL := `count\(\*\),count\(\*\) FILTER\(WHERE l.event_type='link_access' AND l.result!='challenge'\),count\(\*\) FILTER\(WHERE l.event_type='link_access' AND l.result='allowed' AND l.reason IS NULL\),count\(\*\) FILTER\(WHERE l.result='denied'\)`
			mock.ExpectQuery(`SELECT `+metricSQL+` FROM short_link_access_logs l JOIN short_links sl .* WHERE l.accessed_at >= \$1 AND l.accessed_at < \$2`).
				WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"requests", "attempts", "redirects", "failures"}).AddRow(10, 6, 2, 6))
			mock.ExpectQuery(`SELECT l.reason,count\(\*\) .* AND l.result='denied' AND l.reason IS NOT NULL GROUP BY l.reason`).
				WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"reason", "count"}).AddRow("ip_denied", 4).AddRow("password_invalid", 2))
			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Hour)
			bucket := `date_trunc\('hour',l.accessed_at\)`
			if rangeName == "7d" {
				at = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
				bucket = `date_trunc\('day',l.accessed_at AT TIME ZONE \$3\) AT TIME ZONE \$3`
			}
			query := mock.ExpectQuery(`SELECT ` + bucket + `,` + metricSQL + ` .* GROUP BY 1 ORDER BY 1`)
			if rangeName == "7d" {
				query.WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "UTC")
			} else {
				query.WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg())
			}
			query.WillReturnRows(sqlmock.NewRows([]string{"at", "requests", "attempts", "redirects", "failures"}).AddRow(at, 10, 6, 2, 6))
			got, err := NewService(db).Summary(context.Background(), Actor{Role: "admin"}, rangeName, "UTC", "")
			if err != nil {
				t.Fatal(err)
			}
			want := Metric{RequestCount: 10, VisitAttemptCount: 6, RedirectCount: 2, FailureCount: 6}
			if got.Totals != want {
				t.Fatalf("totals = %+v, want %+v", got.Totals, want)
			}
			var sum Metric
			for _, point := range got.Series {
				sum.RequestCount += point.RequestCount
				sum.VisitAttemptCount += point.VisitAttemptCount
				sum.RedirectCount += point.RedirectCount
				sum.FailureCount += point.FailureCount
			}
			if sum != want || !reflect.DeepEqual(got.FailureReasons, []FailureReason{{Reason: "ip_denied", Count: 4}, {Reason: "password_invalid", Count: 2}}) {
				t.Fatalf("series sum=%+v failures=%+v", sum, got.FailureReasons)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
