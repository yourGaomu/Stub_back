package csvexport

import (
	"context"
	"encoding/csv"
	"io"
	"strings"
	"testing"
)

// Domain callbacks may have written all permitted rows before detecting the next
// row. Prepare must discard that staged file and never expose it to the caller.
func TestPrepareDiscardsHeaderAndPartialRowsAfterDomainError(t *testing.T) {
	dir := t.TempDir()
	file, err := prepare(context.Background(), dir, 10, 1024, func(write func([]string) error) (int, error) {
		if err := write([]string{"header"}); err != nil {
			return 0, err
		}
		if err := write([]string{"partial"}); err != nil {
			return 0, err
		}
		return 0, io.ErrUnexpectedEOF
	})
	if file != nil || err != io.ErrUnexpectedEOF {
		t.Fatalf("file=%v err=%v", file, err)
	}
}

func TestPrepareEmptyDatasetStillHasCSVHeader(t *testing.T) {
	file, err := Prepare(context.Background(), func(write func([]string) error) (int, error) { return 0, write([]string{"link_id", "title"}) })
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(string(data[3:]))).ReadAll()
	if err != nil || len(rows) != 1 || file.Rows != 0 {
		t.Fatalf("rows=%v count=%d err=%v", rows, file.Rows, err)
	}
}
