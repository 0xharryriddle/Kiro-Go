package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	"net/http"
	"strings"
	"time"
)

type finalizeKiroSsoProfileRequest struct {
	SessionID  string `json:"sessionId"`
	ProfileArn string `json:"profileArn"`
}

var pollKiroSsoAuthForAdmin = auth.PollKiroSsoAuth
var discoverKiroSsoProfilesForAdmin = discoverKiroProfiles
var addKiroSsoAccountForAdmin = config.AddAccount

func kiroSsoAccountFromResult(result auth.KiroSsoResult, tokenExpiresAt int64) config.Account {
	return config.Account{
		ID:            auth.GenerateAccountID(),
		Email:         strings.TrimSpace(result.Email),
		AccessToken:   result.AccessToken,
		RefreshToken:  result.RefreshToken,
		ClientID:      result.ClientID,
		AuthMethod:    result.AuthMethod,
		Provider:      result.Provider,
		Region:        result.Region,
		ProfileArn:    strings.TrimSpace(result.ProfileArn),
		TokenEndpoint: result.TokenEndpoint,
		IssuerURL:     result.IssuerURL,
		Scopes:        result.Scopes,
		ExpiresAt:     tokenExpiresAt,
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
	}
}

func usableKiroSsoProfiles(discovery KiroProfileDiscovery) []KiroProfile {
	profiles := make([]KiroProfile, 0, len(discovery.Profiles))
	seen := make(map[string]bool)
	for _, profile := range discovery.Profiles {
		profile.Arn = strings.TrimSpace(profile.Arn)
		profile.Region = strings.ToLower(strings.TrimSpace(profile.Region))
		if !profile.Usable || profile.Arn == "" || profile.Region == "" || seen[profile.Arn] {
			continue
		}
		seen[profile.Arn] = true
		profiles = append(profiles, profile)
	}
	sortKiroProfiles(profiles)
	return profiles
}

func (h *Handler) persistKiroSsoAccount(account config.Account) error {
	if err := addKiroSsoAccountForAdmin(account); err != nil {
		return err
	}
	if h.pool != nil {
		h.pool.Reload()
	}
	return nil
}

func writeKiroSsoCompleted(w http.ResponseWriter, account config.Account) {
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":            account.ID,
			"email":         account.Email,
			"authMethod":    account.AuthMethod,
			"profileArn":    account.ProfileArn,
			"profilePinned": account.ProfilePinned,
		},
	})
}

func writeKiroSsoProfileChoice(w http.ResponseWriter, profiles []KiroProfile, warnings []KiroProfileRegionWarning, expiresAt time.Time) {
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":               true,
		"completed":             false,
		"status":                "profile_required",
		"requiresProfileChoice": true,
		"profiles":              profiles,
		"warnings":              warnings,
		"expiresAt":             expiresAt.Unix(),
	})
}

// processCompletedKiroSsoResult owns an exchanged credential after the auth
// package has consumed and deleted its login session. It either persists the
// account immediately or parks the credential until an offered profile is chosen.
func (h *Handler) processCompletedKiroSsoResult(sessionID string, result auth.KiroSsoResult) (config.Account, []KiroProfile, []KiroProfileRegionWarning, time.Time, error) {
	now := time.Now()
	tokenExpiresAt := now.Unix() + int64(result.ExpiresIn)
	account := kiroSsoAccountFromResult(result, tokenExpiresAt)

	discovery, err := discoverKiroSsoProfilesForAdmin(&account)
	if err != nil {
		// Preserve the existing lazy-resolution fallback if eager discovery itself
		// cannot run. The exchanged credential is still valid and recoverable.
		if persistErr := h.persistKiroSsoAccount(account); persistErr != nil {
			return config.Account{}, nil, nil, time.Time{}, persistErr
		}
		return account, nil, nil, time.Time{}, nil
	}
	profiles := usableKiroSsoProfiles(discovery)
	switch len(profiles) {
	case 0:
		// Keep any profile ARN returned directly by the social exchange; external
		// IdP credentials remain empty and resolve lazily on first use.
		if err := h.persistKiroSsoAccount(account); err != nil {
			return config.Account{}, nil, nil, time.Time{}, err
		}
		return account, nil, discovery.Warnings, time.Time{}, nil
	case 1:
		// A single unambiguous profile is cached automatically, not operator-pinned.
		account.ProfileArn = profiles[0].Arn
		account.ProfilePinned = false
		account.RegionOverride = ""
		if err := h.persistKiroSsoAccount(account); err != nil {
			return config.Account{}, nil, nil, time.Time{}, err
		}
		return account, nil, discovery.Warnings, time.Time{}, nil
	default:
		expiresAt, err := h.getKiroSsoProfileChoiceStore().put(
			sessionID, result, tokenExpiresAt, profiles, discovery.Warnings,
		)
		if err != nil {
			return config.Account{}, nil, nil, time.Time{}, err
		}
		return config.Account{}, profiles, discovery.Warnings, expiresAt, nil
	}
}

func (h *Handler) apiFinalizeKiroSsoProfile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	defer r.Body.Close()
	var body finalizeKiroSsoProfileRequest
	if err := decodeSingleProfileAdminJSON(w, r, &body); err != nil {
		writeKiroProfileAdminError(w, http.StatusBadRequest, "invalid_request", "Invalid request")
		return
	}
	body.SessionID = strings.TrimSpace(body.SessionID)
	body.ProfileArn = strings.TrimSpace(body.ProfileArn)
	if body.SessionID == "" || body.ProfileArn == "" || regionFromProfileArn(body.ProfileArn) == "" {
		writeKiroProfileAdminError(w, http.StatusBadRequest, "invalid_profile_choice", "sessionId and a valid profileArn are required")
		return
	}

	var account config.Account
	err := h.getKiroSsoProfileChoiceStore().commit(body.SessionID, body.ProfileArn,
		func(credential auth.KiroSsoResult, selected KiroProfile, tokenExpiresAt int64) error {
			account = kiroSsoAccountFromResult(credential, tokenExpiresAt)
			account.ProfileArn = selected.Arn
			account.ProfilePinned = true
			account.RegionOverride = selected.Region
			return h.persistKiroSsoAccount(account)
		})
	if err != nil {
		code := err.Error()
		switch code {
		case "profile_choice_not_found", "profile_choice_expired":
			writeKiroProfileAdminError(w, http.StatusGone, code, "Profile choice is missing, expired, or already consumed")
		case "profile_not_offered":
			writeKiroProfileAdminError(w, http.StatusConflict, code, "The selected profile was not offered")
		default:
			writeKiroProfileAdminError(w, http.StatusInternalServerError, "persist_failed", "Failed to persist SSO account")
		}
		return
	}
	writeKiroSsoCompleted(w, account)
}

func (h *Handler) pendingKiroSsoProfileChoice(sessionID string) ([]KiroProfile, []KiroProfileRegionWarning, time.Time, bool) {
	profiles, warnings, expiresAt, err := h.getKiroSsoProfileChoiceStore().get(sessionID)
	return profiles, warnings, expiresAt, err == nil
}

func validateCompletedKiroSsoResult(result *auth.KiroSsoResult, status string) error {
	if status != "completed" || result == nil {
		return fmt.Errorf("SSO login did not return a completed credential")
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return fmt.Errorf("SSO login returned an empty access token")
	}
	return nil
}
