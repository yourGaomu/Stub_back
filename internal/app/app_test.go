package app

import (
	"context"
	"testing"
	"time"
)

func TestCloseCancelsAndWaitsForNotificationScan(t *testing.T) {
	var application App
	started := make(chan int, 1)
	cancelled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	closed := make(chan struct{})
	application.startNotificationWorker(context.Background(), func(ctx context.Context, limit int) error {
		started <- limit
		<-ctx.Done()
		close(cancelled)
		<-release
		return ctx.Err()
	})
	defer application.notificationStop()

	select {
	case limit := <-started:
		if limit != 100 {
			t.Errorf("startup scan limit = %d, want 100", limit)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not scan immediately on startup")
	}

	go func() {
		application.Close()
		close(closed)
	}()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel the active scan")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before the scan finished")
	default:
	}
	release <- struct{}{}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the scan finished")
	}
}
