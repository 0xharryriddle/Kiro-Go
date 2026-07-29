package config

import "testing"

// The customer-request ceiling has to behave correctly in three states, and the
// interesting one is the third: a knob that can be set to a hostile value is not
// a safety feature, it is a new outage switch. Unset must default, explicit must
// win, and too-small must be clamped UP rather than obeyed.
func TestMaxRequestBodyBytesDefaultsAndClamps(t *testing.T) {
	cfgLock.Lock()
	cfg = &Config{}
	cfgLock.Unlock()

	if got := GetMaxRequestBodyBytes(); got != 32<<20 {
		t.Fatalf("unset GetMaxRequestBodyBytes() = %d, want %d (32 MiB default)", got, 32<<20)
	}

	// An explicit, sane value wins outright.
	cfgLock.Lock()
	cfg = &Config{MaxRequestBodyBytes: 8 << 20}
	cfgLock.Unlock()
	if got := GetMaxRequestBodyBytes(); got != 8<<20 {
		t.Fatalf("explicit 8 MiB = %d, want %d", got, 8<<20)
	}

	// Zero and negative mean "unset", not "reject everything".
	for _, raw := range []int{0, -1, -999999} {
		cfgLock.Lock()
		cfg = &Config{MaxRequestBodyBytes: raw}
		cfgLock.Unlock()
		if got := GetMaxRequestBodyBytes(); got != 32<<20 {
			t.Fatalf("MaxRequestBodyBytes=%d gave %d, want the %d default", raw, got, 32<<20)
		}
	}

	// The clamp is the point of this test. The p50 prompt in the live corpus is
	// ~150k tokens, so a 1-byte or 1-KiB ceiling would refuse literally every
	// real request. Such a value is raised to the floor instead of honoured.
	for _, raw := range []int{1, 1024, (64 << 10) - 1} {
		cfgLock.Lock()
		cfg = &Config{MaxRequestBodyBytes: raw}
		cfgLock.Unlock()
		if got := GetMaxRequestBodyBytes(); got != 64<<10 {
			t.Fatalf("MaxRequestBodyBytes=%d gave %d, want the %d floor", raw, got, 64<<10)
		}
	}

	// Exactly at the floor is a legitimate explicit value, not a clamp case.
	cfgLock.Lock()
	cfg = &Config{MaxRequestBodyBytes: 64 << 10}
	cfgLock.Unlock()
	if got := GetMaxRequestBodyBytes(); got != 64<<10 {
		t.Fatalf("MaxRequestBodyBytes at the floor = %d, want %d", got, 64<<10)
	}
}

// A nil cfg must not panic. Handler.Close() already proved this class of bug is
// reachable (config.UpdateStats dereferenced a nil cfg during shutdown), and a
// getter consulted on the request path is exactly where a nil deref would be
// worst, so the guard is pinned by a test rather than left to convention.
func TestMaxRequestBodyBytesToleratesNilConfig(t *testing.T) {
	cfgLock.Lock()
	saved := cfg
	cfg = nil
	cfgLock.Unlock()
	defer func() {
		cfgLock.Lock()
		cfg = saved
		cfgLock.Unlock()
	}()

	if got := GetMaxRequestBodyBytes(); got != 32<<20 {
		t.Fatalf("nil cfg gave %d, want the %d default", got, 32<<20)
	}
}
