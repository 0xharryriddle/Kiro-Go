package proxy

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kiro-go/logger"
)

const (
	// traceStoreQueueSize bounds the in-flight record queue. Beyond this the
	// store drops rather than blocking, because observability must never
	// throttle request serving.
	traceStoreQueueSize = 4096
	// traceIndexPrefix names the rotated per-day index files.
	traceIndexPrefix = "index-"
	traceIndexSuffix = ".jsonl"
)

// traceStore appends trace records as JSONL.
//
// Rationale: the previous implementation re-serialised the ENTIRE retained slice
// with MarshalIndent on every single request and wrote it through one shared
// temp path (data/request_logs.json.tmp) from a fresh goroutine per append. That
// is O(n) work per request, and concurrent writers raced on the same temp file —
// the rename was atomic but the temp file was not.
//
// Here a single writer goroutine owns the file handle. Producers hand records to
// a buffered channel and never block: when the queue is full the record is
// dropped and counted, and the counter is surfaced in the metrics summary so
// silent loss is visible rather than mysterious.
type traceStore struct {
	dir string

	ch   chan traceStoreMsg
	done chan struct{}

	dropped atomic.Int64
	written atomic.Int64

	mu       sync.Mutex
	file     *os.File
	writer   *bufio.Writer
	fileDate string

	closeOnce sync.Once
	wg        sync.WaitGroup
}

// traceStoreMsg is either a record to write or a flush barrier. Both travel the
// SAME queue, which is what makes Flush a real barrier: it cannot be observed
// as complete while an earlier record is still in flight. A previous version
// polled len(ch)==0 instead, which returned while a record was dequeued but not
// yet written — a race the -race detector caught as a missing index file.
type traceStoreMsg struct {
	entry   RequestLog
	flushed chan struct{}
}

// newTraceStore starts a store rooted at dir. queueSize <= 0 uses the default.
func newTraceStore(dir string, queueSize int) *traceStore {
	if queueSize <= 0 {
		queueSize = traceStoreQueueSize
	}
	s := &traceStore{
		dir:  dir,
		ch:   make(chan traceStoreMsg, queueSize),
		done: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.run()
	return s
}

// Append enqueues a record. It never blocks: a full queue increments the dropped
// counter instead of stalling the request path.
func (s *traceStore) Append(entry RequestLog) {
	if s == nil {
		return
	}
	select {
	case s.ch <- traceStoreMsg{entry: entry}:
	default:
		s.dropped.Add(1)
	}
}

// Dropped reports how many records were discarded because the queue was full.
func (s *traceStore) Dropped() int64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

// Written reports how many records reached disk.
func (s *traceStore) Written() int64 {
	if s == nil {
		return 0
	}
	return s.written.Load()
}

// fileNameFor returns the index path for a timestamp. Rotation is by UTC date so
// operators comparing traces across hosts see the same boundaries.
func (s *traceStore) fileNameFor(t time.Time) string {
	return filepath.Join(s.dir, traceIndexPrefix+t.UTC().Format("20060102")+traceIndexSuffix)
}

// run is the single writer goroutine.
func (s *traceStore) run() {
	defer s.wg.Done()
	for {
		select {
		case msg, ok := <-s.ch:
			if !ok {
				s.closeFile()
				return
			}
			s.handle(msg)
		case <-s.done:
			// Drain whatever is queued so a shutdown does not lose records.
			for {
				select {
				case msg := <-s.ch:
					s.handle(msg)
				default:
					s.closeFile()
					return
				}
			}
		}
	}
}

// handle processes one queued message: either write a record, or satisfy a
// flush barrier by syncing buffers and signalling the waiter.
func (s *traceStore) handle(msg traceStoreMsg) {
	if msg.flushed != nil {
		s.mu.Lock()
		if s.writer != nil {
			_ = s.writer.Flush()
		}
		s.mu.Unlock()
		close(msg.flushed)
		return
	}
	s.writeOne(msg.entry)
}

func (s *traceStore) writeOne(entry RequestLog) {
	raw, err := json.Marshal(entry)
	if err != nil {
		logger.Warnf("[Trace] failed to encode record: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureFileLocked(time.Now()); err != nil {
		logger.Warnf("[Trace] failed to open trace file: %v", err)
		return
	}
	// One record per line: a reader can recover every intact line even if the
	// process dies mid-write, which a single large JSON array cannot do.
	if _, err := s.writer.Write(raw); err != nil {
		logger.Warnf("[Trace] write failed: %v", err)
		return
	}
	if err := s.writer.WriteByte('\n'); err != nil {
		logger.Warnf("[Trace] write failed: %v", err)
		return
	}
	s.written.Add(1)
}

// ensureFileLocked opens or rotates the index file. Caller holds s.mu.
func (s *traceStore) ensureFileLocked(now time.Time) error {
	date := now.UTC().Format("20060102")
	if s.file != nil && s.fileDate == date {
		return nil
	}
	if s.file != nil {
		_ = s.writer.Flush()
		_ = s.file.Close()
		s.file = nil
		s.writer = nil
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	// 0600: trace records can carry prompt text once body capture is enabled.
	f, err := os.OpenFile(s.fileNameFor(now), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	s.file = f
	s.writer = bufio.NewWriterSize(f, 64*1024)
	s.fileDate = date
	return nil
}

// Flush blocks until every queued record has been written and buffers are
// synced. Used by tests and shutdown.
func (s *traceStore) Flush() {
	if s == nil {
		return
	}
	// Send a barrier through the record queue and wait for the writer to reach
	// it. Because it uses the same channel, every previously enqueued record is
	// necessarily written first.
	done := make(chan struct{})
	select {
	case s.ch <- traceStoreMsg{flushed: done}:
	case <-s.done:
		return
	}
	select {
	case <-done:
	case <-s.done:
	}
}

func (s *traceStore) closeFile() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer != nil {
		_ = s.writer.Flush()
	}
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
		s.writer = nil
	}
}

// Close stops the writer goroutine after draining the queue.
func (s *traceStore) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.done)
		s.wg.Wait()
	})
}

// Prune removes whole index files older than retentionHours. Pruning by file
// rather than by record keeps it O(files) and avoids rewriting live data.
func (s *traceStore) Prune(retentionHours int, now time.Time) {
	if s == nil || retentionHours <= 0 {
		return
	}
	cutoff := now.UTC().Add(-time.Duration(retentionHours) * time.Hour)
	entries, err := filepath.Glob(filepath.Join(s.dir, traceIndexPrefix+"*"+traceIndexSuffix))
	if err != nil {
		return
	}
	for _, path := range entries {
		day, ok := traceIndexDate(path)
		if !ok {
			continue
		}
		// Compare end-of-day so today's file is never pruned by a sub-day window.
		if day.Add(24 * time.Hour).Before(cutoff) {
			s.mu.Lock()
			active := s.file != nil && s.fileNameFor(now) == path
			s.mu.Unlock()
			if active {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				logger.Warnf("[Trace] failed to prune %s: %v", path, err)
			}
		}
	}
}

// traceIndexDate parses the UTC date encoded in an index filename.
func traceIndexDate(path string) (time.Time, bool) {
	base := filepath.Base(path)
	base = strings.TrimPrefix(base, traceIndexPrefix)
	base = strings.TrimSuffix(base, traceIndexSuffix)
	t, err := time.ParseInLocation("20060102", base, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// LoadRecent returns up to limit records, newest last, read from the most recent
// index files. When legacyPath points at a pre-upgrade data/request_logs.json it
// is imported too and then renamed with a .migrated suffix so the import happens
// exactly once.
func (s *traceStore) LoadRecent(limit int, legacyPath string) []RequestLog {
	if s == nil || limit <= 0 {
		return nil
	}
	var records []RequestLog

	// Legacy single-array file from before the JSONL store. The records are
	// written INTO the JSONL store rather than merely returned, because the
	// legacy file is renamed straight afterwards: without re-persisting, the
	// imported history would live only in memory and vanish on the next
	// restart, making the upgrade look like silent data loss.
	//
	// They are deliberately NOT appended to `records` here — the glob below
	// reads them back from the index, and appending as well would double every
	// migrated row.
	if strings.TrimSpace(legacyPath) != "" {
		if raw, err := os.ReadFile(legacyPath); err == nil {
			var legacy []RequestLog
			if err := json.Unmarshal(raw, &legacy); err == nil {
				for _, rec := range legacy {
					s.writeOne(rec)
				}
				s.Flush()
				if err := os.Rename(legacyPath, legacyPath+".migrated"); err != nil {
					logger.Warnf("[Trace] imported %s but could not rename it: %v", legacyPath, err)
				}
			} else {
				logger.Warnf("[Trace] failed to parse legacy %s: %v", legacyPath, err)
			}
		}
	}

	files, err := filepath.Glob(filepath.Join(s.dir, traceIndexPrefix+"*"+traceIndexSuffix))
	if err == nil {
		sort.Strings(files) // lexical order == chronological for YYYYMMDD
		for _, path := range files {
			records = append(records, readTraceIndexFile(path)...)
		}
	}

	// Migrated rows are appended to TODAY's index regardless of their original
	// timestamps, so file order alone is not chronological after an upgrade.
	// Sort by record time (stable, so same-second rows keep write order) before
	// applying the limit, or the newest-N window would drop the wrong rows.
	sort.SliceStable(records, func(i, j int) bool { return records[i].Time < records[j].Time })

	if len(records) > limit {
		records = records[len(records)-limit:]
	}
	return records
}

// readTraceIndexFile parses one JSONL index file, skipping unparseable lines so
// a single torn tail line cannot hide an entire day of history.
func readTraceIndexFile(path string) []RequestLog {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []RequestLog
	scanner := bufio.NewScanner(f)
	// Records can be large once body refs and attempts are present.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec RequestLog
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}
