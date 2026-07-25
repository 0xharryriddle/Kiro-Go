package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Concurrent producers must never interleave partial records. The previous
// implementation re-serialised the whole 500-record slice on every request and
// wrote it through a single shared temp path, so concurrent writers raced on
// that file.
func TestTraceStoreConcurrentAppendsProduceValidLines(t *testing.T) {
	dir := t.TempDir()
	store := newTraceStore(dir, 128)
	defer store.Close()

	const n = 1000
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store.Append(RequestLog{
				Time:      time.Now().Unix(),
				Endpoint:  "claude",
				Status:    "success",
				RequestID: fmt.Sprintf("trc_%04d", i),
			})
		}(i)
	}
	wg.Wait()
	store.Flush()

	files, err := filepath.Glob(filepath.Join(dir, "index-*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("expected an index file, glob=%v err=%v", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	written := len(lines)
	if written == 0 {
		t.Fatal("no lines written")
	}
	// Every line must be independently parseable: a torn write would fail here.
	seen := map[string]bool{}
	for i, line := range lines {
		var rec RequestLog
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not valid JSON (torn write): %q", i, line)
		}
		if seen[rec.RequestID] {
			t.Fatalf("duplicate record %q", rec.RequestID)
		}
		seen[rec.RequestID] = true
	}
	if dropped := store.Dropped(); int(dropped)+written != n {
		t.Fatalf("written(%d) + dropped(%d) != %d", written, dropped, n)
	}
}

// Logging must never throttle serving: when the queue is full the store drops
// and counts, rather than blocking the request path.
func TestTraceStoreDropsInsteadOfBlocking(t *testing.T) {
	dir := t.TempDir()
	// Capacity 1 with no writer draining: appends beyond the buffer must not block.
	store := &traceStore{dir: dir, ch: make(chan traceStoreMsg, 1), done: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			store.Append(RequestLog{RequestID: fmt.Sprintf("trc_%d", i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Append blocked when the queue was full; logging must never throttle serving")
	}
	if store.Dropped() == 0 {
		t.Fatal("expected dropped records to be counted so silent loss is visible")
	}
}

// Trace files can contain prompts once body capture is on, so they must not be
// world-readable.
func TestTraceStoreFilesAreNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	store := newTraceStore(dir, 16)
	store.Append(RequestLog{RequestID: "trc_1", Endpoint: "claude"})
	store.Flush()
	store.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "index-*.jsonl"))
	if len(files) == 0 {
		t.Fatal("no index file")
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("file mode = %o, want 0600", perm)
	}
}

func TestTraceStorePrunesFilesBeyondRetention(t *testing.T) {
	dir := t.TempDir()
	store := newTraceStore(dir, 16)
	defer store.Close()

	// An old rotated file, and today's.
	oldName := filepath.Join(dir, "index-20200101.jsonl")
	if err := os.WriteFile(oldName, []byte("{}\n"), 0600); err != nil {
		t.Fatalf("seed old file: %v", err)
	}
	todayName := filepath.Join(dir, "index-"+time.Now().UTC().Format("20060102")+".jsonl")
	if err := os.WriteFile(todayName, []byte("{}\n"), 0600); err != nil {
		t.Fatalf("seed today file: %v", err)
	}

	store.Prune(24, time.Now())

	if _, err := os.Stat(oldName); !os.IsNotExist(err) {
		t.Fatal("expected the out-of-retention file to be pruned")
	}
	if _, err := os.Stat(todayName); err != nil {
		t.Fatalf("today's file must survive: %v", err)
	}
}

// Rotation is by UTC date, so a record written after midnight lands in a new file.
func TestTraceStoreRotatesByDate(t *testing.T) {
	dir := t.TempDir()
	store := newTraceStore(dir, 16)
	defer store.Close()

	day1 := time.Date(2026, 3, 1, 23, 59, 0, 0, time.UTC)
	day2 := time.Date(2026, 3, 2, 0, 1, 0, 0, time.UTC)
	if got := store.fileNameFor(day1); !strings.HasSuffix(got, "index-20260301.jsonl") {
		t.Fatalf("day1 file = %q", got)
	}
	if got := store.fileNameFor(day2); !strings.HasSuffix(got, "index-20260302.jsonl") {
		t.Fatalf("day2 file = %q", got)
	}
}

// Existing deployments have a data/request_logs.json file. Upgrading must not
// lose that history.
func TestTraceStoreLoadRecentIncludesLegacyFile(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "request_logs.json")
	legacyRecords := []RequestLog{
		{Time: 1, Endpoint: "claude", Model: "sonnet", Status: "success", Tokens: 5},
		{Time: 2, Endpoint: "openai", Status: "error", ErrorType: "quota", Error: "429"},
	}
	raw, _ := json.Marshal(legacyRecords)
	if err := os.WriteFile(legacy, raw, 0600); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	store := newTraceStore(filepath.Join(dir, "traces"), 16)
	defer store.Close()
	store.Append(RequestLog{Time: 3, Endpoint: "responses", Status: "success"})
	store.Flush()

	loaded := store.LoadRecent(500, legacy)
	if len(loaded) != 3 {
		t.Fatalf("expected 3 records (2 legacy + 1 new), got %d", len(loaded))
	}
	// Legacy file is renamed so it is imported exactly once.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("expected the legacy file to be renamed after import")
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Fatalf("expected a .migrated marker: %v", err)
	}
}
