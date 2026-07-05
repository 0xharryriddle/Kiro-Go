package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"time"
)

// healthWindowSeconds is the rolling window over which request-log outcomes are
// aggregated into an account's health score. 6 hours balances responsiveness
// (catches a degrading credential quickly) against stability (a handful of old
// errors don't haunt an account forever).
const healthWindowSeconds int64 = 6 * 3600

// accountHealthItem is one account's health verdict for the admin UI.
//
// HONESTY NOTE: this is a heuristic degradation score derived from observed
// request outcomes, NOT a validated ban predictor. See account_health.go.
type accountHealthItem struct {
	AccountID            string `json:"accountId"`
	Email                string `json:"email,omitempty"`
	Enabled              bool   `json:"enabled"`
	BanStatus            string `json:"banStatus,omitempty"`
	Score                int    `json:"score"`
	Tier                 string `json:"tier"`
	WindowRequests       int    `json:"windowRequests"`
	DangerousErrors      int    `json:"dangerousErrors"`
	MildErrors           int    `json:"mildErrors"`
	ConsecutiveDangerous int    `json:"consecutiveDangerous"`
}

// apiGetAccountHealth aggregates recent request-log outcomes per account into a
// rolling health score. No upstream calls — read-only over in-memory logs.
func (h *Handler) apiGetAccountHealth(w http.ResponseWriter, r *http.Request) {
	logs := h.getRequestLogs()
	accounts := config.GetAccounts()
	now := time.Now().Unix()

	items := make([]accountHealthItem, 0, len(accounts))
	summary := map[string]int{
		"healthy":  0,
		"degraded": 0,
		"critical": 0,
		"unknown":  0,
	}

	for _, a := range accounts {
		in := aggregateHealth(logs, a.ID, now, healthWindowSeconds)
		score, tier := scoreAccountHealth(in)
		switch tier {
		case HealthTierHealthy:
			summary["healthy"]++
		case HealthTierDegraded:
			summary["degraded"]++
		case HealthTierCritical:
			summary["critical"]++
		default:
			summary["unknown"]++
		}
		items = append(items, accountHealthItem{
			AccountID:            a.ID,
			Email:                a.Email,
			Enabled:              a.Enabled,
			BanStatus:            a.BanStatus,
			Score:                score,
			Tier:                 tier,
			WindowRequests:       in.TotalRequests,
			DangerousErrors:      in.DangerousErrors,
			MildErrors:           in.MildErrors,
			ConsecutiveDangerous: in.ConsecutiveDangerous,
		})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"summary":       summary,
		"items":         items,
		"windowSeconds": healthWindowSeconds,
		"note":          "Heuristic degradation score from recent request outcomes over the rolling window. Benign quota/overage errors are excluded. This is NOT a validated ban predictor.",
	})
}
