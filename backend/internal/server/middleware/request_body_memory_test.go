package middleware

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type cancelAfterUploadReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r cancelAfterUploadReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil {
		r.cancel()
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func TestCanceledUploadAccessLogRetainsProgressAndNoUpstream(t *testing.T) {
	sink := initMiddlewareTestLogger(t)
	r := gin.New()
	r.Use(RequestLogger(), ClientRequestID(), Logger())
	selected := false
	r.POST("/responses", func(c *gin.Context) {
		_, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		if RejectRequestBodyReadError(c, err) {
			return
		}
		selected = true
		c.Status(http.StatusOK)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const prefix = "private-upload-marker"
	req := httptest.NewRequest(http.MethodPost, "/responses", io.NopCloser(cancelAfterUploadReader{strings.NewReader(prefix), cancel})).WithContext(ctx)
	req.ContentLength = 1024
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if selected || w.Code != 499 || w.Body.Len() != 0 {
		t.Fatal("canceled upload changed response or selected an upstream")
	}
	var access []*logger.LogEvent
	for _, event := range sink.list() {
		if event.Message == "http request completed" {
			access = append(access, event)
		}
	}
	if len(access) != 1 {
		t.Fatalf("access events=%d, want one", len(access))
	}
	event := access[0]
	if event.Level != "warn" || event.Fields["failure_stage"] != "request_body" || event.Fields["error_kind"] != "client_disconnect" {
		t.Fatalf("canceled upload lost its stage/classification: level=%s fields=%v", event.Level, event.Fields)
	}
	if event.Fields["bytes_read"] != int64(len(prefix)) || event.Fields["content_length"] != int64(1024) {
		t.Fatalf("wrong progress: %v", event.Fields)
	}
	if _, exists := event.Fields["account_id"]; exists {
		t.Fatal("pre-selection upload failure has an upstream account")
	}
	encoded, _ := json.Marshal(event.Fields)
	if strings.Contains(string(encoded), prefix) {
		t.Fatal("diagnostic leaked upload content")
	}
}

type observedUploadReadCloser struct {
	io.ReadCloser
	received chan struct{}
	once     sync.Once
}

func (r *observedUploadReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.once.Do(func() { close(r.received) })
	}
	return n, err
}

func TestRequestBodyUploadDiagnosticsRealHTTP(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		for _, mode := range []string{"cancel", "read_timeout", "gzip_complete"} {
			t.Run(map[bool]string{false: "http1", true: "http2"}[http2]+"/"+mode, func(t *testing.T) {
				sink := initMiddlewareTestLogger(t)
				r := gin.New()
				r.Use(RequestLogger(), ClientRequestID(), Logger())
				var selected atomic.Bool
				r.POST("/responses", func(c *gin.Context) {
					body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
					if err != nil {
						if !RejectRequestBodyReadError(c, err) {
							AbortWithRequestBodyReadFailure(c, "upload_fixture", err)
						}
						return
					}
					if string(body) != `{"input":"private-compressed-body"}` {
						t.Error("decoded upload changed")
					}
					selected.Store(true)
					c.Status(http.StatusNoContent)
				})
				received, completed := make(chan struct{}), make(chan struct{})
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					defer close(completed)
					req.Body = &observedUploadReadCloser{ReadCloser: req.Body, received: received}
					if mode == "read_timeout" {
						if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
							t.Error(err)
						}
					}
					r.ServeHTTP(w, req)
				}))
				server.EnableHTTP2 = http2
				server.StartTLS()
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var payload bytes.Buffer
				if mode == "gzip_complete" {
					zip := gzip.NewWriter(&payload)
					if _, err := io.WriteString(zip, `{"input":"private-compressed-body"}`); err != nil {
						t.Fatal(err)
					}
					if err := zip.Close(); err != nil {
						t.Fatal(err)
					}
				}
				reader, writer := io.Pipe()
				defer func() { _ = reader.Close() }()
				defer func() { _ = writer.Close() }()
				var body io.Reader = reader
				if mode == "gzip_complete" {
					body = bytes.NewReader(payload.Bytes())
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", body)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-Client-Request-ID", "01a1819a-f9dd-78b3-bd12-c917a9326e9f")
				req.Header.Set("Cf-Ray", "a6120a6c2f24fa50-NRT")
				if mode == "gzip_complete" {
					req.Header.Set("Content-Encoding", "gzip")
				} else {
					req.ContentLength = 4096
				}
				clientDone := make(chan error, 1)
				go func() {
					response, e := server.Client().Do(req)
					if response != nil {
						if mode == "gzip_complete" && response.StatusCode != http.StatusNoContent {
							e = fmt.Errorf("complete upload status=%d", response.StatusCode)
						}
						_ = response.Body.Close()
					}
					clientDone <- e
				}()
				if mode != "gzip_complete" {
					if _, err := writer.Write([]byte("private-upload-prefix")); err != nil {
						t.Fatal(err)
					}
					select {
					case <-received:
					case <-ctx.Done():
						t.Fatal("server did not receive upload prefix")
					}
					if mode == "cancel" {
						cancel()
					}
				}
				select {
				case <-completed:
				case <-time.After(3 * time.Second):
					t.Fatal("server did not finish upload lifecycle")
				}
				_ = writer.Close()
				if mode != "gzip_complete" {
					cancel()
				}
				select {
				case err := <-clientDone:
					if mode == "gzip_complete" && err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("client leaked after upload ended")
				}
				var access []*logger.LogEvent
				for _, event := range sink.list() {
					if event.Message == "http request completed" {
						access = append(access, event)
					}
				}
				if len(access) != 1 {
					t.Fatalf("access events=%d", len(access))
				}
				event := access[0]
				wantProto := "HTTP/1.1"
				if http2 {
					wantProto = "HTTP/2.0"
				}
				if event.Fields["protocol"] != wantProto {
					t.Fatalf("wrong protocol: %v", event.Fields["protocol"])
				}
				if mode == "cancel" {
					require.EqualValues(t, 499, event.Fields["status_code"])
					if selected.Load() || event.Fields["failure_stage"] != "request_body" || event.Fields["bytes_read"] != int64(len("private-upload-prefix")) {
						t.Fatalf("lost upload cancellation: %v", event.Fields)
					}
				} else if mode == "read_timeout" {
					require.EqualValues(t, 408, event.Fields["status_code"])
					if selected.Load() {
						t.Fatalf("read timeout misclassified: %v", event.Fields)
					}
					found := false
					for _, failure := range sink.list() {
						if failure.Message == "read request body failed" {
							found = failure.Fields["error_kind"] == "read_timeout" && failure.Fields["bytes_read"] == int64(len("private-upload-prefix"))
						}
					}
					if !found {
						t.Fatal("real read timeout lost progress diagnostics")
					}
				} else if !selected.Load() {
					t.Fatal("complete gzip upload never reached handler")
				}
				if event.Fields["incoming_cf_ray"] != "a6120a6c2f24fa50-NRT" {
					t.Fatal("lost cross-layer correlation")
				}
				for _, event := range sink.list() {
					encoded, _ := json.Marshal(event.Fields)
					if strings.Contains(string(encoded), "private-upload-prefix") || strings.Contains(string(encoded), "private-compressed-body") {
						t.Fatal("wire diagnostics leaked payload")
					}
				}
			})
		}
	}
}
