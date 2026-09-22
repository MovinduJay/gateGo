package observability

import (
	"log/slog"
	"net/http"
	"time"
)

// AccessLog records one structured completion event per HTTP request. The
// wrapper exposes Unwrap so http.ResponseController can still reach optional
// interfaces such as Flusher and Hijacker on the underlying writer.
func AccessLog(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		wrapped := &accessLogWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		logger.Info("request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.status,
			"response_bytes", wrapped.bytes,
			"duration_ms", float64(time.Since(started).Microseconds())/1000,
			"peer", r.RemoteAddr,
			"request_id", wrapped.Header().Get("X-Request-ID"),
			"trace_id", wrapped.Header().Get("X-Trace-ID"),
		)
	})
}

type accessLogWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (w *accessLogWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *accessLogWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	w.bytes += n
	return n, err
}

func (w *accessLogWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
