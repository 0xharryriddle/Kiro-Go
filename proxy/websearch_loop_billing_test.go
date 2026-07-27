package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// The mixed web-search loop calls generateAssistantResponse once per round, and
// each round re-enters the pool via GetNextForModelExcluding (websearch_loop.go
// callUpstreamForWebSearch). That selector is LRU over lastDispatchSeq, which it
// advances on every dispatch, so consecutive rounds deliberately land on
// DIFFERENT accounts when more than one is eligible.
//
// runWebSearchLoop's accounting does not account for that:
//
//	var lastAccountID string
//	var totalCredits float64
//	for roundIdx := 0; roundIdx <= maxUses; roundIdx++ {
//	        ...
//	        if account != nil { lastAccountID = account.ID }   // OVERWRITTEN each round
//	        totalCredits += round.credits                      // accumulated
//	        ...
//	        inputTokens := round.inputTokens                   // TERMINAL round only
//	        if lastAccountID != "" {
//	                h.pool.UpdateStats(lastAccountID, inputTokens+outputTokens, totalCredits)
//	        }
//
// Two separate accounting defects fall out, and the mismatch between the two
// lines above is what shows they are unintended rather than a design choice:
// credits are explicitly accumulated across rounds with +=, while tokens are
// plain-assigned from the final round. The author clearly meant to total the
// request's cost; the token half was missed.
//
// D1 (tokens dropped): every intermediate round's input tokens are discarded.
// A 2-round request that consumed 500 then 700 input tokens upstream reports
// 700 -- the 500 is never billed to anyone. Under-reporting scales with the
// number of search rounds, up to maxWebSearchRounds.
//
// D2 (misattributed): the whole request -- including the accumulated credits of
// rounds served by OTHER accounts -- is charged to whichever account happened to
// serve the LAST round. Account A does real upstream work and is billed zero
// tokens and zero credits for it; account B is billed for A's consumption.
// TotalTokens/TotalCredits per account drive the admin panel and quota-aware
// routing (GetNextForModelExcluding prefers the account with the most remaining
// quota), so this feeds bad numbers straight back into routing decisions.
//
// This test drives the REAL loop across two rounds against two Kiro accounts and
// a stub MCP endpoint, then asserts the pool's per-account totals. It reads the
// numbers out of config rather than asserting an internal call order, so it pins
// the observable consequence.
func TestWebSearchLoopBillsEveryRoundToTheAccountThatServedIt(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, id := range []string{"bill-A", "bill-B", "bill-C"} {
		if err := config.AddAccount(config.Account{
			ID:          id,
			Enabled:     true,
			AccessToken: "token-" + id,
			ProfileArn:  "arn:aws:codewhisperer:profile/" + id,
		}); err != nil {
			t.Fatalf("AddAccount %s: %v", id, err)
		}
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable fallback: %v", err)
	}

	// Round 1 asks for a web_search (so the loop continues); round 2 returns
	// plain text (so the loop flushes). Each round reports its own token usage.
	const round1Input = 500
	const round2Input = 700

	var mu sync.Mutex
	round := 0
	// servedBy is ground truth for which account served each upstream round,
	// recovered from the bearer token the proxy actually sent. Asserting against
	// this instead of an assumed alternation keeps the test honest about a
	// selector whose choice depends on quota and health, not just round order.
	var servedBy []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountID := strings.TrimPrefix(
			strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "token-")
		mu.Lock()
		round++
		n := round
		servedBy = append(servedBy, accountID)
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		if n == 1 {
			_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
				"toolUseId": "toolu_round1",
				"name":      webSearchToolName,
				"input":     map[string]interface{}{"query": "golang"},
				"stop":      true,
			}))
			_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
				"usage": map[string]interface{}{
					"inputTokens":  round1Input,
					"outputTokens": 0,
				},
			}))
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "final answer",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"usage": map[string]interface{}{
				"inputTokens":  round2Input,
				"outputTokens": 0,
			},
		}))
	}))
	defer srv.Close()
	defer swapKiroEndpointsForTest(t, srv)()

	// Stub MCP so the search between rounds succeeds without leaving the process.
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req McpRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      req.ID,
			"jsonrpc": "2.0",
			"result": map[string]interface{}{
				"content": []map[string]interface{}{{
					"type": "text",
					"text": `{"results":[{"title":"t","url":"https://example.com","snippet":"s"}]}`,
				}},
			},
		})
	}))
	defer mcp.Close()
	oldMcp := mcpEndpointOverride
	mcpEndpointOverride = mcp.URL
	defer func() { mcpEndpointOverride = oldMcp }()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}

	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []ClaudeMessage{{Role: "user", Content: "search then answer"}},
		Tools: []ClaudeTool{{
			Type:    "web_search_20250305",
			Name:    webSearchToolName,
			MaxUses: 2,
		}},
	}

	// A real customer key, because that is where the dropped-token defect does
	// its damage: recordSuccessForApiKey drives TokensUsed, and TokenLimit
	// enforcement auto-disables a key when the budget is spent. Under-counting a
	// multi-round search lets a token-budgeted key overrun its limit.
	keyEntry, err := config.AddApiKey(config.ApiKeyEntry{
		Name:    "websearch-billing",
		Key:     "sk-websearch-billing",
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	rec := httptest.NewRecorder()
	h.runWebSearchLoop(rec, req, false, 10, keyEntry.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("loop returned HTTP %d, body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	rounds := round
	mu.Unlock()
	if rounds < 2 {
		t.Fatalf("test did not exercise a multi-round loop (upstream rounds=%d); "+
			"the billing defect only appears across rounds", rounds)
	}

	// Read per-account totals out of POOL MEMORY, which UpdateStats mutates
	// synchronously. config.GetAccounts() is the wrong seam here: UpdateStats
	// propagates to the config store from a deferred goroutine, so a config read
	// races the write and reports zeros for work that was correctly billed.
	// WaitForPendingWrites drains that propagation first so neither read is racy.
	p.WaitForPendingWrites()
	totals := map[string]int{}
	credits := map[string]float64{}
	for _, acc := range p.GetAllAccounts() {
		totals[acc.ID] = acc.TotalTokens
		credits[acc.ID] = acc.TotalCredits
	}

	billedTokens := totals["bill-A"] + totals["bill-B"] + totals["bill-C"]

	// D1 (conservation) is asserted on the two surfaces the dropped value
	// actually reaches, NOT on the pool total. Per-account settling bills from
	// perAccount[].inputTokens, which is a different variable, so the pool total
	// stays correct even with D1 reverted -- an assertion there passes either
	// way and proves nothing. (Found by neutralizing: a pool-total D1 check
	// survived reverting the fix.)
	//
	// D1a: the customer key's TokensUsed, which drives TokenLimit
	// auto-disable. Under-counting lets a token-budgeted key overrun.
	gotKey := config.GetApiKeyEntry(keyEntry.ID)
	if gotKey == nil {
		t.Fatalf("api key %s disappeared", keyEntry.ID)
	}
	if gotKey.TokensUsed < int64(round1Input+round2Input) {
		t.Errorf("D1 tokens dropped: customer key billed TokensUsed=%d but "+
			"upstream reported %d input tokens across %d rounds. "+
			"runWebSearchLoop assigned inputTokens from the TERMINAL round only "+
			"(inputTokens := round.inputTokens) while accumulating credits with "+
			"+=, so intermediate rounds never reached the key's token budget.",
			gotKey.TokensUsed, round1Input+round2Input, rounds)
	}

	// D1b: the usage the CLIENT is told, which must not under-report what the
	// request consumed.
	var reported struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reported); err != nil {
		t.Fatalf("decode loop response: %v (body=%s)", err, rec.Body.String())
	}
	if reported.Usage.InputTokens < round1Input+round2Input {
		t.Errorf("D1 under-reported to client: usage.input_tokens=%d but the "+
			"request consumed %d input tokens across %d rounds.",
			reported.Usage.InputTokens, round1Input+round2Input, rounds)
	}

	// Pool conservation still checked, as a floor rather than the D1 proof.
	if billedTokens < round1Input+round2Input {
		t.Errorf("pool recorded %d input-ish tokens total (A=%d B=%d C=%d) but "+
			"upstream reported %d across %d rounds",
			billedTokens, totals["bill-A"], totals["bill-B"], totals["bill-C"],
			round1Input+round2Input, rounds)
	}

	// D2 (attribution): an account may only be billed for rounds it actually
	// served. Which account serves which round is NOT asserted -- selection is
	// quota/health-aware LRU, so both rounds legitimately land on one account
	// when the other looks worse. The invariant is that no account is billed for
	// a round it did not serve, so an account that served nothing must be zero.
	served := map[string]bool{}
	mu.Lock()
	for _, id := range servedBy {
		served[id] = true
	}
	mu.Unlock()

	for _, id := range []string{"bill-A", "bill-B", "bill-C"} {
		if !served[id] && totals[id] != 0 {
			t.Errorf("D2 misattributed: account %s served no upstream round but "+
				"was billed %d tokens / %.4f credits. runWebSearchLoop charged the "+
				"whole request to lastAccountID regardless of who served it.",
				id, totals[id], credits[id])
		}
		if served[id] && totals[id] == 0 {
			t.Errorf("D2 misattributed: account %s served an upstream round but "+
				"was billed 0 tokens; its usage was absorbed by another account.", id)
		}
	}

	t.Logf("rounds=%d servedBy=%v | A=%d B=%d C=%d tokens (credits %.4f/%.4f/%.4f)",
		rounds, servedBy, totals["bill-A"], totals["bill-B"], totals["bill-C"],
		credits["bill-A"], credits["bill-B"], credits["bill-C"])
}
