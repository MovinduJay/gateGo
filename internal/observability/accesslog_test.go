package observability

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccessLogCapturesCompletionFields(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	handler := AccessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-ID", "request-123")
		w.Header().Set("X-Trace-ID", "trace-456")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}), logger)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/api/items?ignored=true", nil))
	logLine := output.String()
	for _, want := range []string{`"msg":"request completed"`, `"method":"POST"`, `"path":"/api/items"`, `"status":201`, `"response_bytes":7`, `"request_id":"request-123"`, `"trace_id":"trace-456"`} {
		if !strings.Contains(logLine, want) {
			t.Fatalf("access log missing %s: %s", want, logLine)
		}
	}
}

func TestAccessLogWriterUnwrapsResponseWriter(t *testing.T) {
	underlying := httptest.NewRecorder()
	w := &accessLogWriter{ResponseWriter: underlying, status: http.StatusOK}
	if w.Unwrap() != underlying {
		t.Fatal("Unwrap must expose the underlying response writer")
	}
}
