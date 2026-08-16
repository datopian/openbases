// Package httplog records one line per request.
//
// Until now only failures were logged, which meant a request that returned 200
// with an empty body left no trace at all — and "the page is empty" could not
// be told apart from "the request never arrived" without asking the person at
// the screen to read a JSON body back to us. One line per request answers that
// without involving them.
package httplog

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/datopian/workgraph/internal/authn"
)

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Middleware logs method, path, status, size, duration and the caller.
//
// The response SIZE is included deliberately: a 200 carrying two bytes is an
// empty list, and that distinction is the whole reason this exists. The caller
// is identified by subject rather than email, so the line is useful without
// putting a personal address in every entry.
func Middleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health checks would otherwise dominate the log and push the
			// interesting lines out of any window worth reading.
			if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			rec := &recorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			subject, user := "-", "-"
			if id, ok := authn.FromContext(r.Context()); ok {
				if id.Subject != "" {
					subject = id.Subject
				}
				if id.UserID != "" {
					user = id.UserID
				}
			}

			log.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"ms", time.Since(start).Milliseconds(),
				"subject", subject,
				"user", user,
			)
		})
	}
}
