package config

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestAddKiroAPIKeyAccountIfAbsentDeduplicatesIdentityAndRegion(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	first := Account{
		ID: "first", UserId: "user-1", AuthMethod: "api_key", KiroApiKey: "ksk_first",
		Region: "us-east-1", RegionOverride: "eu-central-1", Enabled: true,
	}
	if _, added, err := AddKiroAPIKeyAccountIfAbsent(first); err != nil || !added {
		t.Fatalf("add first: added=%v err=%v", added, err)
	}

	duplicate := first
	duplicate.ID = "duplicate"
	duplicate.KiroApiKey = "ksk_rotated"
	existing, added, err := AddKiroAPIKeyAccountIfAbsent(duplicate)
	if err != nil {
		t.Fatalf("dedup duplicate: %v", err)
	}
	if added || existing.ID != first.ID {
		t.Fatalf("expected existing %q, got added=%v existing=%+v", first.ID, added, existing)
	}

	otherRegion := first
	otherRegion.ID = "other-region"
	otherRegion.RegionOverride = "us-east-1"
	if _, added, err := AddKiroAPIKeyAccountIfAbsent(otherRegion); err != nil || !added {
		t.Fatalf("same identity in a different region should be allowed: added=%v err=%v", added, err)
	}
}

func TestAddKiroAPIKeyAccountIfAbsentFallsBackToKeyIdentity(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	first := Account{
		ID: "first", AuthMethod: "api_key", KiroApiKey: "ksk_shared",
		Region: "eu-central-1", RegionOverride: "eu-central-1", Enabled: true,
	}
	if _, added, err := AddKiroAPIKeyAccountIfAbsent(first); err != nil || !added {
		t.Fatalf("add first: added=%v err=%v", added, err)
	}
	second := first
	second.ID = "second"
	second.UserId = "later-discovered-user"
	existing, added, err := AddKiroAPIKeyAccountIfAbsent(second)
	if err != nil || added || existing.ID != first.ID {
		t.Fatalf("expected key fallback dedup, added=%v existing=%+v err=%v", added, existing, err)
	}
}

func TestAddKiroAPIKeyAccountIfAbsentIsAtomicUnderConcurrency(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	const workers = 12
	var wg sync.WaitGroup
	results := make(chan bool, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			account := Account{
				ID: GenerateMachineId(), UserId: "same-user", AuthMethod: "api_key",
				KiroApiKey: "ksk_same", Region: "us-east-1", RegionOverride: "us-east-1", Enabled: true,
			}
			_, added, err := AddKiroAPIKeyAccountIfAbsent(account)
			results <- added
			errs <- err
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)

	addedCount := 0
	for added := range results {
		if added {
			addedCount++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent add: %v", err)
		}
	}
	if addedCount != 1 {
		t.Fatalf("expected exactly one insertion, got %d", addedCount)
	}
	if got := len(GetAccounts()); got != 1 {
		t.Fatalf("expected one persisted account, got %d", got)
	}
}
