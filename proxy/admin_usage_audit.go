package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// usageAuditItem is the per-account external-usage verdict returned to the admin UI.
//
// All credit figures share the same AWS metering unit. There is deliberately no
// "external tokens" figure: upstream exposes no token count for traffic we did not
// originate, so OurTokensTotal is the ONLY honest token number and it covers only
// requests this proxy served.
type usageAuditItem struct {
	AccountID   string `json:"accountId"`
	Email       string `json:"email,omitempty"`
	Provider    string `json:"provider,omitempty"`
	AuthMethod  string `json:"authMethod,omitempty"`
	Enabled     bool   `json:"enabled"`
	HasUpstream bool   `json:"hasUpstream"`

	// Upstream authoritative (period-scoped, resets each billing period).
	UpstreamCurrent float64 `json:"upstreamCurrent"` // credits used this period from ANY client
	UpstreamLimit   float64 `json:"upstreamLimit"`
	UpstreamPercent float64 `json:"upstreamPercent"`
	NextResetDate   string  `json:"nextResetDate,omitempty"`
	LastRefresh     int64   `json:"lastRefresh,omitempty"`

	// What WE drove.
	OurPeriodCredits float64 `json:"ourPeriodCredits"` // credits we metered within the current period
	OurCreditsTotal  float64 `json:"ourCreditsTotal"`  // cumulative all-time credits we metered
	OurTokensTotal   int     `json:"ourTokensTotal"`   // cumulative all-time tokens we proxied (only honest token figure)

	// External verdict.
	ExternalCredits float64 `json:"externalCredits"` // clamped >=0 estimate of third-party consumption this period
	Confidence      string  `json:"confidence"`      // clean | external | strong_external | unknown
	CheckedAt       int64   `json:"externalCheckedAt,omitempty"`

	// Drill-down: the raw period baseline so operators can see WHY a verdict was reached.
	PeriodKey   string  `json:"periodKey,omitempty"`
	PeriodStart float64 `json:"periodStart"` // upstream CurrentUsage captured at period start
}

// buildUsageAuditItems assembles the fleet's external-usage verdicts by joining the
// persisted per-account audit state with runtime pool stats (our tokens/credits).
func (h *Handler) buildUsageAuditItems() ([]usageAuditItem, map[string]interface{}) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()
	statsMap := make(map[string]config.Account, len(poolAccounts))
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}

	items := make([]usageAuditItem, 0, len(accounts))
	summary := map[string]int{
		"total":          0,
		"clean":          0,
		"external":       0,
		"strongExternal": 0,
		"unknown":        0,
	}
	var fleetExternalCredits float64

	for _, a := range accounts {
		stats := statsMap[a.ID]
		hasUpstream := strings.TrimSpace(a.NextResetDate) != "" || a.UsageLimit > 0

		item := usageAuditItem{
			AccountID:        a.ID,
			Email:            a.Email,
			Provider:         a.Provider,
			AuthMethod:       a.AuthMethod,
			Enabled:          a.Enabled,
			HasUpstream:      hasUpstream,
			UpstreamCurrent:  a.UsageCurrent,
			UpstreamLimit:    a.UsageLimit,
			UpstreamPercent:  a.UsagePercent,
			NextResetDate:    a.NextResetDate,
			LastRefresh:      a.LastRefresh,
			OurPeriodCredits: a.ExternalPeriodOurCredit,
			OurCreditsTotal:  stats.TotalCredits,
			OurTokensTotal:   stats.TotalTokens,
			ExternalCredits:  a.ExternalCreditsEstimate,
			Confidence:       a.ExternalConfidence,
			CheckedAt:        a.ExternalCheckedAt,
			PeriodKey:        a.ExternalPeriodKey,
			PeriodStart:      a.ExternalPeriodStart,
		}
		if item.Confidence == "" {
			item.Confidence = config.ExternalConfidenceUnknown
		}

		summary["total"]++
		switch item.Confidence {
		case config.ExternalConfidenceClean:
			summary["clean"]++
		case config.ExternalConfidenceExternal:
			summary["external"]++
			fleetExternalCredits += item.ExternalCredits
		case config.ExternalConfidenceStrongExternal:
			summary["strongExternal"]++
			fleetExternalCredits += item.ExternalCredits
		default:
			summary["unknown"]++
		}
		items = append(items, item)
	}

	summaryOut := map[string]interface{}{
		"total":                summary["total"],
		"clean":                summary["clean"],
		"external":             summary["external"],
		"strongExternal":       summary["strongExternal"],
		"unknown":              summary["unknown"],
		"fleetExternalCredits": fleetExternalCredits,
	}
	return items, summaryOut
}

// apiGetUsageAudit returns the cached fleet external-usage audit. It performs no
// upstream calls — the verdicts are recomputed on the 30-minute backgroundRefresh
// and by the explicit recheck endpoint.
func (h *Handler) apiGetUsageAudit(w http.ResponseWriter, r *http.Request) {
	items, summary := h.buildUsageAuditItems()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"summary": summary,
		"items":   items,
		"note":    "External usage is measured in AWS agentic-request credits. No external token figure exists: upstream reports no token count for traffic this proxy did not originate.",
	})
}

// apiRecheckUsageAudit forces a live upstream usage fetch and recompute. With an
// accountId in the body it rechecks one account; otherwise it rechecks every
// enabled account with a token. This makes real upstream calls, so it is an
// explicit operator action rather than part of the cached GET.
func (h *Handler) apiRecheckUsageAudit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID string `json:"accountId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	target := strings.TrimSpace(body.AccountID)

	accounts := config.GetAccounts()
	rechecked := 0
	errs := make([]string, 0)

	for i := range accounts {
		acc := accounts[i]
		if target != "" && acc.ID != target {
			continue
		}
		if !acc.HasUpstreamCredential() {
			if target != "" {
				errs = append(errs, "account has no upstream credential")
			}
			continue
		}
		info, err := RefreshAccountInfo(&acc)
		if err != nil {
			logger.Warnf("[UsageAudit] recheck failed for %s: %v", acc.Email, err)
			errs = append(errs, acc.ID+": "+err.Error())
			// Still recompute with hasUpstream=false so the row reflects the failed check time.
			h.recomputeExternalUsage(acc.ID, nil, false)
			continue
		}
		config.UpdateAccountInfo(acc.ID, *info)
		h.recomputeExternalUsage(acc.ID, info, true)
		rechecked++
	}
	h.pool.Reload()

	h.appendAuditLog(AuditLog{
		Category:    "security",
		Action:      "usage_audit_recheck",
		Status:      "success",
		AccountID:   target,
		SafeDetails: map[string]string{"rechecked": strconv.Itoa(rechecked)},
	})

	items, summary := h.buildUsageAuditItems()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"rechecked": rechecked,
		"errors":    errs,
		"summary":   summary,
		"items":     items,
		"checkedAt": time.Now().Unix(),
	})
}
