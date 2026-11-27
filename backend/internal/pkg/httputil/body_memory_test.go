package httputil

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

type bodyReaderOnly struct{ io.Reader }

type delayedBodyReadFailure struct{ err error }

func (r delayedBodyReadFailure) Read([]byte) (int, error) {
	// Windows can report zero elapsed time for a synchronous in-memory read.
	// A controlled delay distinguishes a measured duration from a zero placeholder.
	time.Sleep(20 * time.Millisecond)
	return 0, r.err
}

func TestReadRequestBodyChunksPreservesContent(t *testing.T) {
	for _, size := range []int{0, 1, 512, 513, 1 << 20, (2 << 20) + 17} {
		body := bytes.Repeat([]byte{'x'}, size)
		for _, declared := range []int64{-1, 0, 1, int64(size), 1 << 40} {
			t.Run(fmt.Sprintf("size_%d_length_%d", size, declared), func(t *testing.T) {
				req := &http.Request{Body: io.NopCloser(bodyReaderOnly{bytes.NewReader(body)}), ContentLength: declared, Header: make(http.Header)}
				got, err := ReadRequestBodyWithPrealloc(req)
				if err != nil || !bytes.Equal(got, body) {
					t.Fatalf("body was lost or changed: %v", err)
				}
			})
		}
	}
}

func TestReadRequestBodyChunksPreservesReadErrors(t *testing.T) {
	for _, readErr := range []error{io.ErrUnexpectedEOF, errors.New("connection reset")} {
		reader := io.MultiReader(bytes.NewReader([]byte("partial")), iotest.ErrReader(readErr))
		req := &http.Request{Body: io.NopCloser(reader), ContentLength: 100, Header: make(http.Header)}
		got, err := ReadRequestBodyWithPrealloc(req)
		if !errors.Is(err, readErr) || got != nil {
			t.Fatalf("truncated body accepted: body=%q err=%v", got, err)
		}
	}
}

func TestReadRequestBodyFailureReportsActualWireProgress(t *testing.T) {
	for _, size := range []int{7, (1 << 20) + 37} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			cause := errors.New("private-transport-marker")
			reader := io.MultiReader(bytes.NewReader(bytes.Repeat([]byte{'x'}, size)), delayedBodyReadFailure{cause})
			budget := NewBodyMemoryBudget(8 << 20)
			ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
			defer release()
			req := (&http.Request{Body: io.NopCloser(reader), ContentLength: int64(size + 100), Header: make(http.Header)}).WithContext(ctx)
			body, err := ReadRequestBodyWithPrealloc(req)
			if body != nil || !errors.Is(err, cause) {
				t.Fatalf("failed upload must preserve its cause without exposing partial content: %v", err)
			}
			if requestBodyBudgetUsed(budget) != 0 {
				t.Fatal("failed upload retained its memory reservation")
			}
			var progress *RequestBodyReadError
			if !errors.As(err, &progress) {
				t.Fatal("actual reader failure has no progress annotation")
			}
			if progress.BytesRead != int64(size) || progress.Elapsed < 10*time.Millisecond {
				t.Fatalf("received=%d elapsed=%s, want actual bytes=%d and measured time", progress.BytesRead, progress.Elapsed, size)
			}
			for _, field := range DescribeRequestBodyReadFailure(req, err).LogFields() {
				if field.String == cause.Error() {
					t.Fatal("diagnostics leaked transport text")
				}
			}
		})
	}
}

func TestReadRequestBodyFailureDoesNotInventPreReadProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := (&http.Request{Body: io.NopCloser(strings.NewReader("private-content")), Header: make(http.Header)}).WithContext(ctx)
	body, err := ReadRequestBodyWithPrealloc(req)
	if body != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("pre-read cancellation was lost")
	}
	var progress *RequestBodyReadError
	if errors.As(err, &progress) {
		t.Fatal("unperformed read was reported as measured")
	}
	for _, field := range DescribeRequestBodyReadFailure(req, err).LogFields() {
		if field.Key == "bytes_read" || field.Key == "read_ms" {
			t.Fatal("unobserved progress must stay unknown")
		}
	}
}

func TestReadRequestBodyDecompressionFailureUsesWireBytes(t *testing.T) {
	var zipped bytes.Buffer
	w := gzip.NewWriter(&zipped)
	if _, err := io.WriteString(w, strings.Repeat("private-content", 100)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	truncated := zipped.Bytes()[:zipped.Len()-4]
	req := &http.Request{Body: io.NopCloser(bytes.NewReader(truncated)), ContentLength: int64(len(truncated)), Header: http.Header{"Content-Encoding": []string{"gzip"}}}
	body, err := ReadRequestBodyWithPrealloc(req)
	if body != nil || err == nil {
		t.Fatal("truncated compressed payload accepted")
	}
	var progress *RequestBodyReadError
	if !errors.As(err, &progress) || progress.BytesRead != int64(len(truncated)) {
		t.Fatal("wire progress missing or replaced by decoded byte count")
	}
	if got := DescribeRequestBodyReadFailure(req, err).Kind; got != RequestBodyReadErrorDecodeContentEncoding {
		t.Fatalf("kind=%s", got)
	}
}

func TestReadRequestBodyChunksPreservesMaxBytesReader(t *testing.T) {
	body := bytes.Repeat([]byte{'x'}, (2<<20)+1)
	req := &http.Request{Body: http.MaxBytesReader(nil, io.NopCloser(bytes.NewReader(body)), 2<<20), ContentLength: int64(len(body)), Header: make(http.Header)}
	got, err := ReadRequestBodyWithPrealloc(req)
	var limitErr *http.MaxBytesError
	if !errors.As(err, &limitErr) || limitErr.Limit != 2<<20 || got != nil {
		t.Fatalf("body limit changed: len=%d err=%v", len(got), err)
	}
}

func BenchmarkReadLargeBody(b *testing.B) {
	for _, size := range []int{512 << 10, 69 << 20} {
		b.Run(fmt.Sprintf("bytes_%d", size), func(b *testing.B) {
			body := bytes.Repeat([]byte{'x'}, size)
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req := &http.Request{Body: io.NopCloser(bodyReaderOnly{bytes.NewReader(body)}), ContentLength: int64(size), Header: make(http.Header)}
				got, err := ReadRequestBodyWithPrealloc(req)
				if err != nil || !bytes.Equal(got, body) {
					b.Fatalf("body changed: %v", err)
				}
			}
		})
	}
}
