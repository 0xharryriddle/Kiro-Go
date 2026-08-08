package proxy

import (
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	"strings"
	"sync"
	"time"
)

const tokenRefreshSkewSeconds int64 = 120

// tokenRefreshLock returns the per-account refresh mutex, creating it on first use.
func (h *Handler) tokenRefreshLock(accountID string) *sync.Mutex {
	m, _ := h.tokenRefreshLocks.LoadOrStore(accountID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): the fork's ensureValidToken used to
// start here and upstream inserted a whole new function, refreshAccountToken, at
// the same spot. Upstream's function is kept because it fixes a real ordering bug:
// it re-reads the latest persisted credential under the lock, PERSISTS a rotated
// refresh token, and only then publishes it to the runtime pool, so a crash
// between publish and save can no longer strand the pool holding a token that is
// not on disk. The fork's credential-kind guards were not lost — they now live in
// the surviving ensureValidToken below.
// refreshAccountToken serializes the complete refresh-token rotation lifecycle:
// load the latest persisted credential, refresh it, persist any rotation, and
// only then publish it to the runtime pool. A single lock is intentionally used
// across accounts because refreshes are rare and this keeps every refresh entry
// point consistent.
func (h *Handler) refreshAccountToken(account *config.Account, force bool) (bool, error) {
	if account == nil || strings.TrimSpace(account.ID) == "" {
		return false, fmt.Errorf("account is required for token refresh")
	}

	mu := h.tokenRefreshLock(account.ID)
	mu.Lock()
	defer mu.Unlock()

	var latest *config.Account
	accounts := config.GetAccounts()
	for i := range accounts {
		if accounts[i].ID == account.ID {
			latest = &accounts[i]
			break
		}
	}
	if latest == nil {
		return false, fmt.Errorf("account %s no longer exists", account.ID)
	}
	working := *latest

	// API Key credentials never expire and cannot be OAuth-refreshed.
	if config.IsAPIKeyAccount(&working) {
		token := strings.TrimSpace(working.KiroApiKey)
		if token == "" {
			token = strings.TrimSpace(working.AccessToken)
		}
		if token == "" {
			return false, fmt.Errorf("account %s has no kiroApiKey", working.ID)
		}
		h.pool.UpdateCredentialState(
			account,
			working.ID,
			token,
			"",
			0,
			"",
		)
		return false, nil
	}

	if !force && (working.ExpiresAt == 0 || time.Now().Unix() < working.ExpiresAt-tokenRefreshSkewSeconds) {
		h.pool.UpdateCredentialState(
			account,
			working.ID,
			working.AccessToken,
			working.RefreshToken,
			working.ExpiresAt,
			working.ProfileArn,
		)
		return false, nil
	}
	if strings.TrimSpace(working.RefreshToken) == "" {
		return false, fmt.Errorf("account %s has no refresh token", working.ID)
	}

	// A forced refresh must actually reach the IdP: the fork's RefreshToken
	// short-circuits on an unexpired stored token, which would make every
	// operator-triggered refresh a silent no-op.
	refresh := auth.RefreshToken
	if force {
		refresh = auth.RefreshTokenForce
	}
	accessToken, refreshToken, expiresAt, profileArn, err := refresh(&working)
	if err != nil {
		return false, err
	}
	if refreshToken == "" {
		refreshToken = working.RefreshToken
	}

	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union with one correction.
	// Upstream's persist-before-publish ordering is kept (a rotated credential must
	// be durable before the pool can hand it out), and the fork's in-memory field
	// updates are kept so the caller's *account reflects the refresh.
	//
	// The profileArn is NOT passed to UpdateAccountCredentialState: that function
	// writes any non-empty ARN unconditionally (config.go:1788), which would bypass
	// acceptRefreshedProfileArn and silently move a MANUALLY PINNED account, or cache
	// an ARN from outside a region override. It is gated here instead;
	// acceptRefreshedProfileArn persists it itself when the ARN is acceptable.
	if err := config.UpdateAccountCredentialState(
		working.ID,
		accessToken,
		refreshToken,
		expiresAt,
		"",
	); err != nil {
		return false, fmt.Errorf("persist refreshed token for account %s: %w", working.ID, err)
	}

	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	acceptedProfileArn := ""
	if acceptRefreshedProfileArn(account, profileArn) {
		acceptedProfileArn = strings.TrimSpace(profileArn)
	}

	// Do not expose a rotated credential through the pool until persistence has
	// succeeded. This ordering prevents a later refresh from reading stale state.
	h.pool.UpdateCredentialState(
		account,
		working.ID,
		accessToken,
		refreshToken,
		expiresAt,
		acceptedProfileArn,
	)
	return true, nil
}

// ensureValidToken 确保 token 有效
func (h *Handler) ensureValidToken(account *config.Account) error {
	if config.IsAPIKeyAccount(account) {
		if accountBearerToken(account) == "" {
			return fmt.Errorf("account %s has no kiroApiKey", account.ID)
		}
		return nil
	}
	// Bedrock and custom_api accounts carry static credentials and no Kiro OAuth
	// material, so there is nothing to refresh. They are checked explicitly rather
	// than relying on ExpiresAt == 0, which is what used to cover them by accident
	// and is exactly why the zero case below could not be treated as "refresh due".
	if account.IsBedrock() || account.IsCustomApi() {
		return nil
	}

	// ExpiresAt == 0 on an OAuth account means the expiry is UNKNOWN, not that the
	// token never expires. It used to short-circuit as valid, so such an account
	// was never refreshed at request time and the background refresher skipped it
	// too (it only runs when ExpiresAt > 0). The supplied access token then expired
	// on its own ~1h schedule while a perfectly good refresh token sat unused, every
	// subsequent request failed upstream, and the account was cooled down and
	// eventually banned — with the credential that would have fixed it already in
	// hand.
	//
	// Refresh is attempted only when the material to do it exists; without a refresh
	// token there is nothing to try and failing here would take an account offline
	// that might still be serving.
	if account.ExpiresAt == 0 {
		if strings.TrimSpace(account.RefreshToken) == "" {
			return nil
		}
		_, err := h.refreshAccountToken(account, false)
		return err
	}

	if time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
		return nil
	}

	_, err := h.refreshAccountToken(account, false)
	return err
}
