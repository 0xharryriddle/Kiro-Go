package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// traceBodyPayload is the on-disk shape of one captured request/response pair.
type traceBodyPayload struct {
	TraceID    string          `json:"traceId"`
	CapturedAt int64           `json:"capturedAt"`
	Mode       string          `json:"mode"`
	Truncated  bool            `json:"truncated,omitempty"`
	Request    json.RawMessage `json:"request,omitempty"`
	Response   json.RawMessage `json:"response,omitempty"`
}

// traceBody is the decoded form returned to callers.
type traceBody struct {
	TraceID   string
	Mode      string
	Truncated bool
	Request   []byte
	Response  []byte
}

// traceBodyStore persists captured bodies as gzipped JSON, partitioned by UTC
// date so retention can be enforced by removing whole directories.
//
// Bodies are the most sensitive artefact this proxy can retain, so this type is
// deliberately narrow: every write goes through Capture (which enforces the
// configured mode and scrubs), files are 0600 under a 0700 directory, and reads
// resolve refs against the root to refuse path traversal.
type traceBodyStore struct {
	dir string
}

func newTraceBodyStore(dir string) *traceBodyStore {
	return &traceBodyStore{dir: dir}
}

// timeNowUTC is a seam so tests can reason about the current day boundary.
func timeNowUTC() time.Time { return time.Now().UTC() }

// Capture scrubs and persists bodies when mode permits, returning the relative
// ref and whether truncation occurred. In "off"/"meta" it writes nothing and
// returns an empty ref, so a caller cannot leak prompt text by forgetting to
// check the mode itself.
func (s *traceBodyStore) Capture(mode, traceID string, request, response []byte, maxBytes int) (string, bool, error) {
	if s == nil {
		return "", false, nil
	}
	scrubbedReq := scrubTraceBody(request, mode)
	scrubbedResp := scrubTraceBody(response, mode)
	if len(scrubbedReq) == 0 && len(scrubbedResp) == 0 {
		return "", false, nil
	}
	return s.write(mode, traceID, scrubbedReq, scrubbedResp, maxBytes)
}

// Write persists already-prepared bodies. Tests use it directly; production code
// should prefer Capture so mode enforcement and scrubbing are never skipped.
func (s *traceBodyStore) Write(traceID string, request, response []byte, maxBytes int) (string, bool, error) {
	return s.write(config.TraceCaptureFull, traceID, request, response, maxBytes)
}

func (s *traceBodyStore) write(mode, traceID string, request, response []byte, maxBytes int) (string, bool, error) {
	if s == nil {
		return "", false, nil
	}
	traceID = sanitizeTraceIDForPath(traceID)
	if traceID == "" {
		return "", false, fmt.Errorf("trace id is required")
	}

	req, reqTrunc := truncateTraceBody(request, maxBytes)
	resp, respTrunc := truncateTraceBody(response, maxBytes)
	truncated := reqTrunc || respTrunc

	payload := traceBodyPayload{
		TraceID:    traceID,
		CapturedAt: time.Now().Unix(),
		Mode:       mode,
		Truncated:  truncated,
		Request:    toRawJSON(req),
		Response:   toRawJSON(resp),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", false, err
	}

	day := timeNowUTC().Format("20060102")
	dayDir := filepath.Join(s.dir, day)
	if err := os.MkdirAll(dayDir, 0o700); err != nil {
		return "", false, err
	}

	ref := filepath.ToSlash(filepath.Join(day, traceID+".json.gz"))
	full := filepath.Join(s.dir, day, traceID+".json.gz")

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(encoded); err != nil {
		_ = zw.Close()
		return "", false, err
	}
	if err := zw.Close(); err != nil {
		return "", false, err
	}
	// 0600: captured bodies can contain prompt text.
	if err := os.WriteFile(full, buf.Bytes(), 0o600); err != nil {
		return "", false, err
	}
	return ref, truncated, nil
}

// Read decodes a stored body by its relative ref.
func (s *traceBodyStore) Read(ref string) (*traceBody, error) {
	if s == nil {
		return nil, fmt.Errorf("no body store")
	}
	full, err := s.resolve(ref)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	decoded, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	var payload traceBodyPayload
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return nil, err
	}
	return &traceBody{
		TraceID:   payload.TraceID,
		Mode:      payload.Mode,
		Truncated: payload.Truncated,
		Request:   fromRawJSON(payload.Request),
		Response:  fromRawJSON(payload.Response),
	}, nil
}

// resolve maps a relative ref to an absolute path, refusing anything that
// escapes the body root. Refs reach this function from an HTTP query parameter,
// so traversal defence is mandatory rather than defensive polish.
func (s *traceBodyStore) resolve(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("empty body ref")
	}
	if filepath.IsAbs(ref) || strings.HasPrefix(ref, "/") {
		return "", fmt.Errorf("body ref must be relative")
	}
	root, err := filepath.Abs(s.dir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, filepath.FromSlash(ref))
	cleaned := filepath.Clean(full)
	if cleaned != root && !strings.HasPrefix(cleaned, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("body ref escapes the trace directory")
	}
	return cleaned, nil
}

// Prune removes whole day directories older than retentionHours.
func (s *traceBodyStore) Prune(retentionHours int, now time.Time) {
	if s == nil || retentionHours <= 0 {
		return
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := now.UTC().Add(-time.Duration(retentionHours) * time.Hour)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		day, err := time.ParseInLocation("20060102", entry.Name(), time.UTC)
		if err != nil {
			continue
		}
		// Compare end-of-day so today is never pruned by a sub-day window.
		if day.Add(24 * time.Hour).Before(cutoff) {
			if err := os.RemoveAll(filepath.Join(s.dir, entry.Name())); err != nil {
				logger.Warnf("[Trace] failed to prune bodies for %s: %v", entry.Name(), err)
			}
		}
	}
}

// sanitizeTraceIDForPath keeps only characters safe in a filename, so a crafted
// trace ID can never influence the path.
func sanitizeTraceIDForPath(id string) string {
	id = strings.TrimSpace(id)
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// toRawJSON stores a body as JSON when it already is JSON, and as a JSON string
// otherwise, so a malformed upstream payload cannot corrupt the envelope.
func toRawJSON(body []byte) json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	quoted, err := json.Marshal(string(body))
	if err != nil {
		return nil
	}
	return json.RawMessage(quoted)
}

// fromRawJSON reverses toRawJSON: a JSON string round-trips back to its raw
// bytes, anything else is returned as-is.
func fromRawJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return []byte(asString)
	}
	return []byte(raw)
}
