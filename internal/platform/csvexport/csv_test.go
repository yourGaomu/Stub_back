package csvexport

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestSafeCell(t *testing.T) {
	for _, value := range []string{"=1+1", "+cmd", "-1", "@SUM(A1)", "  =1", "\t=1", "\r+1", "\ufeff=1"} {
		if got := SafeCell(value); got != "'"+value {
			t.Errorf("SafeCell(%q)=%q", value, got)
		}
	}
	for _, value := range []string{"title", "中文😀", "2026-01-01", "", "https://example.com/?q=1"} {
		if got := SafeCell(value); got != value {
			t.Errorf("changed safe value %q", value)
		}
	}
}

func TestPrepareWritesBOMEscapesUnicodeAndCleansFile(t *testing.T) {
	file, err := Prepare(context.Background(), func(write func([]string) error) (int, error) {
		if err := write([]string{"title", "notes"}); err != nil {
			return 0, err
		}
		return 1, write([]string{"中文😀,标题", "=SUM(A1)\nline\"two"})
	})
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "\xef\xbb\xbf") || file.Rows != 1 || file.Bytes != int64(len(data)) {
		t.Fatalf("unexpected metadata: %+v", file)
	}
	rows, err := csv.NewReader(strings.NewReader(string(data[3:]))).ReadAll()
	if err != nil || len(rows) != 2 || rows[1][0] != "中文😀,标题" || !strings.HasPrefix(rows[1][1], "'=SUM") {
		t.Fatalf("rows=%v error=%v", rows, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file remains: %v", err)
	}
}

func TestPrepareRejectsLimitsAndCleansFailedFiles(t *testing.T) {
	for _, test := range []struct {
		name     string
		maxRows  int
		maxBytes int64
		generate func(func([]string) error) (int, error)
		want     error
	}{
		{"row_limit", 1, 1024, func(write func([]string) error) (int, error) {
			_ = write([]string{"header"})
			_ = write([]string{"one"})
			return 2, write([]string{"two"})
		}, ErrLimit},
		{"byte_limit", 10, 20, func(write func([]string) error) (int, error) {
			_ = write([]string{"header"})
			return 1, write([]string{strings.Repeat("x", 100)})
		}, ErrLimit},
		{"query_failure", 10, 1024, func(write func([]string) error) (int, error) {
			_ = write([]string{"header"})
			return 0, io.ErrUnexpectedEOF
		}, io.ErrUnexpectedEOF},
		{"no_header", 10, 1024, func(func([]string) error) (int, error) { return 0, nil }, ErrEmpty},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			file, err := prepare(context.Background(), dir, test.maxRows, test.maxBytes, test.generate)
			if file != nil || !errors.Is(err, test.want) {
				t.Fatalf("file=%v err=%v want=%v", file, err, test.want)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary files not cleaned: %v %v", entries, err)
			}
		})
	}
}

func TestPrepareHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	file, err := Prepare(ctx, func(write func([]string) error) (int, error) { return 0, write([]string{"header"}) })
	if file != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("file=%v err=%v", file, err)
	}
}
