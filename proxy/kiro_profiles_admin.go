package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"strings"
)

const kiroProfileAdminBodyLimit = 8 << 10

type selectKiroProfileRequest struct {
	ProfileArn string `json:"profileArn"`
}

var discoverKiroProfilesForAdmin = discoverKiroProfiles
var resolveKiroProfileForAdmin = ResolveProfileArn
var refreshKiroProfileModelsForAdmin = func(h *Handler, account *config.Account) error {
	return h.fetchAndCacheAccountModels(account)
}

// accountForProfileAdmin returns a detached account copy. Persisted profile/pin
// state remains authoritative while the latest pool copy supplies refreshed OAuth
// token material for an immediate upstream discovery call.
func (h *Handler) accountForProfileAdmin(id string) (config.Account, bool) {
	account, ok := config.GetAccountByID(strings.TrimSpace(id))
	if !ok {
		return config.Account{}, false
	}
	if h.pool == nil {
		return account, true
	}
	for _, runtime := range h.pool.GetAllAccounts() {
		if runtime.ID != account.ID {
			continue
		}
		account.AccessToken = runtime.AccessToken
		account.RefreshToken = runtime.RefreshToken
		account.ExpiresAt = runtime.ExpiresAt
		break
	}
	return account, true
}

func decodeSingleProfileAdminJSON(w http.ResponseWriter, r *http.Request, target interface{}) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, kiroProfileAdminBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func (h *Handler) apiGetKiroProfiles(w http.ResponseWriter, r *http.Request, id string) {
	account, ok := h.accountForProfileAdmin(id)
	if !ok {
		writeKiroProfileAdminError(w, http.StatusNotFound, "account_not_found", "Account not found")
		return
	}
	discovery, err := discoverKiroProfilesForAdmin(&account)
	if err != nil {
		writeKiroProfileAdminError(w, http.StatusBadGateway, "profile_discovery_failed", "Kiro profile discovery failed")
		return
	}
	_ = json.NewEncoder(w).Encode(discovery)
}

func (h *Handler) apiSelectKiroProfile(w http.ResponseWriter, r *http.Request, id string) {
	defer r.Body.Close()
	account, ok := h.accountForProfileAdmin(id)
	if !ok {
		writeKiroProfileAdminError(w, http.StatusNotFound, "account_not_found", "Account not found")
		return
	}
	if account.IsKiroAPIKeyCredential() {
		writeKiroProfileAdminError(w, http.StatusConflict, "key_bound", "Kiro API-key accounts use a key-bound profile")
		return
	}

	var body selectKiroProfileRequest
	if err := decodeSingleProfileAdminJSON(w, r, &body); err != nil {
		writeKiroProfileAdminError(w, http.StatusBadRequest, "invalid_request", "Invalid request")
		return
	}
	requestedArn := strings.TrimSpace(body.ProfileArn)
	requestedRegion := regionFromProfileArn(requestedArn)
	if requestedArn == "" || requestedRegion == "" {
		writeKiroProfileAdminError(w, http.StatusBadRequest, "invalid_profile_arn", "A valid Kiro profile ARN is required")
		return
	}

	// Re-discover immediately before mutation. A stale ARN from an earlier GET or
	// a caller-supplied ARN that was never offered is never accepted.
	discovery, err := discoverKiroProfilesForAdmin(&account)
	if err != nil {
		writeKiroProfileAdminError(w, http.StatusBadGateway, "profile_discovery_failed", "Kiro profile discovery failed")
		return
	}
	found := false
	usable := false
	for _, profile := range discovery.Profiles {
		if profile.Arn == requestedArn {
			found = true
			usable = profile.Usable
			requestedRegion = profile.Region
			break
		}
	}
	if !found {
		writeKiroProfileAdminError(w, http.StatusConflict, "profile_not_discovered", "The requested profile is not in the fresh discovery result")
		return
	}
	if !usable {
		writeKiroProfileAdminError(w, http.StatusUnprocessableEntity, "profile_not_usable", "The requested profile is not usable for this account")
		return
	}

	changed, err := config.UpdateAccountProfileSelection(account.ID, requestedArn, requestedRegion, true)
	if err != nil {
		writeKiroProfileAdminError(w, http.StatusInternalServerError, "profile_update_failed", "Failed to persist profile selection")
		return
	}
	account.ProfileArn = requestedArn
	account.ProfilePinned = true
	account.RegionOverride = requestedRegion
	clearProfileArnResolutionCooldown(&account)
	h.invalidateAggregatedModelsCache()

	modelsRefreshed := false
	modelRefreshError := ""
	if h.pool != nil {
		h.pool.ClearModelList(account.ID)
		h.pool.Reload()
		if err := refreshKiroProfileModelsForAdmin(h, &account); err != nil {
			modelRefreshError = "model_refresh_failed"
		} else {
			modelsRefreshed = true
		}
	}
	// fetchAndCacheAccountModels merges one account into the aggregate. Drop that
	// partial view so the next /v1/models request rebuilds the complete fleet.
	h.invalidateAggregatedModelsCache()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"changed":           changed,
		"profileArn":        requestedArn,
		"region":            requestedRegion,
		"profilePinned":     true,
		"modelsRefreshed":   modelsRefreshed,
		"modelRefreshError": modelRefreshError,
	})
}

func (h *Handler) apiAutoKiroProfile(w http.ResponseWriter, r *http.Request, id string) {
	account, ok := h.accountForProfileAdmin(id)
	if !ok {
		writeKiroProfileAdminError(w, http.StatusNotFound, "account_not_found", "Account not found")
		return
	}
	if account.IsKiroAPIKeyCredential() {
		writeKiroProfileAdminError(w, http.StatusConflict, "key_bound", "Kiro API-key accounts use a key-bound profile")
		return
	}

	changed, err := config.UpdateAccountProfileSelection(account.ID, "", "", false)
	if err != nil {
		writeKiroProfileAdminError(w, http.StatusInternalServerError, "profile_update_failed", "Failed to enable automatic profile selection")
		return
	}
	account.ProfileArn = ""
	account.ProfilePinned = false
	account.RegionOverride = ""
	clearProfileArnResolutionCooldown(&account)
	h.invalidateAggregatedModelsCache()
	if h.pool != nil {
		h.pool.ClearModelList(account.ID)
		h.pool.Reload()
	}

	resolvedArn, resolveErr := resolveKiroProfileForAdmin(&account)
	resolvedArn = strings.TrimSpace(resolvedArn)
	resolved := resolveErr == nil && resolvedArn != ""
	if resolved {
		account.ProfileArn = resolvedArn
	}
	modelsRefreshed := false
	modelRefreshError := ""
	if h.pool != nil {
		// ResolveProfileArn may have persisted a newly selected automatic ARN.
		h.pool.Reload()
		if resolved {
			if err := refreshKiroProfileModelsForAdmin(h, &account); err != nil {
				modelRefreshError = "model_refresh_failed"
			} else {
				modelsRefreshed = true
			}
		}
	}
	h.invalidateAggregatedModelsCache()
	warning := ""
	if !resolved {
		warning = "profile_resolution_failed"
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"changed":           changed,
		"profileArn":        account.ProfileArn,
		"region":            regionFromProfileArn(account.ProfileArn),
		"profilePinned":     false,
		"resolved":          resolved,
		"warning":           warning,
		"modelsRefreshed":   modelsRefreshed,
		"modelRefreshError": modelRefreshError,
	})
}

func writeKiroProfileAdminError(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}
