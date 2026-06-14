package httpx

import (
	"log/slog"
	"net/http"
	"time"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush walks the wrapped writer chain via http.ResponseController so SSE
// flushes survive intermediate wrappers (scs sessionResponseWriter only
// implements Unwrap, not Flush; a direct type assertion would miss it and
// keep events buffered until the connection looked dead to the client).
func (s *statusRecorder) Flush() {
	_ = http.NewResponseController(s.ResponseWriter).Flush()
}

// Unwrap exposes the underlying writer for http.ResponseController so callers
// using SetReadDeadline / SetWriteDeadline / Hijack can also reach the real
// writer through this wrapper.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

func RequestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w, status: 200}
			start := time.Now()
			next.ServeHTTP(rec, r)
			log.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"dur_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		})
	}
}

// PrimeSSE writes SSE response headers and locks the status at 200 *before*
// any downstream call to http.ResponseController(w).Flush() unwraps past
// middleware wrappers (chi compressor, scs session writer, request logger).
//
// Why: datastar.NewSSE finishes with rc.Flush(); ResponseController walks the
// Unwrap chain to the deepest http.Flusher and flushes that — bypassing
// compressResponseWriter.WriteHeader. Result: raw headers (no Content-Encoding)
// reach the client, then subsequent Write calls go through the compressor and
// emit gzipped/brotli/zstd bytes. Browser decodes garbage.
//
// Calling PrimeSSE(w) first lets the compressor's WriteHeader hook pick an
// encoder from the Accept-Encoding header + our Content-Type=text/event-stream
// and set Content-Encoding before the response status reaches the wire.
//
// Order required: PrimeSSE → datastar.NewSSE → patches. (For session-mutating
// handlers it's: ReadSignals → mutate session → commitSession → PrimeSSE →
// NewSSE.)
func PrimeSSE(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
}

func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic", "err", rec, "path", r.URL.Path)
					http.Error(w, "internal error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
