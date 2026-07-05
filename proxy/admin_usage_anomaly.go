package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"time"
)

// usageAnomalyItem is one account's anomaly verdict for the admin UI.
//
// HONESTY NOTE: heuristic over recent request-log volume vs the account's own
// 24h baseline. Detects deviation from established history, not intent.
type usageAnomalyItem struct {
	AccountID         string  `json:"accountId"`
	Email             string  `json:"email,omitempty"`
	Enabled           bool    `json:"enabled"`
	Tier              string  `json:"tier"`
	Ratio             float64 `json:"ratio"`             // recent/baseline hourly-rate ratio
	RecentRequests    int     `json:"recentRequests"`    // requests in the last hour
	BaselineRequests  int     `json:"baselineRequests"`  // requests in the last 24h
	RecentRatePerHr   float64 `json:"recentRatePerHr"`   // recent requests per hour
	BaselineRatePerHr float64 `json:"baselineRatePerHr"` // baseline requests per hour
}

// apiGetUsageAnomaly aggregates recent vs baseline request rates per account and
// flags spikes. No upstream calls — read-only over in-memory request logs.
func (h *Handler) apiGetUsageAnomaly(w http.ResponseWriter, r *http.Request) {
	logs := h.getRequestLogs()
	accounts := config.GetAccounts()
	now := time.Now().Unix()

	items := make([]usageAnomalyItem, 0, len(accounts))
	summary := map[string]int{
		"normal":  0,
		"spike":   0,
		"unknown": 0,
	}

	for _, a := range accounts {
		in := aggregateAnomaly(logs, a.ID, now)
		tier, ratio := detectUsageAnomaly(in)
		switch tier {
		case AnomalyTierSpike:
			summary["spike"]++
		case AnomalyTierNormal:
			summary["normal"]++
		default:
			summary["unknown"]++
		}
		items = append(items, usageAnomalyItem{
			AccountID:         a.ID,
			Email:             a.Email,
			Enabled:           a.Enabled,
			Tier:              tier,
			Ratio:             ratio,
			RecentRequests:    in.RecentRequests,
			BaselineRequests:  in.BaselineRequests,
			RecentRatePerHr:   in.RecentRatePerHr,
			BaselineRatePerHr: in.BaselineRatePerHr,
		})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"summary": summary,
		"items":   items,
		"note":    "Heuristic: recent 1h request rate vs the account's own 24h baseline. A spike is a recent rate that materially exceeds established history — an early signal, not proof of misuse.",
	})
}
