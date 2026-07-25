package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

// Bodies are stored gzipped under a date-partitioned directory keyed by trace ID,
// and the record must reference them by RELATIVE path: an absolute filesystem
// path in an API response leaks server layout.
func TestWriteTraceBodyStoresGzipAndReturnsRelativeRef(t *testing.T) {
	dir := t.TempDir()
	store := newTraceBodyStore(dir)

	ref, truncated, err := store.Write("trc_abc123", []byte(`{"prompt":"hello"}`), []byte(`{"reply":"hi"}`), 0)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if truncated {
		t.Fatal("small bodies must not be flagged as truncated")
	}
	if ref == "" {
		t.Fatal("expected a body ref")
	}
	if filepath.IsAbs(ref) {
		t.Fatalf("body ref must be relative, got %q", ref)
	}
	if !strings.HasSuffix(ref, ".json.gz") {
		t.Fatalf("expected a gzipped ref, got %q", ref)
	}

	// The file must exist on disk and not be world-readable.
	full := filepath.Join(dir, ref)
	info, err := os.Stat(full)
	if err != nil {
		t.Fatalf("stat %s: %v", full, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("body file mode = %#o, must not be group/world readable", mode)
	}

	// And it must round-trip back through Read.
	got, err := store.Read(ref)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(got.Request), "hello") {
		t.Fatalf("request body did not round-trip: %s", got.Request)
	}
	if !strings.Contains(string(got.Response), "hi") {
		t.Fatalf("response body did not round-trip: %s", got.Response)
	}
}

func TestWriteTraceBodyTruncatesOversizeAndFlags(t *testing.T) {
	dir := t.TempDir()
	store := newTraceBodyStore(dir)

	big := []byte(strings.Repeat("A", 5000))
	ref, truncated, err := store.Write("trc_big", big, big, 1000)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !truncated {
		t.Fatal("oversize bodies must be flagged so a partial body is never mistaken for the whole payload")
	}
	got, err := store.Read(ref)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Request) > 1000 {
		t.Fatalf("request body not truncated: %d bytes", len(got.Request))
	}
}

// A traversal-shaped ref must never escape the body directory.
func TestReadTraceBodyRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	store := newTraceBodyStore(dir)

	secret := filepath.Join(dir, "..", "escaped.txt")
	if err := os.WriteFile(secret, []byte("should not be reachable"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, ref := range []string{
		"../escaped.txt",
		"../../etc/passwd",
		"/etc/passwd",
		"20260101/../../escaped.txt",
	} {
		if _, err := store.Read(ref); err == nil {
			t.Fatalf("ref %q must be rejected as path traversal", ref)
		}
	}
}

// Capture mode gates persistence: in meta mode nothing is written at all.
func TestCaptureBodiesRespectsMode(t *testing.T) {
	dir := t.TempDir()
	store := newTraceBodyStore(dir)

	ref, _, err := store.Capture(config.TraceCaptureMeta, "trc_meta", []byte(`{"prompt":"secret"}`), []byte(`{}`), 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if ref != "" {
		t.Fatalf("meta mode must not persist bodies, got ref %q", ref)
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "*", "*"))
	if len(entries) != 0 {
		t.Fatalf("meta mode wrote files to disk: %v", entries)
	}

	ref, _, err = store.Capture(config.TraceCaptureRedacted, "trc_red", []byte(`{"prompt":"a@b.com"}`), []byte(`{}`), 0)
	if err != nil {
		t.Fatalf("capture redacted: %v", err)
	}
	if ref == "" {
		t.Fatal("redacted mode must persist a body")
	}
	got, err := store.Read(ref)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(got.Request), "a@b.com") {
		t.Fatalf("redacted mode kept raw PII: %s", got.Request)
	}
}

// Pruning removes whole day directories beyond retention.
func TestTraceBodyStorePrunesOldDays(t *testing.T) {
	dir := t.TempDir()
	store := newTraceBodyStore(dir)

	old := filepath.Join(dir, "20200101")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(old, "trc_old.json.gz"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := store.Write("trc_new", []byte(`{}`), []byte(`{}`), 0); err != nil {
		t.Fatalf("write: %v", err)
	}

	store.Prune(24, timeNowUTC())

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expected old day directory to be pruned, err=%v", err)
	}
	// Today's directory must survive.
	today, _ := filepath.Glob(filepath.Join(dir, "*", "trc_new.json.gz"))
	if len(today) != 1 {
		t.Fatalf("today's bodies must survive pruning, got %v", today)
	}
}
