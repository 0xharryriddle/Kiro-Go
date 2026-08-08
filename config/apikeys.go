package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"time"
)

// HashApiKey returns the hex SHA-256 of the (trimmed) secret. This is the only
// credential material stored at rest; plaintext keys are never persisted. SHA-256
// is appropriate here (not bcrypt/argon2) because API keys are high-entropy random
// 32-byte tokens, not low-entropy user passwords — a fast hash with constant-time
// comparison defeats both extraction-from-disk and timing attacks.
func HashApiKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// apiKeyHashMatches constant-time compares a provided key's hash against a stored
// hash, avoiding a timing side-channel on the credential.
func apiKeyHashMatches(providedKey, storedHash string) bool {
	if storedHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashApiKey(providedKey)), []byte(storedHash)) == 1
}

// ListApiKeys returns a snapshot of all configured API key entries.
func ListApiKeys() []ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]ApiKeyEntry, len(cfg.ApiKeys))
	copy(out, cfg.ApiKeys)
	return out
}

// GetApiKeyEntry returns a copy of the entry with the given ID, or nil if not found.
func GetApiKeyEntry(id string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cp := cfg.ApiKeys[i]
			return &cp
		}
	}
	return nil
}

// AddApiKey appends a new API key entry. Generates ID and CreatedAt if missing,
// rejects empty Key values, and refuses duplicates of an existing Key.
func AddApiKey(entry ApiKeyEntry) (ApiKeyEntry, error) {
	entries, err := AddApiKeys([]ApiKeyEntry{entry})
	if err != nil {
		return ApiKeyEntry{}, err
	}
	return entries[0], nil
}

func AddApiKeys(entries []ApiKeyEntry) ([]ApiKeyEntry, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return nil, errors.New("config not initialized")
	}
	if len(entries) == 0 {
		return nil, errors.New("no api keys provided")
	}

	// MERGE POLICY NOTE (fork ↔ hian699): upstream's batch shape (many keys per
	// call, all-or-nothing rollback) is kept, but its de-duplication and storage
	// were plaintext-based: it wrote entry.Key straight into cfg.ApiKeys and
	// compared against existing.Key. This fork stores keys HASHED at rest
	// (KeyHash + display-only KeyMask, Key cleared — see config.go's ApiKeyEntry
	// and the plaintext→hash migration on load), so `existing.Key` is "" for every
	// stored entry and a plaintext dedupe check would silently never match while
	// re-persisting the secret. Dedupe therefore compares HASHES, and the
	// plaintext is returned to the caller but never stored.
	seen := make(map[string]bool, len(cfg.ApiKeys)+len(entries))
	for _, existing := range cfg.ApiKeys {
		if existing.KeyHash != "" {
			seen[existing.KeyHash] = true
		}
	}

	now := time.Now().Unix()
	// out carries the plaintext back to the caller (the one-time create response
	// that admin_apikeys.go / admin_bot_api.go hand to the buyer); stored holds
	// the at-rest copies with the plaintext cleared.
	out := make([]ApiKeyEntry, len(entries))
	stored := make([]ApiKeyEntry, len(entries))
	for i, entry := range entries {
		entry.Key = strings.TrimSpace(entry.Key)
		if entry.Key == "" {
			return nil, errors.New("api key value must not be empty")
		}
		hash := HashApiKey(entry.Key)
		if seen[hash] {
			return nil, errors.New("api key already exists")
		}
		seen[hash] = true
		if entry.ID == "" {
			entry.ID = newUUID()
		}
		if entry.CreatedAt == 0 {
			entry.CreatedAt = now
		}
		// Bound accounts are optional: an empty set means the key routes through the
		// shared pool. When set, drop unknown/duplicate ids so only live accounts remain.
		entry.BoundAccountIDs = sanitizeBoundAccountIDsLocked(entry.BoundAccountIDs)
		entry.Models = sanitizeModelList(entry.Models)
		entry.Model = ""
		entry.KeyHash = hash
		entry.KeyMask = MaskApiKey(entry.Key)
		out[i] = entry

		atRest := entry
		atRest.Key = "" // never store plaintext at rest
		stored[i] = atRest
	}

	oldLen := len(cfg.ApiKeys)
	cfg.ApiKeys = append(cfg.ApiKeys, stored...)
	if err := saveLocked(); err != nil {
		cfg.ApiKeys = cfg.ApiKeys[:oldLen]
		return nil, err
	}
	return out, nil
}

// UpdateApiKey applies a patch to an existing API key. Patch semantics:
//   - Name, Key are overwritten when non-empty in patch.
//   - Enabled, TokenLimit, CreditLimit are always overwritten (zero values are valid).
//   - Counters (TokensUsed/CreditsUsed/RequestsCount) are not touched here; use
//     RecordApiKeyUsage or ResetApiKeyUsage instead.
//   - Migrated stays as-is once true; only flips when explicitly set in patch.
func UpdateApiKey(id string, patch ApiKeyEntry) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	idx := -1
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("api key not found")
	}
	if patch.Name != "" {
		cfg.ApiKeys[idx].Name = patch.Name
	}
	if patch.Key != "" {
		newKey := strings.TrimSpace(patch.Key)
		newHash := HashApiKey(newKey)
		// Reject duplicates against any other entry (compare hashes at rest).
		for j := range cfg.ApiKeys {
			if j != idx && cfg.ApiKeys[j].KeyHash == newHash {
				return errors.New("api key value collides with existing entry")
			}
		}
		cfg.ApiKeys[idx].KeyHash = newHash
		cfg.ApiKeys[idx].KeyMask = MaskApiKey(newKey)
		cfg.ApiKeys[idx].Key = "" // never store plaintext at rest
	}
	cfg.ApiKeys[idx].Enabled = patch.Enabled
	cfg.ApiKeys[idx].TokenLimit = patch.TokenLimit
	cfg.ApiKeys[idx].CreditLimit = patch.CreditLimit
	cfg.ApiKeys[idx].ExpiresAt = patch.ExpiresAt
	cfg.ApiKeys[idx].RPMLimit = patch.RPMLimit
	cfg.ApiKeys[idx].IPLimit = patch.IPLimit
	cfg.ApiKeys[idx].IPAllowlist = patch.IPAllowlist
	cfg.ApiKeys[idx].TPMLimit = patch.TPMLimit
	// Bound accounts are always overwritten from the patch (sanitized). Empty = the key
	// falls back to shared-pool routing; a non-empty set restricts it to those accounts.
	cfg.ApiKeys[idx].BoundAccountIDs = sanitizeBoundAccountIDsLocked(patch.BoundAccountIDs)
	cfg.ApiKeys[idx].Models = sanitizeModelList(patch.Models)
	cfg.ApiKeys[idx].Model = ""
	if patch.Migrated {
		cfg.ApiKeys[idx].Migrated = true
	}
	return saveLocked()
}

// DeleteApiKey removes the API key entry with the given ID. Returns nil even if
// the ID is unknown (idempotent), matching the existing DeleteAccount style.
func DeleteApiKey(id string) error {
	_, err := DeleteApiKeys([]string{id})
	return err
}

func DeleteApiKeys(ids []string) (int, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return 0, errors.New("config not initialized")
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return 0, errors.New("no api key ids provided")
	}
	original := append([]ApiKeyEntry(nil), cfg.ApiKeys...)
	kept := cfg.ApiKeys[:0]
	deleted := 0
	for _, e := range cfg.ApiKeys {
		if want[e.ID] {
			deleted++
			continue
		}
		kept = append(kept, e)
	}
	cfg.ApiKeys = kept
	if deleted == 0 {
		return 0, nil
	}
	if err := saveLocked(); err != nil {
		cfg.ApiKeys = original
		return deleted, err
	}
	return deleted, nil
}

// FindApiKeyByValue returns a copy of the entry whose stored hash matches the
// hash of the given value, or nil if no match. Comparison is constant-time to
// avoid a timing side-channel. O(n) linear scan.
func FindApiKeyByValue(key string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || key == "" {
		return nil
	}
	for i := range cfg.ApiKeys {
		if apiKeyHashMatches(key, cfg.ApiKeys[i].KeyHash) {
			cp := cfg.ApiKeys[i]
			return &cp
		}
	}
	return nil
}

// HasApiKeys returns true when at least one API key entry is configured.
func HasApiKeys() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return len(cfg.ApiKeys) > 0
}

// RecordApiKeyUsage atomically adds tokens and credits to the entry's counters,
// updates LastUsedAt, increments RequestsCount, attributes the usage to the given
// model (F10; empty model is recorded under "unknown"), and persists.
//
// Quota enforcement: when the updated counters reach a configured limit
// (token or credit), the key is deactivated (Enabled=false) in the same write.
// This is the "sold quota" contract used by the Telegram bot flow — a key sold
// with N credits stops working permanently once N credits are consumed, and
// stays off until an admin re-enables it (e.g. after a top-up).
func RecordApiKeyUsage(id string, tokens int64, credits float64, model string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			if tokens > 0 {
				cfg.ApiKeys[i].TokensUsed += tokens
				cfg.ApiKeys[i].LifetimeTokens += tokens
			}
			if credits > 0 {
				cfg.ApiKeys[i].CreditsUsed += credits
				cfg.ApiKeys[i].LifetimeCredits += credits
			}
			cfg.ApiKeys[i].RequestsCount++
			cfg.ApiKeys[i].LifetimeRequests++
			cfg.ApiKeys[i].LastUsedAt = time.Now().Unix()

			// Per-model breakdown.
			mk := strings.TrimSpace(model)
			if mk == "" {
				mk = "unknown"
			}
			if cfg.ApiKeys[i].ModelUsage == nil {
				cfg.ApiKeys[i].ModelUsage = make(map[string]ApiKeyModelUsage)
			}
			mu := cfg.ApiKeys[i].ModelUsage[mk]
			mu.Requests++
			if tokens > 0 {
				mu.Tokens += tokens
			}
			if credits > 0 {
				mu.Credits += credits
			}
			cfg.ApiKeys[i].ModelUsage[mk] = mu

			// Auto-deactivate on quota exhaustion (see function comment).
			if overToken, overCredit := ApiKeyOverLimit(cfg.ApiKeys[i]); overToken || overCredit {
				cfg.ApiKeys[i].Enabled = false
			}
			return saveLocked()
		}
	}
	return errors.New("api key not found")
}

// ResetApiKeyUsage clears the current-period counters (TokensUsed/CreditsUsed/
// RequestsCount) for the entry, granting a fresh quota. Lifetime counters are left
// untouched so the grand total survives. LastUsedAt is preserved so operators can
// still see when the key was last used.
func ResetApiKeyUsage(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cfg.ApiKeys[i].TokensUsed = 0
			cfg.ApiKeys[i].CreditsUsed = 0
			cfg.ApiKeys[i].RequestsCount = 0
			cfg.ApiKeys[i].ModelUsage = nil
			return saveLocked()
		}
	}
	return errors.New("api key not found")
}

// RechargeApiKey additively increases an API key's limits (a customer buying
// more credits for an existing key they want to keep) and re-enables the key
// when the top-up brings its usage back under all configured limits.
//
// Amounts are ADDED, not set: a key sold with 100 credits, exhausted, then
// recharged by 50 ends with CreditLimit=150 (remaining = 150 - 100 = 50). Usage
// counters (CreditsUsed/TokensUsed) are preserved so lifetime accounting stays
// intact — this is the inverse of ResetApiKeyUsage, which the recharge flow
// intentionally does NOT use (a recharge tops up allowance; it does not wipe the
// buyer's consumption record).
//
// addCredits/addTokens must be >= 0 (validated by the caller); a zero amount
// leaves that limit untouched. Adding to a currently-unlimited limit (0) turns
// it metered at the added amount — the bot only recharges metered keys, so this
// is benign. Re-enable happens only when the key is under limit after the
// top-up: a partial recharge that still leaves usage over the (also-raised)
// limit stays disabled. Returns the updated entry.
//
// Overflow is rejected BEFORE anything is mutated, both directions:
//
//   - TokenLimit is int64 and `+=` wraps silently. A top-up near math.MaxInt64
//     drove the limit NEGATIVE, which makes ApiKeyOverLimit permanently true —
//     a paid top-up bricked the very key it was meant to extend.
//   - CreditLimit is float64 and saturates to +Inf. saveLocked then cannot
//     marshal the config at all, and because the in-memory cfg was already
//     mutated the damage outlived the failed call: EVERY later write (any
//     account edit, usage counter, ban stamp, new key) failed for the life of
//     the process. Validating before mutating is what makes a rejected recharge
//     leave no trace.
//
// Both are wire-reachable: math.MaxFloat64 and 9223372036854775807 are ordinary
// JSON numbers on POST /admin/recharge_api_key, whose only check is `>= 0`.
func RechargeApiKey(id string, addCredits float64, addTokens int64) (ApiKeyEntry, error) {
	// Reject non-finite input up front: NaN/±Inf arriving here would poison the
	// limit and every subsequent config save.
	if math.IsNaN(addCredits) || math.IsInf(addCredits, 0) {
		return ApiKeyEntry{}, errors.New("credit top-up must be a finite number")
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return ApiKeyEntry{}, errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			// Bounds-check against the CURRENT limits before touching them, so a
			// refused recharge mutates nothing.
			if addTokens > 0 && cfg.ApiKeys[i].TokenLimit > math.MaxInt64-addTokens {
				return ApiKeyEntry{}, errors.New("token top-up would overflow the key's token limit")
			}
			if addCredits > 0 {
				if sum := cfg.ApiKeys[i].CreditLimit + addCredits; math.IsInf(sum, 0) || math.IsNaN(sum) {
					return ApiKeyEntry{}, errors.New("credit top-up would overflow the key's credit limit")
				}
			}
			// Snapshot over-limit state BEFORE raising limits. Enabled=false has two
			// causes — auto-deactivation on exhaustion (RecordApiKeyUsage) and a
			// deliberate operator disable (UpdateApiKey / admin toggle, used for
			// abuse, chargebacks, disputed orders) — and the entry does not record
			// which. Being over limit while the top-up arrives is the only evidence
			// that exhaustion is what turned the key off, so it is the sole license
			// to switch it back on. Without this snapshot, a top-up on an
			// operator-quarantined key silently restored the credential: a banned
			// buyer could unban themselves by paying again.
			wasOverToken, wasOverCredit := ApiKeyOverLimit(cfg.ApiKeys[i])

			if addCredits > 0 {
				cfg.ApiKeys[i].CreditLimit += addCredits
			}
			if addTokens > 0 {
				cfg.ApiKeys[i].TokenLimit += addTokens
			}
			// Re-enable a key auto-deactivated on exhaustion now that the raised
			// limit puts it back under quota. No-op if already enabled; stays off if
			// still over limit after a partial top-up, and stays off if the key was
			// never over limit (an operator turned it off, so only an operator may
			// turn it back on). The limit increase applies either way — declining to
			// lift a quarantine must not discard paid-for allowance.
			if wasOverToken || wasOverCredit {
				if overToken, overCredit := ApiKeyOverLimit(cfg.ApiKeys[i]); !overToken && !overCredit {
					cfg.ApiKeys[i].Enabled = true
				}
			}
			if err := saveLocked(); err != nil {
				return ApiKeyEntry{}, err
			}
			return cfg.ApiKeys[i], nil
		}
	}
	return ApiKeyEntry{}, errors.New("api key not found")
}

// ResetApiKeyUsageAll clears BOTH the current-period counters and the lifetime counters
// for the entry, wiping all recorded usage as if the key were new. LastUsedAt is
// preserved. Use this for the "Reset All" action; use ResetApiKeyUsage for a routine
// per-cycle quota reset that keeps the grand total.
func ResetApiKeyUsageAll(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cfg.ApiKeys[i].TokensUsed = 0
			cfg.ApiKeys[i].CreditsUsed = 0
			cfg.ApiKeys[i].RequestsCount = 0
			cfg.ApiKeys[i].LifetimeTokens = 0
			cfg.ApiKeys[i].LifetimeCredits = 0
			cfg.ApiKeys[i].LifetimeRequests = 0
			return saveLocked()
		}
	}
	return errors.New("api key not found")
}

// sanitizeBoundAccountIDsLocked trims, de-duplicates, and drops unknown IDs from a
// key's bound-account list, keeping only IDs that match a currently-stored account.
// Order is preserved (first occurrence wins). MUST be called with cfgLock held,
// since it reads cfg.Accounts directly (the RWMutex is not reentrant).
func sanitizeBoundAccountIDsLocked(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	known := make(map[string]bool, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		known[a.ID] = true
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] || !known[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// sanitizeModelList trims, drops empties, and de-duplicates a key's model allowlist,
// preserving order (first occurrence wins). Returns nil for an empty result so the
// field is omitted from JSON. Model IDs are stored verbatim (as chosen in the UI);
// normalization to the canonical upstream name happens later in applyModelOverride.
func sanitizeModelList(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(models))
	out := make([]string, 0, len(models))
	for _, raw := range models {
		m := strings.TrimSpace(raw)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// GenerateApiKeyValue returns a new random 32-byte hex API key prefixed with "sk-".
func GenerateApiKeyValue() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return "sk-" + hex.EncodeToString(buf)
}

// ApiKeyDisplayMask returns the masked form to show in admin views. It prefers
// the stored KeyMask (set at create/update); for a legacy entry still carrying a
// plaintext Key (not yet migrated) it masks that as a fallback. Returns "" when
// neither is available.
func ApiKeyDisplayMask(e ApiKeyEntry) string {
	if e.KeyMask != "" {
		return e.KeyMask
	}
	if e.Key != "" {
		return MaskApiKey(e.Key)
	}
	return ""
}

// MaskApiKey produces a display-friendly masked version: keeps first 6 and last 4
// characters, replaces the middle with "****". Returns "" for empty input and
// the original string if it's too short to mask meaningfully.
func MaskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 6 {
		return key
	}
	return key[:3] + "***" + key[len(key)-3:]
}

// ApiKeyExpired reports whether the key has a set expiry (ExpiresAt > 0) that is now
// in the past. Keys with ExpiresAt == 0 never expire.
func ApiKeyExpired(e ApiKeyEntry) bool {
	return e.ExpiresAt > 0 && time.Now().Unix() >= e.ExpiresAt
}

// ApiKeyOverLimit returns (overToken, overCredit) for the entry. Limits with value 0
// are ignored. The function does not lock; callers should pass a copied entry.
func ApiKeyOverLimit(e ApiKeyEntry) (overToken bool, overCredit bool) {
	if e.TokenLimit > 0 && e.TokensUsed >= e.TokenLimit {
		overToken = true
	}
	if e.CreditLimit > 0 && e.CreditsUsed >= e.CreditLimit {
		overCredit = true
	}
	return
}
