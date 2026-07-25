package config

import (
	"path/filepath"
	"sync"
	"testing"
)

// Init assigned the package-global cfgPath directly, with no lock held, while
// Save() reads cfgPath (config.go:692) to decide where to write. Save is reached
// from detached goroutines — pool.UpdateStats persists account stats via
// `go config.UpdateAccountStats(...)` — so a concurrent Init and a background
// persist raced on cfgPath. The race detector caught this as:
//
//	Write at ... by goroutine N: config.Init()      config.go:500
//	Previous read at ... by goroutine M: config.Save() config.go:692
//
// Beyond being UB, the practical hazard is a background save reading a
// half-published path and writing the config to the wrong location (or an empty
// path). Every other cfgPath access already happens under cfgLock; Init was the
// lone unsynchronised writer.
//
// Run with -race: without the fix this reports a data race on cfgPath.
func TestInitIsRaceFreeWithConcurrentSave(t *testing.T) {
	dir := t.TempDir()
	if err := Init(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("initial init: %v", err)
	}
	if err := AddAccount(Account{ID: "a", Enabled: true}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Background persists, exactly as pool.UpdateStats does.
	go func() {
		defer wg.Done()
		for i := 1; i <= 200; i++ {
			_ = UpdateAccountStats("a", i, 0, i*10, float64(i), int64(i))
		}
	}()

	// Concurrent re-initialisation against fresh paths.
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = Init(filepath.Join(dir, "config.json"))
		}
	}()

	wg.Wait()
}

// Init must still publish the path it was given, so a later Save writes to the
// right file. This guards against "fixing" the race by dropping the assignment.
func TestInitPublishesPath(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "nested", "config.json")
	if err := Init(want); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := configPath(); got != want {
		t.Fatalf("cfgPath = %q, want %q", got, want)
	}
}
