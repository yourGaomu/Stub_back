package httpapi

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strconv"
	"testing"
)

func TestDecodeMCPFileContentEnforcesDecodedSizeLimit(t *testing.T) {
	for _, size := range []int{7 << 20, (7 << 20) + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, size))
			_, err := decodeMCPFileContent(encoded)
			if size == 7<<20 && err != nil {
				t.Fatalf("decode maximum allowed size: %v", err)
			}
			if size > 7<<20 && !errors.Is(err, errMCPFileTooLarge) {
				t.Fatalf("error = %v, want oversized-file error", err)
			}
		})
	}
}

func TestDecodeMCPFileContentAcceptsDataURLAndRejectsInvalidBase64(t *testing.T) {
	content, err := decodeMCPFileContent("data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString([]byte("hello")))
	if err != nil || string(content) != "hello" {
		t.Fatalf("decoded content = %q, error = %v", content, err)
	}
	if _, err := decodeMCPFileContent("!!!!"); err == nil {
		t.Fatal("invalid Base64 unexpectedly succeeded")
	}
}

func TestDecodeMCPPartContentLimitsDecodedBytesAndAcceptsRawBase64(t *testing.T) {
	part := bytes.Repeat([]byte{'x'}, 32<<20)
	reader, size, err := prepareMCPPartContent(base64.RawStdEncoding.EncodeToString(part))
	if err != nil || size != int64(len(part)) {
		t.Fatalf("prepared max part = %d bytes, error = %v", size, err)
	}
	readSize, err := io.Copy(io.Discard, reader)
	if err != nil || readSize != size {
		t.Fatalf("decoded max part = %d bytes, error = %v", readSize, err)
	}
	oversized := base64.StdEncoding.EncodeToString(append(part, 'x'))
	if _, _, err := prepareMCPPartContent(oversized); !errors.Is(err, errMCPPartTooLarge) {
		t.Fatalf("error = %v, want oversized part", err)
	}
	if _, _, err := prepareMCPPartContent("not base64!"); err == nil {
		t.Fatal("invalid Base64 unexpectedly succeeded")
	}
	reader, size, err = prepareMCPPartContent("YWI")
	if err != nil || size != 2 {
		t.Fatalf("prepare unpadded Base64 = size %d, error = %v", size, err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil || string(decoded) != "ab" {
		t.Fatalf("decode unpadded Base64 = %q, error = %v", decoded, err)
	}
}
