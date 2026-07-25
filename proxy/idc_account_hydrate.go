package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
	"strings"
)

// hydrateAccountAfterLogin resolves the profile ARN and upstream identity labels
// for a freshly persisted account, best effort.
//
// Why this exists: the interactive IAM Identity Center login knows only the SSO
// PORTAL region (e.g. us-east-1 for ssoins-*.us-east-1.portal.amazonaws.com). That
// is not necessarily the region hosting the tenant's CodeWhisperer profile. When
// the two differ, every identity call made at login time against the portal region
// fails with "User is not authorized to make this call" (403), because the request
// carries no profileArn and the portal region has no profile at all. The account
// therefore lands with a blank email, a blank userId, and no profileArn, which the
// admin UI renders as an unidentified account with no profile.
//
// The fix is to resolve the profile FIRST (cross-region, plan-aware) and only then
// ask upstream who the credential belongs to, using the profile's own region. Both
// steps are best effort: a login must still succeed if upstream metadata is
// briefly unavailable, since ordinary lazy resolution retries on first use.
//
// It deliberately does NOT call RefreshAccountInfo: that classifies failures and
// can disable/ban an account, which is the wrong behavior for a login that just
// succeeded. Only additive identity backfill happens here.
func hydrateAccountAfterLogin(account *config.Account) {
	if account == nil || strings.TrimSpace(account.ID) == "" {
		return
	}

	// GetUsageLimits resolves and persists the profile ARN through
	// ensureRestProfileArn (cross-region probe, preferring an in-plan profile) and
	// returns the identity block in the same round trip.
	usage, err := GetUsageLimits(account)
	if err != nil {
		logger.Warnf("[Login] Identity/profile hydration deferred for account %s: %v", account.ID, err)
		return
	}
	if usage == nil || usage.UserInfo == nil {
		return
	}

	email := strings.TrimSpace(usage.UserInfo.Email)
	userID := strings.TrimSpace(usage.UserInfo.UserId)
	if email == "" && userID == "" {
		return
	}
	if email != "" {
		account.Email = email
	}
	if userID != "" {
		account.UserId = userID
	}
	if err := config.UpdateAccountIdentity(account.ID, email, userID); err != nil {
		logger.Warnf("[Login] Failed to persist identity for account %s: %v", account.ID, err)
	}
}
