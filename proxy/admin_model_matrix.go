package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
)

// modelMatrixEntry is one model's fleet-wide availability for the admin UI.
type modelMatrixEntry struct {
	Model        string             `json:"model"`
	CapableCount int                `json:"capableCount"`
	Accounts     []matrixAccountRef `json:"accounts"`
}

// matrixAccountRef identifies one capable account (id + email label + whether it
// currently routes locally).
type matrixAccountRef struct {
	AccountID string `json:"accountId"`
	Email     string `json:"email,omitempty"`
	Enabled   bool   `json:"enabled"`
}

// apiGetModelMatrix returns the fleet model-availability grid: for each model in
// the union of all accounts' cached model sets, which accounts serve it. Read-only
// over the pool's in-memory model cache; no upstream calls.
//
// When no account has a model cache yet, routing is optimistic (any account may
// serve any model) and the matrix is empty — surfaced via optimisticFallback so
// the UI can explain the empty grid instead of implying zero capability.
func (h *Handler) apiGetModelMatrix(w http.ResponseWriter, r *http.Request) {
	entries, accountsWithCache := h.pool.ModelMatrix()

	// Build an id -> account lookup for email labels and enabled state.
	accounts := config.GetAccounts()
	byID := make(map[string]config.Account, len(accounts))
	for _, a := range accounts {
		byID[a.ID] = a
	}

	items := make([]modelMatrixEntry, 0, len(entries))
	for _, e := range entries {
		refs := make([]matrixAccountRef, 0, len(e.AccountIDs))
		for _, id := range e.AccountIDs {
			acc := byID[id]
			refs = append(refs, matrixAccountRef{
				AccountID: id,
				Email:     acc.Email,
				Enabled:   acc.Enabled,
			})
		}
		items = append(items, modelMatrixEntry{
			Model:        e.Model,
			CapableCount: e.CapableCount,
			Accounts:     refs,
		})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":            true,
		"totalModels":        len(items),
		"accountsWithCache":  accountsWithCache,
		"optimisticFallback": accountsWithCache == 0,
		"items":              items,
		"note":               "Model availability is read from each account's cached model list (refreshed on model refresh). When no account has a cache yet, routing is optimistic and this grid is empty.",
	})
}
