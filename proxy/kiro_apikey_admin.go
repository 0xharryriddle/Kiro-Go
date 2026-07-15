package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
)

const (
	kiroAPIKeyProbeTTL        = 5 * time.Minute
	kiroAPIKeyMinLength       = 8
	kiroAPIKeyMaxLength       = 512
	kiroAPIKeyMaxProbeRegions = 10
)

type kiroAPIKeyProbeRequest struct {
	KiroAPIKey string   `json:"kiroApiKey"`
	Regions    []string `json:"regions,omitempty"`
}

type kiroAPIKeyProbeRegion struct {
	Region            string  `json:"region"`
	Usable            bool    `json:"usable"`
	Email             string  `json:"email,omitempty"`
	UserID            string  `json:"userId,omitempty"`
	SubscriptionType  string  `json:"subscriptionType,omitempty"`
	SubscriptionTitle string  `json:"subscriptionTitle,omitempty"`
	UsageCurrent      float64 `json:"usageCurrent,omitempty"`
	UsageLimit        float64 `json:"usageLimit,omitempty"`
	NextResetDate     string  `json:"nextResetDate,omitempty"`
	ErrorCode         string  `json:"errorCode,omitempty"`
}

type kiroAPIKeyProbeResponse struct {
	ProbeID   string                  `json:"probeId,omitempty"`
	ExpiresAt int64                   `json:"expiresAt,omitempty"`
	Regions   []kiroAPIKeyProbeRegion `json:"regions"`
}

type kiroAPIKeyCommitRequest struct {
	ProbeID        string `json:"probeId"`
	SelectedRegion string `json:"selectedRegion"`
	Nickname       string `json:"nickname,omitempty"`
	Enabled        *bool  `json:"enabled,omitempty"`
}

type pendingKiroAPIKeyProbe struct {
	id        string
	key       string
	expiresAt time.Time
	regions   map[string]kiroAPIKeyProbeRegion
	timer     *time.Timer
}

type kiroAPIKeyProbeStore struct {
	mu      sync.Mutex
	entries map[string]*pendingKiroAPIKeyProbe
	ttl     time.Duration
}

func newKiroAPIKeyProbeStore(ttl time.Duration) *kiroAPIKeyProbeStore {
	if ttl <= 0 {
		ttl = kiroAPIKeyProbeTTL
	}
	return &kiroAPIKeyProbeStore{
		entries: make(map[string]*pendingKiroAPIKeyProbe),
		ttl:     ttl,
	}
}

func (s *kiroAPIKeyProbeStore) put(key string, regions []kiroAPIKeyProbeRegion) (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := &pendingKiroAPIKeyProbe{
		id:        uuid.NewString(),
		key:       key,
		expiresAt: time.Now().Add(s.ttl),
		regions:   make(map[string]kiroAPIKeyProbeRegion),
	}
	for _, region := range regions {
		if region.Usable {
			entry.regions[region.Region] = region
		}
	}
	s.entries[entry.id] = entry
	entry.timer = time.AfterFunc(s.ttl, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		// Identity-check the entry so a stale timer can never delete a newer value.
		if current, ok := s.entries[entry.id]; ok && current == entry {
			entry.key = ""
			delete(s.entries, entry.id)
		}
	})
	return entry.id, entry.expiresAt
}

// commit serializes valid commit attempts and consumes the secret only after the
// persistence callback succeeds. Transient durable-write failures therefore keep
// the original probe deadline and let the operator retry without re-entering the key.
func (s *kiroAPIKeyProbeStore) commit(id, region string, persist func(string, kiroAPIKeyProbeRegion) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[id]
	if !ok {
		return fmt.Errorf("probe_not_found")
	}
	if !time.Now().Before(entry.expiresAt) {
		entry.key = ""
		delete(s.entries, id)
		if entry.timer != nil {
			entry.timer.Stop()
		}
		return fmt.Errorf("probe_expired")
	}
	result, ok := entry.regions[region]
	if !ok || !result.Usable {
		return fmt.Errorf("region_not_probed")
	}
	if persist == nil {
		return fmt.Errorf("probe_persist_missing")
	}
	if err := persist(entry.key, result); err != nil {
		return err
	}
	entry.key = ""
	delete(s.entries, id)
	if entry.timer != nil {
		entry.timer.Stop()
	}
	return nil
}

func (h *Handler) getKiroAPIKeyProbeStore() *kiroAPIKeyProbeStore {
	h.kiroAPIKeyProbesMu.Lock()
	defer h.kiroAPIKeyProbesMu.Unlock()
	if h.kiroAPIKeyProbes == nil {
		h.kiroAPIKeyProbes = newKiroAPIKeyProbeStore(kiroAPIKeyProbeTTL)
	}
	return h.kiroAPIKeyProbes
}

func validateKiroIssuedAPIKey(raw string) (string, error) {
	key := raw
	if len(key) < kiroAPIKeyMinLength || len(key) > kiroAPIKeyMaxLength {
		return "", fmt.Errorf("Kiro API key length is invalid")
	}
	if !strings.HasPrefix(key, "ksk_") {
		return "", fmt.Errorf("Kiro API key must start with ksk_")
	}
	for _, r := range key {
		if r > unicode.MaxASCII || unicode.IsSpace(r) || unicode.IsControl(r) || !unicode.IsPrint(r) {
			return "", fmt.Errorf("Kiro API key contains invalid characters")
		}
	}
	return key, nil
}

func kiroAPIKeyProbeRegions(requested []string) ([]string, error) {
	if len(requested) > kiroAPIKeyMaxProbeRegions {
		return nil, fmt.Errorf("too many regions; maximum is %d", kiroAPIKeyMaxProbeRegions)
	}
	if len(requested) == 0 {
		if env := strings.TrimSpace(os.Getenv("KIRO_PROFILE_REGIONS")); env != "" {
			requested = strings.Split(env, ",")
		} else {
			requested = append([]string(nil), defaultKiroProfileRegions...)
		}
	}
	seen := make(map[string]bool)
	regions := make([]string, 0, len(requested))
	for _, raw := range requested {
		region, ok := validateRegionOverride(raw)
		if !ok || region == "" {
			return nil, fmt.Errorf("invalid region %q", raw)
		}
		if !seen[region] {
			seen[region] = true
			regions = append(regions, region)
		}
	}
	if len(regions) == 0 {
		return nil, fmt.Errorf("at least one region is required")
	}
	if len(regions) > kiroAPIKeyMaxProbeRegions {
		return nil, fmt.Errorf("too many regions; maximum is %d", kiroAPIKeyMaxProbeRegions)
	}
	return regions, nil
}

var probeKiroAPIKeyAccount = func(account *config.Account) (*config.AccountInfo, error) {
	return RefreshAccountInfo(account)
}

func classifyKiroAPIKeyProbeError(err error) string {
	if err == nil {
		return ""
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "401"), strings.Contains(lower, "403"), strings.Contains(lower, "unauthorized"), strings.Contains(lower, "invalid"), strings.Contains(lower, "expired"):
		return "unauthorized"
	case strings.Contains(lower, "429"), strings.Contains(lower, "rate limit"):
		return "rate_limited"
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "connection"), strings.Contains(lower, "no such host"), strings.Contains(lower, "http 5"):
		return "region_unavailable"
	default:
		return "upstream_error"
	}
}

func maskKiroIssuedAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "********"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

func decodeSingleKiroAPIKeyJSON(decoder *json.Decoder, target interface{}) error {
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

func (h *Handler) apiProbeKiroAPIKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	defer r.Body.Close()
	var body kiroAPIKeyProbeRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decodeSingleKiroAPIKeyJSON(decoder, &body); err != nil {
		writeKiroAPIKeyError(w, http.StatusBadRequest, "invalid_request", "Invalid request")
		return
	}
	key, err := validateKiroIssuedAPIKey(body.KiroAPIKey)
	if err != nil {
		writeKiroAPIKeyError(w, http.StatusBadRequest, "invalid_key", err.Error())
		return
	}
	regions, err := kiroAPIKeyProbeRegions(body.Regions)
	if err != nil {
		writeKiroAPIKeyError(w, http.StatusBadRequest, "invalid_regions", err.Error())
		return
	}

	results := make([]kiroAPIKeyProbeRegion, 0, len(regions))
	usable := 0
	for _, region := range regions {
		probe := &config.Account{
			AuthMethod:     "api_key",
			Provider:       "KiroAPIKey",
			KiroApiKey:     key,
			Region:         region,
			RegionOverride: region,
			MachineId:      config.GenerateMachineId(),
		}
		info, probeErr := probeKiroAPIKeyAccount(probe)
		result := kiroAPIKeyProbeRegion{Region: region}
		if probeErr != nil || info == nil {
			if probeErr == nil {
				probeErr = fmt.Errorf("empty upstream probe result")
			}
			result.ErrorCode = classifyKiroAPIKeyProbeError(probeErr)
			logger.Warnf("[KiroAPIKeyProbe] Probe failed in region %s (%s)", region, result.ErrorCode)
		} else {
			result.Usable = true
			result.Email = strings.TrimSpace(info.Email)
			result.UserID = strings.TrimSpace(info.UserId)
			result.SubscriptionType = strings.TrimSpace(info.SubscriptionType)
			result.SubscriptionTitle = strings.TrimSpace(info.SubscriptionTitle)
			result.UsageCurrent = info.UsageCurrent
			result.UsageLimit = info.UsageLimit
			result.NextResetDate = info.NextResetDate
			usable++
		}
		results = append(results, result)
	}
	if usable == 0 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "Kiro API key was not usable in any probed region",
			"code":    "no_usable_region",
			"regions": results,
		})
		return
	}

	probeID, expiresAt := h.getKiroAPIKeyProbeStore().put(key, results)
	_ = json.NewEncoder(w).Encode(kiroAPIKeyProbeResponse{
		ProbeID:   probeID,
		ExpiresAt: expiresAt.Unix(),
		Regions:   results,
	})
}

func (h *Handler) apiCommitKiroAPIKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	defer r.Body.Close()
	var body kiroAPIKeyCommitRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decodeSingleKiroAPIKeyJSON(decoder, &body); err != nil {
		writeKiroAPIKeyError(w, http.StatusBadRequest, "invalid_request", "Invalid request")
		return
	}
	body.ProbeID = strings.TrimSpace(body.ProbeID)
	region, ok := validateRegionOverride(body.SelectedRegion)
	if body.ProbeID == "" || !ok || region == "" {
		writeKiroAPIKeyError(w, http.StatusBadRequest, "invalid_commit", "probeId and a valid selectedRegion are required")
		return
	}
	nickname := strings.TrimSpace(body.Nickname)
	if len(nickname) > 100 {
		writeKiroAPIKeyError(w, http.StatusBadRequest, "invalid_nickname", "nickname is too long")
		return
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	var key string
	var account config.Account
	var existing config.Account
	added := false
	err := h.getKiroAPIKeyProbeStore().commit(body.ProbeID, region, func(probedKey string, result kiroAPIKeyProbeRegion) error {
		key = probedKey
		account = config.Account{
			ID: auth.GenerateAccountID(), Email: result.Email, UserId: result.UserID,
			Nickname: nickname, KiroApiKey: probedKey, AuthMethod: "api_key", Provider: "KiroAPIKey",
			Region: region, RegionOverride: region, ExpiresAt: 0, MachineId: config.GenerateMachineId(),
			Enabled: enabled, BanStatus: "ACTIVE", SubscriptionType: result.SubscriptionType,
			SubscriptionTitle: result.SubscriptionTitle, UsageCurrent: result.UsageCurrent,
			UsageLimit: result.UsageLimit, NextResetDate: result.NextResetDate, LastRefresh: time.Now().Unix(),
		}
		if account.UsageLimit > 0 {
			account.UsagePercent = account.UsageCurrent / account.UsageLimit
		}
		var persistErr error
		existing, added, persistErr = config.AddKiroAPIKeyAccountIfAbsent(account)
		return persistErr
	})
	if err != nil {
		code := err.Error()
		status := http.StatusBadRequest
		message := "Probe is missing, expired, already consumed, or does not include that region"
		if code == "probe_not_found" || code == "probe_expired" {
			status = http.StatusGone
		} else if code != "region_not_probed" {
			status = http.StatusInternalServerError
			code = "persist_failed"
			message = "Failed to persist account; retry with the same probeId before it expires"
		}
		writeKiroAPIKeyError(w, status, code, message)
		return
	}
	if !added {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "A Kiro API-key account for this identity and region already exists",
			"code":  "duplicate_identity_region",
			"account": map[string]interface{}{
				"id": existing.ID, "email": existing.Email, "region": accountDataPlaneRegionForResponse(existing),
			},
		})
		return
	}

	if h.pool != nil {
		h.pool.Reload()
		if account.Enabled {
			go func(acc config.Account) {
				if err := h.fetchAndCacheAccountModels(&acc); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for Kiro API-key account %s: %v", acc.Email, err)
				}
			}(account)
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id": account.ID, "email": account.Email, "userId": account.UserId,
			"nickname": account.Nickname, "region": region, "enabled": account.Enabled,
			"credentialKind": "api_key", "kiroApiKeyMask": maskKiroIssuedAPIKey(key),
		},
	})
}

func accountDataPlaneRegionForResponse(account config.Account) string {
	if region := account.EffectiveRegionOverride(); region != "" {
		return region
	}
	if region := regionFromProfileArn(account.ProfileArn); region != "" {
		return region
	}
	return strings.ToLower(strings.TrimSpace(account.Region))
}

func writeKiroAPIKeyError(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}
