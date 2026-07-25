package pool

import (
	"sync"
	"testing"

	"kiro-go/config"
)

// newTestPool is defined in account_test.go and reused here.

// The routing getters used to return &p.accounts[idx] — a pointer directly into
// pool storage. Callers then mutated it with NO pool lock held (proxy's
// ensureValidToken assigns AccessToken/RefreshToken/ExpiresAt on the returned
// account), while UpdateToken writes those same fields under p.mu.Lock. That is
// a genuine data race on p.accounts[i].AccessToken, confirmed by the race
// detector before this fix.
//
// Returning a copy is safe because no caller relies on the alias for write-back:
// every mutation site also calls pool.UpdateToken (locked) and
// config.UpdateAccountToken (durable), and Reload() rebuilds pool storage from
// config. The alias was in fact an unreliable write path already — weighted
// accounts appear as N independent copies in p.accounts, so mutating one alias
// only ever updated one of them.
func TestRoutingGettersReturnCopiesNotPoolAliases(t *testing.T) {
	cases := []struct {
		name string
		get  func(p *AccountPool) *config.Account
	}{
		{"GetNext", func(p *AccountPool) *config.Account { return p.GetNext() }},
		{"GetNextExcluding", func(p *AccountPool) *config.Account { return p.GetNextExcluding(nil) }},
		{"GetByID", func(p *AccountPool) *config.Account { return p.GetByID("a") }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newTestPool(config.Account{ID: "a", Enabled: true, AccessToken: "orig"})

			got := c.get(p)
			if got == nil {
				t.Fatalf("%s returned nil", c.name)
			}
			got.AccessToken = "mutated-by-caller"

			if p.accounts[0].AccessToken != "orig" {
				t.Fatalf("%s leaked a pointer into pool storage: pool token became %q",
					c.name, p.accounts[0].AccessToken)
			}
		})
	}
}

// Same guarantee for the model-scoped getters, which are the ones the request
// handlers actually use (GetNextForModelExcluding on every request).
func TestModelRoutingGettersReturnCopies(t *testing.T) {
	p := newTestPool(config.Account{ID: "a", Enabled: true, AccessToken: "orig"})
	p.SetModelList("a", []string{"claude-opus-5"})

	for _, name := range []string{"GetNextForModel", "GetNextForModelExcluding"} {
		var got *config.Account
		if name == "GetNextForModel" {
			got = p.GetNextForModel("claude-opus-5")
		} else {
			got = p.GetNextForModelExcluding("claude-opus-5", nil)
		}
		if got == nil {
			t.Fatalf("%s returned nil", name)
		}
		got.AccessToken = "mutated-by-caller"
		if p.accounts[0].AccessToken != "orig" {
			t.Fatalf("%s leaked a pointer into pool storage: pool token became %q",
				name, p.accounts[0].AccessToken)
		}
	}
}

// Reproduces the exact concurrent shape that tripped the race detector: one
// goroutine acquiring an account and mutating it the way ensureValidToken does,
// another calling UpdateToken under the write lock. Run with -race.
func TestConcurrentAcquireAndUpdateTokenIsRaceFree(t *testing.T) {
	p := newTestPool(config.Account{ID: "a", Enabled: true, AccessToken: "old"})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			if acc := p.GetNext(); acc != nil {
				acc.AccessToken = "written-by-caller"
				acc.ExpiresAt = int64(i)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			p.UpdateToken("a", "written-by-pool", "r", int64(i))
		}
	}()
	wg.Wait()
}

// A copy must still carry every field the caller needs, otherwise returning a
// copy would silently break routing (e.g. a lost ProfileArn would send the
// request to the wrong data plane).
func TestReturnedCopyPreservesRoutingFields(t *testing.T) {
	want := config.Account{
		ID:            "a",
		Enabled:       true,
		Email:         "user@example.com",
		AccessToken:   "tok",
		RefreshToken:  "ref",
		ExpiresAt:     1234,
		ProfileArn:    "arn:aws:codewhisperer:eu-central-1:1:profile/X",
		ProfilePinned: true,
		AuthMethod:    "idc",
		Region:        "us-east-1",
	}
	p := newTestPool(want)

	got := p.GetNext()
	if got == nil {
		t.Fatal("GetNext returned nil")
	}
	// config.Account contains a slice field, so compare the routing-relevant
	// scalars individually rather than the whole struct.
	if got.ID != want.ID ||
		got.Email != want.Email ||
		got.AccessToken != want.AccessToken ||
		got.RefreshToken != want.RefreshToken ||
		got.ExpiresAt != want.ExpiresAt ||
		got.ProfileArn != want.ProfileArn ||
		got.ProfilePinned != want.ProfilePinned ||
		got.AuthMethod != want.AuthMethod ||
		got.Region != want.Region ||
		got.Enabled != want.Enabled {
		t.Fatalf("returned copy lost fields:\n got: %+v\nwant: %+v", *got, want)
	}
}
