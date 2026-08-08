// Package pool 账号池管理
// 实现轮询负载均衡、错误冷却、Token 刷新
package pool

import (
	"kiro-go/auth"
	"kiro-go/config"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const tokenRefreshSkewSeconds int64 = 120

// AccountDiagnostics describes route-availability state without exposing secrets.
type AccountDiagnostics struct {
	ID               string  `json:"id"`
	Enabled          bool    `json:"enabled"`
	InPool           bool    `json:"inPool"`
	Available        bool    `json:"available"`
	Reason           string  `json:"reason"`
	ErrorCount       int     `json:"errorCount"`
	CooldownUntil    int64   `json:"cooldownUntil,omitempty"`
	TokenExpiresAt   int64   `json:"tokenExpiresAt,omitempty"`
	UsageCurrent     float64 `json:"usageCurrent"`
	UsageLimit       float64 `json:"usageLimit"`
	UsagePercent     float64 `json:"usagePercent"`
	OverageStatus    string  `json:"overageStatus,omitempty"`
	OverageAllowed   bool    `json:"overageAllowed"`
	CachedModelCount int     `json:"cachedModelCount"`
}

// ModelRoutingDiagnostics explains which accounts can route a specific model.
type ModelRoutingDiagnostics struct {
	Model              string               `json:"model"`
	HasAnyModelCache   bool                 `json:"hasAnyModelCache"`
	OptimisticFallback bool                 `json:"optimisticFallback"`
	RouteableCount     int                  `json:"routeableCount"`
	Accounts           []AccountDiagnostics `json:"accounts"`
}

const (
	circuitClosed         = 0
	circuitOpen           = 1
	circuitHalfOpen       = 2
	circuitErrorThreshold = 5
	circuitOpenDuration   = 30 * time.Second
	sessionAffinityTTL    = 10 * time.Minute

	// healthEWMAAlpha is the smoothing factor for the per-account EWMA health
	// signals (error rate and latency). Higher = react faster to recent events.
	healthEWMAAlpha = 0.3

	// maxAffinityEntries bounds the session-affinity map; when exceeded, expired
	// bindings are swept before inserting a new one.
	maxAffinityEntries = 1024
)

// circuitBreaker is a per-account 3-state breaker. Its own mutex guards the
// state transitions so it can be evaluated from the selection path (which only
// holds the pool's RLock) and still persist open->half-open correctly, instead
// of silently un-blocking every request once the open window elapses.
type circuitBreaker struct {
	mu             sync.Mutex
	state          int
	consecutiveErr int
	openedAt       time.Time
	// probeAt is when the currently outstanding half-open probe was admitted.
	// It exists so half-open means "one probe in flight" rather than "fully
	// open again": without it every caller after the open window was let
	// through, and the full request rate resumed instantly against an account
	// that had just failed circuitErrorThreshold times in a row.
	probeAt time.Time
}

// isOpen reports whether the breaker is currently blocking requests.
//
// This is a PURE PREDICATE: it must never mutate breaker state. Selection
// evaluates the same account through several independent gates in a single pass
// (the quota-aware eligibility check, the LRU candidate loop, and the cooldown
// fallback), so a query that consumed the half-open probe as a side effect made
// those gates disagree with each other: the first call promoted open->half-open
// and returned "admissible", and the next call in the SAME pass saw a fresh
// probe already in flight and returned "blocked" — taking a single-account pool
// completely dark exactly when its breaker was due a recovery probe.
//
// Claiming the probe is therefore a separate, explicit step (claimProbe), done
// once at the dispatch commit point for the account actually selected.
func (cb *circuitBreaker) isOpen(now time.Time) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case circuitOpen:
		// The open window having elapsed means "a probe is due", so the account
		// is admissible; whoever dispatches to it claims the probe.
		return now.Sub(cb.openedAt) < circuitOpenDuration
	case circuitHalfOpen:
		// A probe is already in flight: block everyone else so half-open sends
		// exactly one request at the suspect account and decides from its
		// outcome (recordError re-opens, reset closes).
		//
		// A probe that never reports back (dropped request, restart mid-flight)
		// must not wedge the breaker shut forever, so once another open window
		// has elapsed a fresh probe may replace the abandoned one. That bounds
		// the worst case at one request per circuitOpenDuration rather than none.
		return now.Sub(cb.probeAt) < circuitOpenDuration
	default:
		return false
	}
}

// claimProbe records that the caller is about to dispatch to this account, so a
// half-open breaker admits exactly ONE in-flight probe. It is the mutating half
// of the pair whose read half is isOpen, and it must be called only at a real
// dispatch commit point — never from an eligibility gate, or the probe is spent
// on an account that is then not selected.
//
// Callers hold the pool write lock, so the claim is serialized across
// concurrent selections; the breaker's own mutex guards the fields themselves.
// A no-op for a closed breaker: nothing is being probed.
func (cb *circuitBreaker) claimProbe(now time.Time) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case circuitOpen:
		if now.Sub(cb.openedAt) >= circuitOpenDuration {
			cb.state = circuitHalfOpen
			cb.probeAt = now
		}
	case circuitHalfOpen:
		if now.Sub(cb.probeAt) >= circuitOpenDuration {
			cb.probeAt = now
		}
	}
}

// recordError advances the breaker on a failed request.
func (cb *circuitBreaker) recordError(now time.Time) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecutiveErr++
	if cb.state == circuitHalfOpen {
		cb.state = circuitOpen // probe failed → re-open
		cb.openedAt = now
		cb.probeAt = time.Time{} // the probe reported back; clear it
	} else if cb.consecutiveErr >= circuitErrorThreshold && cb.state == circuitClosed {
		cb.state = circuitOpen
		cb.openedAt = now
	}
}

// reset closes the breaker after a successful request.
func (cb *circuitBreaker) reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = circuitClosed
	cb.consecutiveErr = 0
	cb.probeAt = time.Time{} // the probe succeeded; clear it
}

// accountHealth tracks per-account health signals used by score-weighted
// selection: EWMA latency and a decaying EWMA error rate.
type accountHealth struct {
	ewmaLatencyMs float64 // EWMA latency (α=healthEWMAAlpha)
	ewmaErrorRate float64 // EWMA error rate in [0,1]; 1 per error, 0 per success
	samples       int     // number of recorded observations
}

// apiKeyBinding binds an API key to a preferred account for session affinity.
type apiKeyBinding struct {
	accountID string
	lastUsed  time.Time
}

// isCircuitOpen reports whether an account's circuit breaker is currently open
// (and should be skipped). It transitions open→half-open after the open
// duration elapses, allowing a single probe through. Safe to call without
// holding p.mu — it takes its own RLock for the map read, and the breaker's own
// mutex guards the state transition.
func (p *AccountPool) isCircuitOpen(id string, now time.Time) bool {
	p.mu.RLock()
	cb := p.circuitState[id]
	p.mu.RUnlock()
	return cb != nil && cb.isOpen(now)
}

// AccountPool 账号池
type AccountPool struct {
	mu              sync.RWMutex
	accounts        []config.Account
	totalAccounts   int
	lastDispatchSeq map[string]uint64          // accountID → last dispatch sequence (lower = used longer ago; LRU clock)
	dispatchSeq     uint64                     // monotonically increases per dispatch (stable LRU ordering)
	cooldowns       map[string]time.Time       // 账号冷却时间
	errorCounts     map[string]int             // 连续错误计数
	modelLists      map[string]map[string]bool // accountID → set of modelIDs (from ListAvailableModels)
	allowLists      map[string][]string        // accountID → per-account model allow-list (from config; empty slice = no restriction)
	reprobeBackoff  map[string]time.Duration   // accountID → next backoff interval
	reprobeNext     map[string]time.Time       // accountID → when to next probe
	stopRecover     chan struct{}
	circuitState    map[string]*circuitBreaker // accountID → circuit breaker state
	healthStats     map[string]*accountHealth  // accountID → EWMA latency + error/success counts
	apiKeyAffinity  map[string]apiKeyBinding   // apiKeyID → preferred account (sticky routing)
	// pendingWrites tracks the detached config-persistence goroutines started by
	// UpdateStats. Stats are written off the request path deliberately (a config
	// Save must not add latency to a proxied response), which leaves a write in
	// flight after the call returns. Tests use WaitForPendingWrites to drain them
	// before tearing down a temp config dir; production never needs to wait.
	pendingWrites sync.WaitGroup
}

// WaitForPendingWrites blocks until every detached stats-persistence goroutine
// started by UpdateStats has finished. Intended for tests that point config at a
// temporary directory: without draining, a late Save() re-creates config files
// while the harness is removing that directory ("directory not empty" cleanup
// failures) and can outlive the test that owns the data.
func (p *AccountPool) WaitForPendingWrites() {
	p.pendingWrites.Wait()
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool 获取全局账号池单例
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:       make(map[string]time.Time),
			lastDispatchSeq: make(map[string]uint64),
			errorCounts:     make(map[string]int),
			modelLists:      make(map[string]map[string]bool),
			allowLists:      make(map[string][]string),
			reprobeBackoff:  make(map[string]time.Duration),
			reprobeNext:     make(map[string]time.Time),
			circuitState:    make(map[string]*circuitBreaker),
			healthStats:     make(map[string]*accountHealth),
			apiKeyAffinity:  make(map[string]apiKeyBinding),
		}
		pool.Reload()
		if config.GetAutoRecoverEnabled() {
			pool.startAutoRecover()
		}
	})
	return pool
}

// Reload rebuilds the account list from config (one entry per account; weight is
// handled as selection probability by healthScore, not as duplicated slots).
// Over-quota accounts are dropped unless either the per-account upstream
// Overages switch (OverageStatus=ENABLED) or the global AllowOverUsage
// setting permits over-quota routing.
func (p *AccountPool) Reload() {
	// Read config (takes cfgLock) BEFORE acquiring p.mu so the pool lock never
	// nests cfgLock — see GetNextForModelExcluding for the ordering rationale.
	enabled := config.GetEnabledAccounts()
	// The prune below must know every CONFIGURED account, not just the routable
	// ones. Keying it off `enabled` deleted the cooldown and breaker of any
	// account that is merely disabled — including the 24h safety-net cooldown
	// DisableAccount sets immediately before calling Reload, which erased itself
	// on the very next line (TestDisableAccountSetsCooldown).
	configured := config.GetAccounts()
	allowOverUsage := config.GetAllowOverUsage()

	p.mu.Lock()
	defer p.mu.Unlock()
	var accounts []config.Account
	// Rebuild per-account model allow-lists from config (single source of truth),
	// normalized lower-case for case-insensitive routing checks. An empty/absent
	// list means "no restriction".
	allowLists := make(map[string][]string, len(enabled))
	for _, a := range enabled {
		if len(a.ModelAllowList) > 0 {
			norm := make([]string, 0, len(a.ModelAllowList))
			for _, m := range a.ModelAllowList {
				m = strings.ToLower(strings.TrimSpace(m))
				if m != "" {
					norm = append(norm, m)
				}
			}
			if len(norm) > 0 {
				allowLists[a.ID] = norm
			}
		}
		if isQuotaBlocked(a, allowOverUsage) {
			continue
		}
		accounts = append(accounts, a) // one entry per account (weight handled by score)
	}
	p.accounts = accounts
	p.allowLists = allowLists
	p.totalAccounts = len(enabled)

	// Drop per-account routing state for accounts that no longer exist in
	// config. Two reasons this matters, both observed as real behaviour:
	//
	//   1. Unbounded growth. Every add/remove cycle leaves a permanent entry in
	//      circuitState, lastDispatchSeq, cooldowns, errorCounts, healthStats
	//      and modelLists, keyed by an ID nothing will ever look up again.
	//   2. Worse, IDs are reusable. Deleting an account and re-adding it with
	//      the same ID silently inherits the old breaker (possibly OPEN, so the
	//      fresh account is unroutable), the old cooldown, the old error count
	//      and a stale LRU sequence. An operator re-adding a credential to fix
	//      a problem would get an account that looks broken for no visible
	//      reason.
	//
	// Keyed off every CONFIGURED account (not `enabled`, and not the
	// post-quota-filter `accounts`) so an account that is merely disabled or
	// temporarily over quota keeps its cooldown, breaker and health history.
	// Only an account genuinely gone from config loses its state.
	live := make(map[string]bool, len(configured))
	for _, a := range configured {
		live[a.ID] = true
	}
	for id := range p.circuitState {
		if !live[id] {
			delete(p.circuitState, id)
		}
	}
	for id := range p.lastDispatchSeq {
		if !live[id] {
			delete(p.lastDispatchSeq, id)
		}
	}
	for id := range p.cooldowns {
		if !live[id] {
			delete(p.cooldowns, id)
		}
	}
	for id := range p.errorCounts {
		if !live[id] {
			delete(p.errorCounts, id)
		}
	}
	for id := range p.healthStats {
		if !live[id] {
			delete(p.healthStats, id)
		}
	}
	for id := range p.modelLists {
		if !live[id] {
			delete(p.modelLists, id)
		}
	}
	// Affinity bindings that point at a removed account would otherwise pin a
	// session to an ID that can never be selected again; the binding is dead
	// weight and the affinity path would fall through on every request.
	for key, binding := range p.apiKeyAffinity {
		if !live[binding.accountID] {
			delete(p.apiKeyAffinity, key)
		}
	}
}

// GetNext 获取下一个可用账号（加权轮询）
func (p *AccountPool) GetNext() *config.Account {
	return p.GetNextExcluding(nil)
}

// GetNextExcluding 获取下一个可用账号（LRU 轮询，最少最近使用），并跳过指定账号。
// Delegates to GetNextForModelExcluding with model="" (any model).
func (p *AccountPool) GetNextExcluding(excluded map[string]bool) *config.Account {
	return p.GetNextForModelExcluding("", excluded)
}

// SetModelList 缓存账号支持的模型集合（由 handler 在刷新后调用）
func (p *AccountPool) SetModelList(accountID string, modelIDs []string) {
	set := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		set[strings.ToLower(strings.TrimSpace(id))] = true
	}
	p.mu.Lock()
	if p.modelLists == nil {
		p.modelLists = make(map[string]map[string]bool)
	}
	p.modelLists[accountID] = set
	p.mu.Unlock()
}

// ClearModelList drops the cached model set for an account. Used when a region
// override changes: the old region's model list must not keep the account
// marked model-capable until a successful refresh in the new region. After this,
// accountHasModel falls back to optimistic cold-start behavior (the allow-list,
// if any, still applies) until the next SetModelList populates the new region's
// models.
func (p *AccountPool) ClearModelList(accountID string) {
	p.mu.Lock()
	delete(p.modelLists, accountID)
	p.mu.Unlock()
}

// GetModelList 返回该账号缓存的模型 ID 列表（供 admin API 使用）。
// 若尚无缓存则返回空切片。
func (p *AccountPool) GetModelList(accountID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set, ok := p.modelLists[accountID]
	if !ok || len(set) == 0 {
		return []string{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

// accountHasModel 检查账号是否支持指定模型。
// 若该账号尚无模型列表（冷启动），视为支持所有模型。
//
// A per-account model allow-list (config ModelAllowList) is enforced FIRST and
// unconditionally: if the account has a non-empty allow-list and the model is
// not on it, the account is never routed that model — even during cold start
// (before the upstream model cache is populated). This is what lets an operator
// pin a model to specific accounts.
func (p *AccountPool) accountHasModel(accountID, model string) bool {
	want := strings.ToLower(strings.TrimSpace(model))
	if allow, ok := p.allowLists[accountID]; ok && len(allow) > 0 {
		permitted := false
		for _, m := range allow {
			if m == want {
				permitted = true
				break
			}
		}
		if !permitted {
			return false
		}
	}
	list, ok := p.modelLists[accountID]
	if !ok || len(list) == 0 {
		return true // 冷启动：列表未就绪，乐观放行
	}
	return list[want]
}

// GetNextForModel 获取下一个支持指定模型的可用账号。
// model 应为去掉 thinking 后缀的实际模型名。
// 若无账号有该模型列表数据，行为与 GetNext 相同（乐观路由）。
func (p *AccountPool) GetNextForModel(model string) *config.Account {
	return p.GetNextForModelExcluding(model, nil)
}

// GetNextForModelExcluding selects an account supporting the model using
// least-recently-used (LRU) selection: the eligible account dispatched longest
// ago is chosen first, which interleaves requests evenly across healthy
// accounts (replacing score-weighted random, whose EWMA-driven skew
// concentrated load on a few "lucky" accounts and starved the rest). Health
// score acts as a tie-breaker when several accounts share the lowest dispatch
// sequence (cold start with several never-dispatched accounts). Skips
// excluded, cooled-down, circuit-open, token-expiring, and quota-blocked
// accounts. model="" means "any model".
func (p *AccountPool) GetNextForModelExcluding(model string, excluded map[string]bool) *config.Account {
	// Read config (takes cfgLock) BEFORE acquiring p.mu so the pool lock never
	// nests cfgLock. Holding p.mu while blocking on cfgLock would freeze every
	// other pool operation behind the write lock if a config writer is mid-Save
	// (synchronous os.WriteFile). Keeping cfgLock a strict leaf preserves the
	// global order tokenRefreshMu → p.mu → refreshLockFor → cfgLock.
	allowOverUsage := config.GetAllowOverUsage()
	// Hoisted for the same reason as allowOverUsage. This read used to sit at the
	// quota-aware branch below, INSIDE p.mu: the hoist above only narrows the
	// window, it does not close it, because a config writer can acquire cfgLock
	// in the gap between these two reads and then this one blocks with the pool
	// lock already held.
	quotaAware := config.GetQuotaAwareRouting()

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.accounts) == 0 {
		return nil
	}

	now := time.Now()

	// Quota-aware routing: prefer the model-capable account with the most
	// remaining quota. Falls back to LRU selection below when none has usable
	// data. pickQuotaAware already returns a copy.
	if quotaAware {
		if acc := p.pickQuotaAware(model, excluded, now, allowOverUsage); acc != nil {
			// Stamp the LRU clock even though this pick did not use it. Every
			// other dispatch path stamps (the LRU pick below and the
			// cooldown fallback both do), and skipping it here leaves a stale
			// sequence behind: the moment quota data goes absent or the toggle
			// is turned off, LRU selection sees this account as
			// least-recently-used and hands it a burst of consecutive requests.
			// Keeping the clock authoritative across both modes makes the
			// toggle safe to flip at runtime.
			p.dispatchSeq++
			if p.lastDispatchSeq == nil {
				p.lastDispatchSeq = make(map[string]uint64)
			}
			p.lastDispatchSeq[acc.ID] = p.dispatchSeq
			// Dispatch commit point: claim the half-open probe (see claimProbe).
			if cb := p.circuitState[acc.ID]; cb != nil {
				cb.claimProbe(now)
			}
			return acc
		}
	}

	// Build candidate list: each eligible account paired with its last
	// dispatch sequence (LRU key) and health score (tie-breaker).
	type candidate struct {
		acc     *config.Account
		lastSeq uint64
		score   float64
	}
	var candidates []candidate
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if model != "" && !p.accountHasModel(acc.ID, model) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		if cb := p.circuitState[acc.ID]; cb != nil && cb.isOpen(now) {
			continue
		}
		// Do NOT skip a token that is near or past expiry. The request handler
		// calls ensureValidToken on the selected account and refreshes it before
		// use, so an expiring account is still perfectly serviceable — while
		// skipping it here removes the only path that would ever refresh it,
		// and a pool whose tokens all aged out would go dark instead of
		// self-healing. Locked in by
		// TestGetNext/GetNextForModelKeepsExpiringTokenAvailableForRequestRefresh.
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		candidates = append(candidates, candidate{
			acc:     acc,
			lastSeq: p.lastDispatchSeq[acc.ID],
			score:   p.healthScore(acc.ID, effectiveWeight(acc.Weight)),
		})
	}

	if len(candidates) == 0 {
		// Fallback: return the account with the earliest cooldown. Stamp the
		// LRU clock so a later healthy pick doesn't immediately re-select this
		// account (every other dispatch path stamps; the fallback must too, or
		// a degraded account's stale seq wins the next pick and gets a burst).
		//
		// This is the other half of the cooldown contract documented on
		// RecordError: because a quota error parks an account for a full hour,
		// refusing to dispatch during that window would take a single-account
		// pool completely dark. Serving the account whose cooldown expires
		// soonest keeps the pool alive and lets the upstream be authoritative
		// about whether the quota has actually reset.
		acc := p.fallbackEarliestCooldown(model, excluded, allowOverUsage, now)
		if acc != nil {
			p.dispatchSeq++
			if p.lastDispatchSeq == nil {
				p.lastDispatchSeq = make(map[string]uint64)
			}
			p.lastDispatchSeq[acc.ID] = p.dispatchSeq
			// Dispatch commit point: claim the half-open probe (see claimProbe).
			if cb := p.circuitState[acc.ID]; cb != nil {
				cb.claimProbe(now)
			}
		}
		return acc
	}

	// Least-recently-used: pick the candidate dispatched longest ago (lowest
	// sequence; never-dispatched accounts share the zero value). Ties are broken
	// by health score (higher preferred), then at random so a cold pool spreads
	// its first picks instead of always hitting the first account. A monotonic
	// sequence counter (not wall-clock) keys the ordering, so two accounts can
	// never share a key — LRU stays exact round-robin regardless of clock
	// resolution.
	oldest := candidates[0].lastSeq
	for _, c := range candidates[1:] {
		if c.lastSeq < oldest {
			oldest = c.lastSeq
		}
	}
	var top []candidate
	topScore := -1.0
	for _, c := range candidates {
		if c.lastSeq != oldest {
			continue
		}
		if len(top) == 0 || c.score > topScore {
			top = []candidate{c}
			topScore = c.score
		} else if c.score == topScore {
			top = append(top, c)
		}
	}
	chosen := top[rand.Intn(len(top))].acc

	// Advance the monotonic LRU clock and stamp the chosen account so the next
	// pick goes to a different one.
	p.dispatchSeq++
	if p.lastDispatchSeq == nil {
		p.lastDispatchSeq = make(map[string]uint64)
	}
	p.lastDispatchSeq[chosen.ID] = p.dispatchSeq

	// Claim the half-open probe for the account we are actually dispatching to.
	// Done here, at the commit point, rather than in the eligibility gates
	// above: those evaluate several accounts (and evaluate the same account
	// more than once), so claiming there would spend the probe on a candidate
	// that loses the LRU tie-break and never receives a request.
	if cb := p.circuitState[chosen.ID]; cb != nil {
		cb.claimProbe(now)
	}

	// Return a COPY, never &p.accounts[i]. Callers mutate the returned account
	// without holding the pool lock (proxy's ensureValidToken assigns
	// AccessToken/RefreshToken/ExpiresAt), which raced with UpdateToken writing
	// the same fields under p.mu.Lock (proven -race DATA RACE). Persistence does
	// not depend on the alias: every mutation site also calls pool.UpdateToken
	// plus config.UpdateAccountToken, and Reload() rebuilds pool storage from
	// config. GetByID and pickQuotaAware copy for the same reason.
	selected := *chosen
	return &selected
}

// GetByID 根据 ID 获取账号
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			account := p.accounts[i]
			return &account
		}
	}
	return nil
}

// hasAccountLocked reports whether id is still a routable member of the pool.
// Caller must hold p.mu (read or write). Exists so a caller that already holds
// the write lock can confirm membership without the lock upgrade GetByID's own
// RLock would require (which would deadlock).
func (p *AccountPool) hasAccountLocked(id string) bool {
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			return true
		}
	}
	return false
}

// GetNextForModelWithApiKey selects an account for a request, preferring the one
// already bound to the API key (session affinity). Falls back to health-aware
// scoring if the bound account is unavailable or affinity is disabled.
func (p *AccountPool) GetNextForModelWithApiKey(model string, excluded map[string]bool, apiKey string) *config.Account {
	// Hoist config reads ABOVE any pool lock (same anti-pattern fixed in 58727ec
	// for GetNextForModelExcluding) — never call config.* under p.mu, or a config
	// Save() mid-write nests cfgLock under the pool lock and freezes every other
	// pool operation. allowOverUsage feeds the quota gate below.
	allowOverUsage := config.GetAllowOverUsage()

	// Session affinity: try the bound account first.
	if apiKey != "" && config.GetSessionAffinityEnabled() {
		p.mu.RLock()
		binding, ok := p.apiKeyAffinity[apiKey]
		p.mu.RUnlock()
		if ok && time.Since(binding.lastUsed) < sessionAffinityTTL {
			// Check if the bound account is available.
			acc := p.GetByID(binding.accountID)
			if acc != nil && acc.Enabled {
				isExcluded := excluded != nil && excluded[acc.ID]
				p.mu.RLock()
				cooldown, hasCooldown := p.cooldowns[acc.ID]
				// Mirror the normal-path eligibility gates (GetNextForModelExcluding):
				// model support (accountHasModel reads p.modelLists with no lock of its
				// own, so it MUST run under p.mu.RLock) and quota. isQuotaBlocked is a
				// pure function on the account value, but we read it here under the same
				// lock to keep both gates consistent with one snapshot.
				// Token-near-expiry is INTENTIONALLY NOT mirrored: a near-expiry token is
				// still valid within refresh-skew and the handler refreshes it; gating on
				// it would rebind the session to a different account every refresh window.
				hasModel := model == "" || p.accountHasModel(acc.ID, model)
				quotaBlocked := isQuotaBlocked(*acc, allowOverUsage)
				p.mu.RUnlock()
				cooldownActive := hasCooldown && time.Now().Before(cooldown)
				if !isExcluded && !cooldownActive && !p.isCircuitOpen(acc.ID, time.Now()) && hasModel && !quotaBlocked {
					now := time.Now()
					p.mu.Lock()
					// Re-verify membership under the SAME write lock that commits
					// the dispatch. Every gate above ran in its own short critical
					// section (binding read, GetByID copy, cooldown/model/quota
					// read, breaker check), so a Reload can complete in any of the
					// gaps between them — deleting or disabling this account — and
					// the pre-fix code still returned the stale copy it captured
					// before that Reload. The proxy would then dispatch one more
					// request on a credential the completed reload had already
					// removed from routing.
					//
					// Committing and validating under one lock closes every window
					// at once: if the account is gone we drop the binding and fall
					// through to normal selection, which costs one re-pick and
					// never a failed request.
					if !p.hasAccountLocked(acc.ID) {
						delete(p.apiKeyAffinity, apiKey)
						p.mu.Unlock()
						acc = nil
					} else {
						binding.lastUsed = now
						p.dispatchSeq++
						if p.lastDispatchSeq == nil {
							p.lastDispatchSeq = make(map[string]uint64)
						}
						p.lastDispatchSeq[acc.ID] = p.dispatchSeq
						p.apiKeyAffinity[apiKey] = binding
						// Dispatch commit point: claim the half-open probe
						// (see claimProbe).
						if cb := p.circuitState[acc.ID]; cb != nil {
							cb.claimProbe(now)
						}
						p.mu.Unlock()
						return acc
					}
				}
			}
		}
	}

	// Fall back to normal selection.
	acc := p.GetNextForModelExcluding(model, excluded)
	if acc != nil && apiKey != "" && config.GetSessionAffinityEnabled() {
		now := time.Now()
		p.mu.Lock()
		// A write to a nil Go map panics, and this map is only populated by
		// GetPool(); a pool assembled field-by-field (Reload and selection are
		// both driven that way) reaches here with it nil. Without this the
		// first affinity bind would take the whole proxy process down instead
		// of degrading to "no affinity" — every other optional map on the hot
		// path is already guarded the same way (see lastDispatchSeq below).
		if p.apiKeyAffinity == nil {
			p.apiKeyAffinity = make(map[string]apiKeyBinding)
		}
		if len(p.apiKeyAffinity) >= maxAffinityEntries {
			p.pruneExpiredAffinityLocked(now)
			// Expiry-only pruning is not a bound. Every distinct API key seen
			// inside one TTL window keeps its binding alive, so a caller that
			// rotates keys (or an attacker sending one request per random key)
			// grows this map without limit while maxAffinityEntries silently
			// does nothing — the pre-existing check could only ever free
			// already-stale entries. When pruning cannot get us back under the
			// cap, evict the least-recently-used bindings, which is exactly the
			// data affinity is allowed to lose: dropping a binding costs one
			// re-pick through normal selection, never a failed request.
			if len(p.apiKeyAffinity) >= maxAffinityEntries {
				p.evictOldestAffinityLocked(maxAffinityEntries - 1)
			}
		}
		p.apiKeyAffinity[apiKey] = apiKeyBinding{accountID: acc.ID, lastUsed: now}
		p.mu.Unlock()
	}
	return acc
}

// evictOldestAffinityLocked shrinks the affinity map to at most target entries by
// repeatedly dropping the least-recently-used binding. Caller must hold p.mu.
//
// The scan is O(n) per eviction and runs with the pool write lock held, so the
// cost was measured rather than assumed: ~20.6µs per eviction at the 1024-entry
// cap (BenchmarkEvictOldestAffinityAtCap). That is negligible against the Kiro
// round-trip the selected account is about to make, and it is only reached at
// all when a client rotates API keys fast enough to keep 1024 bindings fresh
// inside the 10-minute TTL — i.e. the abuse case whose alternative was letting
// the map grow without limit. Steady-state traffic never enters this path.
//
// If that ever stops being true, the fix is a heap or an intrusive LRU list, not
// a larger cap.
func (p *AccountPool) evictOldestAffinityLocked(target int) {
	if target < 0 {
		target = 0
	}
	for len(p.apiKeyAffinity) > target {
		var oldestKey string
		var oldestAt time.Time
		first := true
		for key, binding := range p.apiKeyAffinity {
			if first || binding.lastUsed.Before(oldestAt) {
				oldestKey = key
				oldestAt = binding.lastUsed
				first = false
			}
		}
		if first {
			return // map is empty; nothing left to evict
		}
		delete(p.apiKeyAffinity, oldestKey)
	}
}

// pruneExpiredAffinityLocked removes session-affinity bindings whose TTL has
// elapsed, keeping the map from growing unbounded with one entry per distinct
// API key ever seen. Caller must hold p.mu (write lock).
func (p *AccountPool) pruneExpiredAffinityLocked(now time.Time) {
	for key, binding := range p.apiKeyAffinity {
		if now.Sub(binding.lastUsed) >= sessionAffinityTTL {
			delete(p.apiKeyAffinity, key)
		}
	}
}

// RecordSuccess 记录请求成功，清除冷却
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Don't wipe a hard (quota/overage/disable) backoff on a late in-flight
	// success — only clear transient (3-error, +1min) cooldowns. A quota backoff
	// (+1h) must persist to its natural expiry or the exhausted upstream gets
	// re-selected immediately.
	if cd, ok := p.cooldowns[id]; ok && time.Until(cd) > cooldownClearThreshold {
		// keep the hard cooldown
	} else {
		delete(p.cooldowns, id)
	}
	p.errorCounts[id] = 0
	// Circuit breaker: reset on success.
	if cb := p.circuitState[id]; cb != nil {
		cb.reset()
	}
	// Health stats: a success decays the error rate toward 0 without erasing
	// recent failures (so a previously-flapping account stays slightly penalised).
	p.recordHealthObservation(id, false)
}

// cooldownClearThreshold is the remaining duration below which RecordSuccess
// will clear a cooldown. Transient (3-error) cooldowns are +1min; quota/overage
// backoffs are +1h. A late in-flight success must not wipe a hard backoff —
// only short transient ones — or the exhausted upstream gets re-selected.
const cooldownClearThreshold = 10 * time.Minute

// setCooldownIfLater sets the cooldown to newExpiry only if it extends the
// existing one (or none exists), so a transient short backoff (the 3-error
// +1min cooldown) can never shorten a longer quota/overage backoff (+1h) for
// the same account. Caller must hold p.mu.
func setCooldownIfLater(cooldowns map[string]time.Time, id string, newExpiry time.Time) {
	if ex, ok := cooldowns[id]; !ok || newExpiry.After(ex) {
		cooldowns[id] = newExpiry
	}
}

// RecordError records a request error for the account: it advances the
// consecutive-error count (reset on success by RecordSuccess), applies a local
// cooldown, advances the circuit breaker, and raises the EWMA error rate used by
// health scoring.
//
// MERGE POLICY NOTE. The two lineages shipped contradictory contracts here and
// only one can hold:
//
//	origin/main: quota error -> 1h cooldown, 3 consecutive errors -> 1min
//	             cooldown, and GetNextForModelExcluding falls back to the
//	             earliest-cooldown account when nothing else is eligible.
//	harry:       RecordError counts only (no cooldown), and the pool returns nil
//	             rather than ever handing back a cooling account.
//
// origin/main's pair is kept because the two halves depend on each other: the 1h
// quota backoff matches how an upstream 429 actually clears (hourly quota reset,
// not the breaker's 30s window), and the fallback is what stops a small pool
// from going fully dark while that backoff runs. setCooldownIfLater never
// shortens an existing longer cooldown, so the transient 1min branch cannot
// clobber a quota hour (TestRecordErrorDoesNotShortenQuotaCooldown).
//
// The circuit breaker and EWMA health signals from origin/main are additive and
// kept as well: the breaker parks an account that fails repeatedly for a short
// window regardless of cooldown, and health score biases selection away from it.
func (p *AccountPool) RecordError(id string, isQuotaError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	p.errorCounts[id]++

	if isQuotaError {
		// 配额错误，冷却 1 小时（不缩短已有的更长冷却）
		setCooldownIfLater(p.cooldowns, id, now.Add(time.Hour))
	} else if p.errorCounts[id] >= 3 {
		// 连续 3 次错误，冷却 1 分钟（不缩短已有的更长冷却）
		setCooldownIfLater(p.cooldowns, id, now.Add(time.Minute))
	}

	// Circuit breaker: track consecutive errors.
	cb := p.circuitState[id]
	if cb == nil {
		cb = &circuitBreaker{}
		if p.circuitState == nil {
			p.circuitState = make(map[string]*circuitBreaker)
		}
		p.circuitState[id] = cb
	}
	cb.recordError(now)

	// Health stats: raise the EWMA error rate.
	p.recordHealthObservation(id, true)
}

// recordHealthObservation updates the account's EWMA error rate. isError=true
// pushes the rate toward 1, false decays it toward 0. Caller must hold p.mu.
func (p *AccountPool) recordHealthObservation(id string, isError bool) {
	if p.healthStats == nil {
		p.healthStats = make(map[string]*accountHealth)
	}
	h := p.healthStats[id]
	if h == nil {
		h = &accountHealth{}
		p.healthStats[id] = h
	}
	h.samples++
	sample := 0.0
	if isError {
		sample = 1.0
	}
	h.ewmaErrorRate = healthEWMAAlpha*sample + (1-healthEWMAAlpha)*h.ewmaErrorRate
}

// IsAuthFailure reports whether an error indicates the refresh token / credentials
// have been revoked or invalidated upstream (401, 403 with auth markers, etc.).
// These accounts cannot be recovered automatically and must be re-authenticated.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	// Decide from the FIRST status token in the message, not from whichever code
	// happens to appear anywhere in it. Every formatter in this repo writes the
	// authoritative status at the front and appends the opaque upstream body
	// after it, so position is what separates "the status upstream returned"
	// from "a number quoted inside a response body".
	//
	// Scanning for any 5xx anywhere (the previous shape) misclassified a genuine
	// revoked credential whose body merely quoted a server error:
	//
	//	refresh failed: 400 {"error":"invalid_grant","upstream returned 500"}
	//
	// hit the 5xx gate and returned false, so classifyAndBanOnUsageError never
	// saw an auth failure and the account was never flagged for re-auth — the
	// mirror image of the false-ban this gate exists to prevent, and it left
	// this classifier disagreeing with proxy's isAuthErrorMessage on the same
	// string.
	// A 401/403 anywhere is positive evidence on its own: only the credential
	// can produce it, and callers upstream of this classifier format such
	// errors in several shapes ("received 403 Forbidden" has no HTTP-status
	// word in front of it). Requiring status CONTEXT here would silently stop
	// detecting those, so this check keeps the original boundary-token rule.
	if HasStatusToken(msg, "401") || HasStatusToken(msg, "403") {
		return true
	}

	// The 5xx SUPPRESSION is the branch that needs to be conservative, because
	// it can only ever *clear* a credential failure. Two independent conditions
	// must hold before a 5xx is allowed to veto the markers below:
	//
	//  1. The number is presented AS an HTTP status (hasStatusContextBefore).
	//     Error strings routinely carry unrelated integers in 400-599 — credit
	//     counters ("usage 512/1000 credits"), elapsed seconds, balances,
	//     sequence numbers. Reading one as a server status inverts the
	//     classifier: a genuinely revoked credential is filed as an outage, so
	//     the account is never flagged for re-auth and keeps being routed while
	//     every request fails. That is the mirror of the false ban this gate
	//     prevents, and harder to spot — a false ban shows in the admin UI, a
	//     missed revocation just looks like an account that keeps erroring.
	//
	//  2. It is the EARLIEST status in the message. Every formatter writes the
	//     authoritative status at the front and appends the opaque upstream body
	//     after it, so a 400 whose body quotes "upstream returned 500" is a
	//     revoked credential, not an outage.
	if status, ok := firstUpstreamStatusToken(lower); ok && status >= 500 {
		return false
	}
	if strings.Contains(lower, "bad credentials") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid grant") ||
		strings.Contains(lower, "invalid_token") ||
		strings.Contains(lower, "invalid token") ||
		strings.Contains(lower, "token expired") ||
		strings.Contains(lower, "token has expired") ||
		strings.Contains(lower, "unauthorized") {
		return true
	}
	return false
}

// HasStatusToken returns true when status appears in s with non-alphanumeric
// boundaries on both sides, so "401" matches "HTTP 401 from ..." but not
// "4011", "14013", or an alphanumeric token like "request_401abc". Exported so
// the proxy package can match upstream status codes by the same boundary rule
// (quota/overage classifiers) instead of bare strings.Contains on the body.
func HasStatusToken(s, status string) bool {
	for {
		idx := strings.Index(s, status)
		if idx < 0 {
			return false
		}
		leftOK := idx == 0 || !isAlphaNum(s[idx-1])
		rightIdx := idx + len(status)
		rightOK := rightIdx >= len(s) || !isAlphaNum(s[rightIdx])
		if leftOK && rightOK {
			return true
		}
		s = s[idx+len(status):]
	}
}

// firstUpstreamStatusToken returns the EARLIEST HTTP status code (4xx/5xx)
// appearing in s as a standalone token, and whether one was found.
//
// Position matters: the authoritative status is written at the front of the
// message by whichever formatter produced it, and everything after it is an
// opaque upstream body that may quote unrelated status codes. Returning the
// leftmost token keeps the header authoritative.
//
// The boundary rule is HasStatusToken's, so a request ID like "req_5031" cannot
// masquerade as a 503. Codes are enumerated rather than pattern-matched so the
// pool package keeps its current import set (no regexp), and the enumeration
// covers 400-599 because a 4xx must be distinguishable from a 5xx here rather
// than merely "not 5xx".
func firstUpstreamStatusToken(s string) (int, bool) {
	bestIdx := -1
	bestStatus := 0
	for code := 400; code <= 599; code++ {
		token := strconv.Itoa(code)
		offset := 0
		for {
			idx := statusTokenIndexFrom(s, token, offset)
			if idx < 0 {
				break
			}
			// Only count a number that is INTRODUCED as an HTTP status. Error
			// strings routinely carry unrelated integers in 400-599 — credit
			// counters ("usage 512/1000 credits"), elapsed seconds ("expired
			// 540 seconds ago"), balances, sequence numbers — and reading one
			// of those as a server status inverts this classifier: a genuinely
			// revoked credential is filed as an upstream outage, so the
			// account is never flagged for re-auth and keeps being routed
			// while every request fails. That is the mirror of the false ban
			// the 5xx gate prevents, and harder to notice: a false ban is
			// visible in the admin UI, a missed revocation just looks like an
			// account that keeps erroring.
			if hasStatusContextBefore(s, idx) {
				if bestIdx < 0 || idx < bestIdx {
					bestIdx = idx
					bestStatus = code
				}
				break
			}
			offset = idx + len(token)
		}
	}
	if bestIdx < 0 {
		return 0, false
	}
	return bestStatus, true
}

// statusContextWords are the words that precede a real HTTP status in the error
// strings this repo produces. They mirror proxy's upstreamStatusPatterns:
//
//	"HTTP 500 from kiro: <body>"                    -> http
//	"refresh failed: 500 <body>"                    -> failed
//	"social token exchange failed (status 503): ..." -> status
//	"upstream status 502: <body>"                   -> status
//	"upstream returned 502: <body>"                 -> returned
var statusContextWords = []string{"http", "status", "returned", "failed"}

// hasStatusContextBefore reports whether the token starting at idx is preceded
// by a word that introduces an HTTP status. It skips intervening spaces and
// punctuation (':' and '(' appear in the real formats) and then compares the
// preceding word.
func hasStatusContextBefore(s string, idx int) bool {
	end := idx
	for end > 0 {
		c := s[end-1]
		if c == ' ' || c == '	' || c == ':' || c == '(' || c == '=' {
			end--
			continue
		}
		break
	}
	if end == 0 {
		return false
	}
	start := end
	for start > 0 && isAlphaNum(s[start-1]) {
		start--
	}
	if start == end {
		return false
	}
	word := strings.ToLower(s[start:end])
	for _, w := range statusContextWords {
		if word == w {
			return true
		}
	}
	return false
}

// statusTokenIndexFrom returns the index of the first occurrence of status at or
// after start that has non-alphanumeric boundaries on both sides, or -1. Same
// rule as HasStatusToken, but it reports WHERE the token is (so callers can
// compare positions) and accepts a start offset (so a caller can keep scanning
// past a match it rejected for lacking HTTP-status context).
func statusTokenIndexFrom(s, status string, start int) int {
	if start < 0 {
		start = 0
	}
	if start > len(s) {
		return -1
	}
	offset := start
	for {
		rel := strings.Index(s[offset:], status)
		if rel < 0 {
			return -1
		}
		idx := offset + rel
		leftOK := idx == 0 || !isAlphaNum(s[idx-1])
		rightIdx := idx + len(status)
		rightOK := rightIdx >= len(s) || !isAlphaNum(s[rightIdx])
		if leftOK && rightOK {
			return idx
		}
		offset = idx + len(status)
	}
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isAlphaNum(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// IsSuspensionError reports whether the error indicates the account has been
// temporarily suspended by upstream or has no available Kiro profile.
// Unlike auth failures (revoked credentials), these may be transient, but
// the account should be disabled until an operator re-enables it.
func IsSuspensionError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "temporarily_suspended") ||
		strings.Contains(lower, "temporarily suspended") ||
		strings.Contains(lower, "no available kiro profile")
}

// DisableAccount marks an account as disabled (auth revoked / unrecoverable),
// removes it from the in-memory pool so subsequent requests skip it, and
// persists the change via config.SetAccountBanStatus.
func (p *AccountPool) DisableAccount(id, reason string) {
	if err := config.SetAccountBanStatus(id, "DISABLED", reason); err != nil {
		// best effort — even if persistence fails, drop it from memory
		_ = err
	}
	// Stamp the safety-net cooldown BOTH before and after Reload, using
	// setCooldownIfLater so the second stamp is idempotent.
	//
	// Neither order works alone, which is why both stamps are here:
	//   - Stamping only BEFORE leaves the cooldown exposed to Reload's prune
	//     when the id is already gone from config (the prune keys off
	//     config.GetAccounts()).
	//   - Stamping only AFTER opens a window between Reload returning and the
	//     stamp landing, during which a concurrent selection sees an account
	//     that is in p.accounts with no cooldown — i.e. fully routable, which
	//     is exactly what this call exists to prevent.
	//
	// Stamping on both sides closes the window and survives the prune for
	// either id shape.
	now := time.Now()
	p.mu.Lock()
	setCooldownIfLater(p.cooldowns, id, now.Add(24*time.Hour))
	p.mu.Unlock()
	p.Reload()
	p.mu.Lock()
	setCooldownIfLater(p.cooldowns, id, now.Add(24*time.Hour))
	p.mu.Unlock()
}

// MarkOverLimit marks an account as over usage limit (after a 402 / OVERAGE response).
// With the upstream OverageStatus model, the live status is refreshed via
// FetchOverageStatus from the request handler; here we just cooldown briefly so
// the next attempt picks a different account, then reload.
func (p *AccountPool) MarkOverLimit(id string) {
	// Stamped on both sides of Reload for the same reason as DisableAccount:
	// before, so no window exists where the account is reloaded but not yet
	// cooled; after, so the prune cannot drop it for a config-absent id.
	//
	// setCooldownIfLater rather than a raw assignment: a raw write here
	// SHORTENED an existing longer backoff, so a 402 arriving while a 1h quota
	// cooldown (or the 24h disable safety net) was already in force pulled the
	// account back into rotation early — the opposite of what marking it
	// over-limit is for.
	now := time.Now()
	p.mu.Lock()
	setCooldownIfLater(p.cooldowns, id, now.Add(time.Hour))
	p.mu.Unlock()
	p.Reload()
	p.mu.Lock()
	setCooldownIfLater(p.cooldowns, id, now.Add(time.Hour))
	p.mu.Unlock()
}

// UpdateToken 更新账号 Token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.UpdateCredentialState(nil, id, accessToken, refreshToken, expiresAt, "")
}

// UpdateCredentialState publishes one persisted refresh result to both the
// pool and an optional caller-owned account while holding the pool lock. The
// target may itself point into the pool.
func (p *AccountPool) UpdateCredentialState(
	target *config.Account,
	id string,
	accessToken string,
	refreshToken string,
	expiresAt int64,
	profileArn string,
) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.accounts[i].RefreshToken = refreshToken
			}
			p.accounts[i].ExpiresAt = expiresAt
			if profileArn != "" {
				p.accounts[i].ProfileArn = profileArn
			}
		}
	}
	if target != nil {
		target.AccessToken = accessToken
		if refreshToken != "" {
			target.RefreshToken = refreshToken
		}
		target.ExpiresAt = expiresAt
		if profileArn != "" {
			target.ProfileArn = profileArn
		}
	}
}

// Count 返回账号总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.totalAccounts > 0 {
		return p.totalAccounts
	}

	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		seen[acc.ID] = true
	}
	return len(seen)
}

// AvailableCount 返回可用账号数
func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	count := 0
	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		count++
	}
	return count
}

// UpdateStats 更新账号统计
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var updated bool
	var requestCount, errorCount, totalTokens int
	var totalCredits float64
	var lastUsed int64
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			if !updated {
				p.accounts[i].RequestCount++
				p.accounts[i].TotalTokens += tokens
				p.accounts[i].TotalCredits += credits
				p.accounts[i].LastUsed = time.Now().Unix()

				requestCount = p.accounts[i].RequestCount
				errorCount = p.accounts[i].ErrorCount
				totalTokens = p.accounts[i].TotalTokens
				totalCredits = p.accounts[i].TotalCredits
				lastUsed = p.accounts[i].LastUsed
				updated = true
				continue
			}
			p.accounts[i].RequestCount = requestCount
			p.accounts[i].ErrorCount = errorCount
			p.accounts[i].TotalTokens = totalTokens
			p.accounts[i].TotalCredits = totalCredits
			p.accounts[i].LastUsed = lastUsed
		}
	}
	if updated {
		// AddExternalPeriodOurCredit still ends in config.Save() (config.go:
		// AddExternalPeriodOurCredit), which rotates config.json.bak before
		// writing; two concurrent goroutines can interleave that rotate-then-write
		// and leave the backup set inconsistent. Keeping this detached keeps the
		// disk write off the request path, and running the pair sequentially in ONE
		// goroutine makes them a single ordered writer.
		//
		// UpdateAccountStats itself is now cheap (it only marks the config dirty;
		// the background stats saver coalesces the write via FlushDirty), so it is
		// no longer the reason this is detached — the credit accumulation is.
		periodCredits := credits
		p.pendingWrites.Add(1)
		go func() {
			defer p.pendingWrites.Done()
			config.UpdateAccountStats(id, requestCount, errorCount, totalTokens, totalCredits, lastUsed)
			// Accumulate the credits WE metered into the current external-usage
			// billing period. This is the local half of the external-usage audit;
			// the upstream half is captured on backgroundRefresh and compared in
			// ComputeExternalUsage.
			if periodCredits > 0 {
				config.AddExternalPeriodOurCredit(id, periodCredits)
			}
		}()
	}
}

// Diagnostics returns per-account routing state for all persisted accounts.
func (p *AccountPool) Diagnostics() []AccountDiagnostics {
	accounts := config.GetAccounts()
	return p.DiagnosticsFor(accounts)
}

// DiagnosticsFor returns per-account routing state for the supplied account set.
func (p *AccountPool) DiagnosticsFor(accounts []config.Account) []AccountDiagnostics {
	// Hoist the config read ABOVE p.mu — see GetNextForModelExcluding for the
	// ordering rationale. diagnosticsForLocked used to call
	// config.GetAllowOverUsage() itself, which nested cfgLock under the pool
	// lock: a config writer mid-Save (synchronous ~145KB file I/O) parked this
	// RLock behind disk it does not control, and every other pool operation —
	// dispatch, cooldown stamping, model-list updates — queued behind it.
	allowOverUsage := config.GetAllowOverUsage()

	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.diagnosticsForLocked(accounts, "", allowOverUsage)
}

// ModelRouting returns diagnostics scoped to one requested model.
func (p *AccountPool) ModelRouting(model string) ModelRoutingDiagnostics {
	return p.ModelRoutingFor(config.GetAccounts(), model)
}

// ModelRoutingFor returns model routing diagnostics for the supplied account set.
func (p *AccountPool) ModelRoutingFor(accounts []config.Account, model string) ModelRoutingDiagnostics {
	// Hoist the config read ABOVE p.mu — same rationale as DiagnosticsFor: the
	// shared callee must never take cfgLock while the pool lock is held.
	allowOverUsage := config.GetAllowOverUsage()

	p.mu.RLock()
	defer p.mu.RUnlock()
	model = strings.ToLower(strings.TrimSpace(model))
	hasAnyModelCache := false
	for _, set := range p.modelLists {
		if len(set) > 0 {
			hasAnyModelCache = true
			break
		}
	}
	items := p.diagnosticsForLocked(accounts, model, allowOverUsage)
	routeable := 0
	for _, item := range items {
		if item.Available {
			routeable++
		}
	}
	return ModelRoutingDiagnostics{
		Model:              model,
		HasAnyModelCache:   hasAnyModelCache,
		OptimisticFallback: !hasAnyModelCache,
		RouteableCount:     routeable,
		Accounts:           items,
	}
}

// ModelMatrixEntry describes one model's fleet-wide availability.
type ModelMatrixEntry struct {
	Model        string   `json:"model"`        // model ID
	AccountIDs   []string `json:"accountIds"`   // accounts whose cache lists this model
	CapableCount int      `json:"capableCount"` // len(AccountIDs)
}

// ModelMatrix returns the fleet model-availability grid: the union of every
// account's cached model set, and for each model the accounts that serve it.
// accountsWithCache is the number of accounts that have a non-empty model cache;
// when zero, routing is optimistic (no cache yet) and the matrix is empty.
// Models are returned sorted for stable UI ordering.
func (p *AccountPool) ModelMatrix() (entries []ModelMatrixEntry, accountsWithCache int) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	modelToAccounts := make(map[string][]string)
	for accountID, set := range p.modelLists {
		if len(set) == 0 {
			continue
		}
		accountsWithCache++
		for modelID := range set {
			modelToAccounts[modelID] = append(modelToAccounts[modelID], accountID)
		}
	}

	entries = make([]ModelMatrixEntry, 0, len(modelToAccounts))
	for modelID, ids := range modelToAccounts {
		sort.Strings(ids)
		entries = append(entries, ModelMatrixEntry{
			Model:        modelID,
			AccountIDs:   ids,
			CapableCount: len(ids),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Model < entries[j].Model })
	return entries, accountsWithCache
}

// diagnosticsForLocked must be called with p.mu held. allowOverUsage is passed in
// rather than read here: this function runs under the pool lock, and reading it
// from config would nest cfgLock beneath p.mu, freezing the whole pool for the
// duration of any concurrent config write (see DiagnosticsFor).
func (p *AccountPool) diagnosticsForLocked(accounts []config.Account, model string, allowOverUsage bool) []AccountDiagnostics {
	now := time.Now()
	inPool := make(map[string]bool)
	for _, acc := range p.accounts {
		inPool[acc.ID] = true
	}

	out := make([]AccountDiagnostics, 0, len(accounts))
	for _, acc := range accounts {
		reason := "available"
		available := true
		cooldownUntil := int64(0)

		if !acc.Enabled {
			available = false
			reason = "disabled"
		} else if model != "" && !p.accountHasModel(acc.ID, model) {
			available = false
			reason = "unsupported_model"
		} else if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			available = false
			reason = "cooldown"
			cooldownUntil = cooldown.Unix()
		} else if isQuotaBlocked(acc, allowOverUsage) {
			available = false
			reason = "quota_exhausted"
		} else if !inPool[acc.ID] {
			available = false
			reason = "not_in_pool"
		} else if acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			// token_refresh_due is INFORMATIONAL, not a blocking state: the request
			// handler refreshes the token before use, so such an account is still
			// routable. It must therefore be evaluated LAST.
			//
			// Previously it sat ahead of the quota and not-in-pool checks, so a
			// quota-exhausted account that Reload() had already dropped from the
			// routing pool was reported to the operator as Available=true with
			// reason "token_refresh_due" — the admin panel showed a healthy account
			// that could not serve a single request, hiding the real cause.
			available = true
			reason = "token_refresh_due"
		}

		out = append(out, AccountDiagnostics{
			ID:               acc.ID,
			Enabled:          acc.Enabled,
			InPool:           inPool[acc.ID],
			Available:        available,
			Reason:           reason,
			ErrorCount:       p.errorCounts[acc.ID],
			CooldownUntil:    cooldownUntil,
			TokenExpiresAt:   acc.ExpiresAt,
			UsageCurrent:     acc.UsageCurrent,
			UsageLimit:       acc.UsageLimit,
			UsagePercent:     acc.UsagePercent,
			OverageStatus:    acc.OverageStatus,
			OverageAllowed:   isUpstreamOverageEnabled(acc) || allowOverUsage,
			CachedModelCount: len(p.modelLists[acc.ID]),
		})
	}
	return out
}

// GetAllAccounts 获取所有账号副本
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

func isOverUsageLimit(acc config.Account) bool {
	return acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit
}

// remainingQuota returns the account's remaining period quota
// (usageLimit - usageCurrent, clamped >= 0) and whether the account carries
// usable quota data at all (usageLimit > 0). Accounts with no limit data return
// (0, false) so quota-aware routing can fall back to round-robin for them.
func remainingQuota(acc config.Account) (float64, bool) {
	if acc.UsageLimit <= 0 {
		return 0, false
	}
	rem := acc.UsageLimit - acc.UsageCurrent
	if rem < 0 {
		rem = 0
	}
	return rem, true
}

// eligibleForRoute reports whether an account can currently receive a request:
// not excluded, not on cooldown, and not quota-blocked. The model filter is
// applied separately by the caller. Caller must hold at least a read lock.
func (p *AccountPool) eligibleForRoute(acc *config.Account, excluded map[string]bool, now time.Time, allowOverUsage bool) bool {
	if excluded != nil && excluded[acc.ID] {
		return false
	}
	if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
		return false
	}
	if isQuotaBlocked(*acc, allowOverUsage) {
		return false
	}
	// The circuit breaker must gate this path too. The LRU path checks it
	// inline (see GetNextForModelExcluding), but quota-aware selection runs
	// BEFORE that loop and returns immediately on a hit — so without this an
	// operator who enables quota-aware routing silently loses the breaker, and
	// the account with the most remaining quota is exactly the one a breaker is
	// most likely to be open on (a freshly-banned account has burned nothing).
	//
	// Read p.circuitState directly rather than via p.isCircuitOpen: callers of
	// eligibleForRoute already hold p.mu for writing, and isCircuitOpen takes
	// RLock, which would self-deadlock. The breaker's own mutex still guards the
	// open->half-open transition inside isOpen.
	if cb := p.circuitState[acc.ID]; cb != nil && cb.isOpen(now) {
		return false
	}
	return true
}

// pickQuotaAware returns the eligible account with the MOST remaining period
// quota (usageLimit - usageCurrent). Pass model="" to skip the model filter.
// It returns nil when no eligible account has usable quota data, so the caller
// falls back to round-robin. Weighted duplicates are de-duplicated by ID; in
// quota-aware mode selection is driven by remaining quota, not weight. Ties go
// to the first candidate in list order (deterministic). Caller holds RLock.
func (p *AccountPool) pickQuotaAware(model string, excluded map[string]bool, now time.Time, allowOverUsage bool) *config.Account {
	var best *config.Account
	var bestRem float64
	seen := make(map[string]bool)
	for i := range p.accounts {
		acc := &p.accounts[i]
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if model != "" && !p.accountHasModel(acc.ID, model) {
			continue
		}
		if !p.eligibleForRoute(acc, excluded, now, allowOverUsage) {
			continue
		}
		rem, ok := remainingQuota(*acc)
		if !ok {
			continue
		}
		if best == nil || rem > bestRem {
			best = acc
			bestRem = rem
		}
	}
	if best == nil {
		return nil
	}
	// Return a COPY: both callers hand this pointer straight back to the request
	// path, which mutates it without the pool lock (see GetNextExcluding).
	selected := *best
	return &selected
}

// isQuotaBlocked reports whether an over-quota account should be skipped:
// the per-account upstream Overages switch (OverageStatus=ENABLED) and the
// global allowOverUsage setting are the two ways to keep it routable.
func isQuotaBlocked(acc config.Account, allowOverUsage bool) bool {
	return isOverUsageLimit(acc) && !isUpstreamOverageEnabled(acc) && !allowOverUsage
}

// isUpstreamOverageEnabled reports whether the upstream Overages switch is ON for this account.
// "ENABLED" → true; anything else (DISABLED, UNKNOWN, empty) → false.
func isUpstreamOverageEnabled(acc config.Account) bool {
	return strings.EqualFold(acc.OverageStatus, "ENABLED")
}

func effectiveWeight(weight int) int {
	if weight < 1 {
		return 1
	}
	return weight
}

// RecordLatency records an observed request latency into the account's EWMA
// (α=healthEWMAAlpha). Called from request handlers after a response completes
// so health-aware selection can prefer faster accounts.
func (p *AccountPool) RecordLatency(id string, latencyMs float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.healthStats == nil {
		p.healthStats = make(map[string]*accountHealth)
	}
	h := p.healthStats[id]
	if h == nil {
		h = &accountHealth{}
		p.healthStats[id] = h
	}
	if h.ewmaLatencyMs == 0 {
		h.ewmaLatencyMs = latencyMs
	} else {
		h.ewmaLatencyMs = healthEWMAAlpha*latencyMs + (1-healthEWMAAlpha)*h.ewmaLatencyMs
	}
	if h.samples == 0 {
		h.samples = 1 // a latency observation counts as evidence for scoring
	}
}

// healthScore returns a tie-breaker weight for the account: higher = preferred.
// score = effectiveWeight × (1 - ewmaErrorRate) × (1 / (1 + latency/1000))
// Used only to break ties in LRU selection. Lock-free: the caller holds p.mu.
func (p *AccountPool) healthScore(id string, weight int) float64 {
	w := float64(weight)
	if w < 1 {
		w = 1
	}
	h := p.healthStats[id]
	if h == nil || h.samples == 0 {
		return w // no data → default weight
	}
	latencyFactor := 1.0 / (1.0 + h.ewmaLatencyMs/1000.0)
	return w * (1.0 - h.ewmaErrorRate) * latencyFactor
}

// fallbackEarliestCooldown returns the account with the earliest cooldown
// (or one with no cooldown at all) when no fully-healthy candidate exists.
// model="" means "any model". Caller must hold p.mu (at least RLock).
//
// now is passed in rather than read here so this fallback and the candidate
// loop that precedes it evaluate every account's breaker against the SAME
// instant; a second time.Now() could straddle circuitOpenDuration and let an
// account the candidate loop just rejected slip through the fallback.
func (p *AccountPool) fallbackEarliestCooldown(model string, excluded map[string]bool, allowOverUsage bool, now time.Time) *config.Account {
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if model != "" && !p.accountHasModel(acc.ID, model) {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		// The breaker must gate this path too. This fallback exists so a pool
		// whose every account is in cooldown still serves traffic (a quota
		// backoff parks an account for a full hour), and a cooldown is a
		// timing hint — serving through it is a deliberate trade. An OPEN
		// circuit is a different statement: circuitErrorThreshold consecutive
		// failures just proved this upstream is not answering, so dispatching
		// to it converts one open breaker into a stream of failed requests
		// that each re-arm the breaker. Skipping it here does not take the
		// pool permanently dark: isOpen promotes open->half-open after
		// circuitOpenDuration and admits a probe, so service resumes on the
		// first request after the window (pinned by
		// TestOpenCircuitRecoversViaProbeAfterWindow).
		if cb := p.circuitState[acc.ID]; cb != nil && cb.isOpen(now) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			return acc
		}
	}
	return best
}

// startAutoRecover launches a background goroutine that periodically refreshes
// disabled accounts' tokens. If a refresh succeeds, the account is re-enabled.
// Exponential backoff per account: 1m → 5m → 25m → max 2h.
func (p *AccountPool) startAutoRecover() {
	p.stopRecover = make(chan struct{})
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.reprobeDisabled()
			case <-p.stopRecover:
				return
			}
		}
	}()
}

// reprobeDisabled iterates disabled accounts whose reprobe time has arrived,
// attempts a token refresh, and re-enables on success.
func (p *AccountPool) reprobeDisabled() {
	if !config.GetAutoRecoverEnabled() {
		return
	}
	now := time.Now()
	all := config.GetAccounts()
	for _, acc := range all {
		if acc.Enabled || acc.BanStatus != "DISABLED" {
			continue
		}
		// Auto-recovery's premise is that DISABLED means "credentials went bad",
		// so a successful token refresh is proof the account is healthy again.
		// That premise does not hold for an external-usage quarantine: the
		// credential there is perfectly valid — it is being used by a THIRD
		// PARTY as well. Refreshing it always succeeds, so without this guard
		// auto-recovery re-enables the account within one 60s tick and silently
		// undoes F3, putting a shared credential straight back into rotation.
		// Lifting this quarantine is an operator decision, not a token check.
		if acc.BanReason == config.ExternalUsageDisableReason {
			continue
		}
		// Check backoff schedule.
		p.mu.Lock()
		next, ok := p.reprobeNext[acc.ID]
		backoff := p.reprobeBackoff[acc.ID]
		p.mu.Unlock()
		if ok && now.Before(next) {
			continue // not time yet
		}
		// Attempt refresh.
		_, _, _, _, err := auth.RefreshToken(&acc)
		if err == nil {
			// Success! Re-enable.
			config.SetAccountEnabled(acc.ID, true)
			p.Reload()
			p.mu.Lock()
			delete(p.reprobeBackoff, acc.ID)
			delete(p.reprobeNext, acc.ID)
			p.mu.Unlock()
			continue
		}
		// Failure → increase backoff: 1m → 5m → 25m → 2h max.
		if backoff == 0 {
			backoff = time.Minute
		} else {
			backoff *= 5
			if backoff > 2*time.Hour {
				backoff = 2 * time.Hour
			}
		}
		p.mu.Lock()
		p.reprobeBackoff[acc.ID] = backoff
		p.reprobeNext[acc.ID] = now.Add(backoff)
		p.mu.Unlock()
	}
}

// AccountHealthSnapshot is a read-only view of one account's dispatch/health
// signals, surfaced to operators (admin /admin/pool) so the effect of session
// affinity and health-aware routing is observable: a warm, stuck-to account
// shows lower LatencyMsEWMA than a cold one that keeps getting re-picked.
type AccountHealthSnapshot struct {
	ID              string  `json:"id"`
	Email           string  `json:"email,omitempty"`
	LatencyMsEWMA   float64 `json:"latencyMsEwma"`
	ErrorRateEWMA   float64 `json:"errorRateEwma"`
	Samples         int     `json:"samples"`
	Circuit         string  `json:"circuit"` // closed | open | half-open
	CooldownActive  bool    `json:"cooldownActive"`
	LastDispatchSeq uint64  `json:"lastDispatchSeq"` // monotonic LRU clock; higher = more recently dispatched
}

// HealthSnapshots returns a per-account health view for every account currently
// in the pool. Lock order is p.mu (RLock) then each breaker's own mutex — the
// same order the dispatch path uses (p.mu held while calling cb.isOpen) — so
// this introduces no new deadlock risk.
func (p *AccountPool) HealthSnapshots() []AccountHealthSnapshot {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]AccountHealthSnapshot, 0, len(p.accounts))
	for i := range p.accounts {
		acc := &p.accounts[i]
		snap := AccountHealthSnapshot{
			ID:              acc.ID,
			Email:           acc.Email,
			Circuit:         "closed",
			LastDispatchSeq: p.lastDispatchSeq[acc.ID],
		}
		if h := p.healthStats[acc.ID]; h != nil {
			snap.LatencyMsEWMA = h.ewmaLatencyMs
			snap.ErrorRateEWMA = h.ewmaErrorRate
			snap.Samples = h.samples
		}
		if cb := p.circuitState[acc.ID]; cb != nil {
			cb.mu.Lock()
			switch cb.state {
			case circuitOpen:
				snap.Circuit = "open"
			case circuitHalfOpen:
				snap.Circuit = "half-open"
			}
			cb.mu.Unlock()
		}
		if cd, ok := p.cooldowns[acc.ID]; ok && now.Before(cd) {
			snap.CooldownActive = true
		}
		out = append(out, snap)
	}
	return out
}

// LatencyAggregate is a customer-safe summary of dispatch latency across the
// pool: no account identities, just the distribution. Surfaced in /v1/stats so
// the effect of session affinity is visible without leaking pool internals.
type LatencyAggregate struct {
	AccountsWithData int     `json:"accountsWithData"`
	LatencyMsMean    float64 `json:"latencyMsMean"`
	LatencyMsMin     float64 `json:"latencyMsMin"`
	LatencyMsMax     float64 `json:"latencyMsMax"`
}

// LatencyAggregate summarizes per-account EWMA latency without exposing which
// account is which. Accounts with no recorded latency are ignored (includes
// accounts that only logged errors, since they have no latency to average).
func (p *AccountPool) LatencyAggregate() LatencyAggregate {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var agg LatencyAggregate
	var sum float64
	for i := range p.accounts {
		h := p.healthStats[p.accounts[i].ID]
		if h == nil || h.samples == 0 || h.ewmaLatencyMs <= 0 {
			continue
		}
		l := h.ewmaLatencyMs
		if agg.AccountsWithData == 0 || l < agg.LatencyMsMin {
			agg.LatencyMsMin = l
		}
		if l > agg.LatencyMsMax {
			agg.LatencyMsMax = l
		}
		sum += l
		agg.AccountsWithData++
	}
	if agg.AccountsWithData > 0 {
		agg.LatencyMsMean = sum / float64(agg.AccountsWithData)
	}
	return agg
}

// GetNextForModelBoundExcluding is like GetNextForModelExcluding but restricts the
// selection to accounts whose ID is in allowed (the API key's bound-account set).
// Returns nil when no bound account is currently usable, so the caller can decide
// whether to fall back to the shared pool. An empty allowed set also returns nil:
// a bound key with no live bound account is never silently widened to the pool.
//
// Rather than re-implementing selection, this narrows the candidate set and then
// delegates, so bound keys go through exactly the same pipeline as unbound ones
// (quota-aware routing, circuit breakers, LRU + health tie-break, cooldown
// fallback, and the detached copy that keeps callers from racing pool writers).
// A second, independent selection path would silently bypass all of it.
func (p *AccountPool) GetNextForModelBoundExcluding(model string, allowed, excluded map[string]bool) *config.Account {
	if len(allowed) == 0 {
		return nil
	}
	// Convert the whitelist into the blacklist the shared selector speaks: keep
	// the caller's exclusions and add every pooled account not in allowed.
	merged := make(map[string]bool, len(excluded)+len(allowed))
	for id := range excluded {
		merged[id] = true
	}
	// Snapshot the pooled IDs under RLock and release it before delegating:
	// GetNextForModelExcluding takes p.mu itself, so holding it here would
	// self-deadlock.
	p.mu.RLock()
	for i := range p.accounts {
		if id := p.accounts[i].ID; !allowed[id] {
			merged[id] = true
		}
	}
	p.mu.RUnlock()
	return p.GetNextForModelExcluding(model, merged)
}
