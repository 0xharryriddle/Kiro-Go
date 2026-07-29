package proxy

import (
	"errors"
	"io"
	"net/http"

	"kiro-go/config"
)

// errRequestBodyTooLarge marks a body that exceeded the configured ceiling, so a
// caller can answer 413 instead of the 400 it would otherwise send for an
// unreadable body. It is a sentinel rather than a typed error because callers
// only need to branch on "too large" vs "everything else".
var errRequestBodyTooLarge = errors.New("request body exceeds the configured limit")

// readLimitedRequestBody buffers r.Body under the ceiling from
// config.GetMaxRequestBodyBytes.
//
// Why this exists: the four customer-facing handlers each did a bare
// io.ReadAll(r.Body) with no ceiling, so one request could make the process
// allocate without bound — and because authenticate() returns (nil, nil) when
// requireApiKey is off (the live posture), that allocation is reachable with no
// credential at all. The admin surfaces in this repo already decided this
// question: 7 sites in handler.go plus admin_bot_api.go, kiro_apikey_admin.go
// and kiro_profiles_admin.go all wrap MaxBytesReader. The customer hot path was
// the one place that did not.
//
// Wrapping the reader (rather than trusting Content-Length) is deliberate and
// matches the convention already documented in admin_bot_api.go: a chunked or
// lying Content-Length cannot slip past a reader that counts actual bytes.
//
// The distinction from the admin helpers is that this returns the buffered bytes
// instead of decoding: every caller needs the raw body for a second purpose
// (token estimation, trace capture, upstream rewrite), so json.Decoder-over-body
// is not a drop-in here.
func readLimitedRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, nil
	}
	limit := config.GetMaxRequestBodyBytes()
	r.Body = http.MaxBytesReader(w, r.Body, int64(limit))
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// http.MaxBytesReader reports the overflow as *http.MaxBytesError
		// (Go 1.19+). Checking the type keeps a genuine network read error
		// reported as a read error rather than being mislabelled 413.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errRequestBodyTooLarge
		}
		return nil, err
	}
	return body, nil
}

// isRequestBodyTooLarge reports whether an error from readLimitedRequestBody was
// the ceiling rather than a transport fault. Callers use it to pick 413 over
// 400, in whichever error dialect that surface speaks.
func isRequestBodyTooLarge(err error) bool {
	return errors.Is(err, errRequestBodyTooLarge)
}
