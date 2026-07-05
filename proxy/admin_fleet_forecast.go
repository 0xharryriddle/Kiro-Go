package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"time"
)

// fleetForecastItem is one account's capacity projection for the admin UI.
//
// All figures are AWS agentic-request credits — the scarce resource. This is a
// READ-ONLY projection: it changes no routing and takes no action. Burn rate uses
// the authoritative upstream delta (total consumption from every client) so the
// runway is honest even when a credential is shared/used externally.
type fleetForecastItem struct {
	AccountID        string  `json:"accountId"`
	Email            string  `json:"email,omitempty"`
	Enabled          bool    `json:"enabled"`
	RemainingCredits float64 `json:"remainingCredits"`
	UsageLimit       float64 `json:"usageLimit"`
	BurnPerHour      float64 `json:"burnPerHour"`
	HoursToExhaust   float64 `json:"hoursToExhaust,omitempty"`
	ExhaustsAt       int64   `json:"exhaustsAt,omitempty"`
	ElapsedSeconds   int64   `json:"elapsedSeconds"`
	NextResetDate    string  `json:"nextResetDate,omitempty"`
	Confidence       string  `json:"confidence"`
}

// apiGetFleetForecast projects, per enabled account, when its remaining period
// quota will be exhausted at the observed burn rate. No upstream calls — it reads
// the usage baseline captured by the 30-min background refresh. Read-only.
func (h *Handler) apiGetFleetForecast(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	now := time.Now().Unix()

	items := make([]fleetForecastItem, 0, len(accounts))
	summary := map[string]interface{}{
		"projectable":           0, // accounts with an "ok" projection
		"idle":                  0,
		"depleted":              0,
		"unknown":               0,
		"fleetRemainingCredits": 0.0,
		"fleetBurnPerHour":      0.0,
		"soonestHoursToExhaust": 0.0, // min hoursToExhaust across projectable accounts (0 = none)
		"soonestAccountId":      "",
	}
	projectable := 0
	idle := 0
	depleted := 0
	unknown := 0
	var fleetRemaining, fleetBurn, soonest float64
	soonestID := ""

	for _, a := range accounts {
		hasUpstream := a.NextResetDate != "" || a.UsageLimit > 0
		fc := config.ForecastAccount(config.AccountForecastInput{
			UsageCurrent:  a.UsageCurrent,
			UsageLimit:    a.UsageLimit,
			PeriodStart:   a.ExternalPeriodStart,
			PeriodStartAt: a.ExternalPeriodStartAt,
			HasUpstream:   hasUpstream,
			Enabled:       a.Enabled,
		}, now)

		items = append(items, fleetForecastItem{
			AccountID:        a.ID,
			Email:            a.Email,
			Enabled:          a.Enabled,
			RemainingCredits: fc.RemainingCredits,
			UsageLimit:       a.UsageLimit,
			BurnPerHour:      fc.BurnPerHour,
			HoursToExhaust:   fc.HoursToExhaust,
			ExhaustsAt:       fc.ExhaustsAt,
			ElapsedSeconds:   fc.ElapsedSeconds,
			NextResetDate:    a.NextResetDate,
			Confidence:       fc.Confidence,
		})

		fleetRemaining += fc.RemainingCredits
		switch fc.Confidence {
		case config.ForecastConfidenceOK:
			projectable++
			fleetBurn += fc.BurnPerHour
			// Only enabled accounts contribute to the "soonest exhaust" alarm,
			// since a disabled account we don't route to isn't our burn concern.
			if a.Enabled && fc.HoursToExhaust > 0 && (soonest == 0 || fc.HoursToExhaust < soonest) {
				soonest = fc.HoursToExhaust
				soonestID = a.ID
			}
		case config.ForecastConfidenceIdle:
			idle++
		case config.ForecastConfidenceDepleted:
			depleted++
		default:
			unknown++
		}
	}

	summary["projectable"] = projectable
	summary["idle"] = idle
	summary["depleted"] = depleted
	summary["unknown"] = unknown
	summary["fleetRemainingCredits"] = fleetRemaining
	summary["fleetBurnPerHour"] = fleetBurn
	summary["soonestHoursToExhaust"] = soonest
	summary["soonestAccountId"] = soonestID

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"summary": summary,
		"items":   items,
		"note":    "Read-only projection in AWS agentic-request credits. Burn rate uses total upstream consumption since the period baseline; accounts show 'unknown' until ~10 min after a baseline is captured.",
	})
}
