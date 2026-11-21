package csvexport

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"os"
	"strings"
	"unicode"
)

const (
	MaxRows  = 10000
	MaxBytes = 20 << 20
)

var (
	ErrLimit = errors.New("CSV export limit exceeded")
	ErrEmpty = errors.New("CSV export requires a header")
)

type File struct {
	*os.File
	Rows  int
	Bytes int64
}

func (f *File) Close() error {
	name := f.Name()
	err := f.File.Close()
	removeErr := os.Remove(name)
	return errors.Join(err, removeErr)
}

type boundedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *boundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, ErrLimit
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, err
}

// SafeCell prevents spreadsheet formula evaluation, including whitespace prefixes.
func SafeCell(value string) string {
	probe := strings.TrimLeftFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '\ufeff'
	})
	if probe != "" && strings.ContainsRune("=+-@", rune(probe[0])) {
		return "'" + value
	}
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") {
		return "'" + value
	}
	return value
}

// Prepare stages the complete CSV before the HTTP response starts.
// generate writes one header followed by data rows and returns its data-row count.
func Prepare(ctx context.Context, generate func(func([]string) error) (int, error)) (*File, error) {
	return prepare(ctx, "", MaxRows, MaxBytes, generate)
}

func prepare(ctx context.Context, dir string, maxRows int, maxBytes int64, generate func(func([]string) error) (int, error)) (_ *File, returnErr error) {
	temp, err := os.CreateTemp(dir, "short-links-export-*.csv")
	if err != nil {
		return nil, err
	}
	file := &File{File: temp}
	defer func() {
		if returnErr != nil {
			_ = file.Close()
		}
	}()
	bounded := &boundedWriter{writer: temp, remaining: maxBytes}
	if _, err := bounded.Write([]byte{0xef, 0xbb, 0xbf}); err != nil {
		return nil, err
	}
	writer := csv.NewWriter(bounded)
	writer.UseCRLF = true
	written := 0
	writeRow := func(row []string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if written > maxRows {
			return ErrLimit
		}
		cells := make([]string, len(row))
		for i, value := range row {
			cells[i] = SafeCell(value)
		}
		if err := writer.Write(cells); err != nil {
			return err
		}
		written++
		return nil
	}
	count, err := generate(writeRow)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	if written == 0 {
		return nil, ErrEmpty
	}
	if count != written-1 {
		return nil, errors.New("CSV row count mismatch")
	}
	if count > maxRows {
		return nil, ErrLimit
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	file.Rows, file.Bytes = count, maxBytes-bounded.remaining
	return file, nil
}
