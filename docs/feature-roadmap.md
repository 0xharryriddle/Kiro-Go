# Kiro-Go Feature Roadmap & Design

Status: SHIPPED — all 12 features (F1–F12) implemented, tested, and deployed. Author: engineering review pass.
Scope: candidate features derived from a full codebase scan plus a review of the
production LLM-gateway landscape (LiteLLM, Portkey, Helicone, 2025). This document
originally existed to decide *what to build next*; it is retained as the design
record now that the work is complete.

Delivery summary (all merged to origin/harry, each with unit tests + admin UI + en/zh i18n):
- F1 account health score · F2 quota-aware routing · F3 external-usage auto-action
- F4 fleet capacity forecast · F5 response cache (exact-match, opt-in) · F6 per-key RPM/TPM rate limits
- F7 event webhook bus · F8 model-availability matrix · F9 Prometheus /metrics exposition
- F10 per-model cost attribution · F11 PII redaction in prompt filter · F12 usage anomaly detection
All features that change routing, auto-disable accounts, cache responses, expose /metrics,
or POST to a webhook are config-gated and default OFF, preserving prior behaviour until
opted in. F5 caches only non-streaming, tool-free, non-thinking requests, exact-match,
in-memory, TTL-bounded — the conservative design from §3.

---

## 1. Guiding principle

Kiro-Go is **not** a generic LLM gateway. Its defining constraint is that its
"providers" are **scarce, bannable, real Kiro/Amazon Q subscriptions metered in
credits (agentic-request units), not dollars.** Every account is precious and a
hard ban is effectively unrecoverable.

Therefore the highest-value features are the ones that **protect the account fleet**
(avoid bans, detect misuse) and **stretch it** (waste fewer credits, balance burn),
NOT the generic FinOps/observability features that already have mature competitors.

Design filter applied to every idea below:
- Does it exploit the scarce-account constraint? (native value)
- Does it rest on data we already persist? (low risk, no fabrication)
- What is the blast radius / reversibility?

---

## 2. Current-state inventory (verified from code)

| Capability | Where |
|---|---|
| OpenAI `/v1/chat/completions`, Anthropic `/v1/messages`, `/v1/responses` | `proxy/handler.go` route table |
| Multi-account round-robin pool | `pool/account.go` |
| Auto token refresh (background + on-demand) | `proxy/handler.go:refreshAllAccounts`, `pool/account.go` |
| Per-account overage switch (upstream ENABLED/DISABLED) | `proxy/kiro_overage.go` |
| Per-account usage (`UsageCurrent`/`UsageLimit`/`UsagePercent`/`NextResetDate`) | `config/config.go` Account, refreshed 30 min |
| External-usage audit (credits, period-scoped, confidence tiers) | `config/external_usage.go`, `proxy/admin_usage_audit.go` |
| Account diagnostics / external-IdP diagnostics / model-routing / replay dry-run | `proxy/handler.go`, `pool/account.go` |
| Request logs (ring buffer, 500) + audit logs (1000) | `proxy/handler.go` |
| API keys with **cumulative** token/credit limits | `config/apikeys.go`, `proxy/admin_apikeys.go` |
| Error classification (quota/overage/suspended/auth/profile) | `proxy/account_failover.go:classifyError` |
| Prompt filter (regex / lines-containing) | `config/config.go` PromptFilterRule |
| Config backup/restore/export, outbound proxy, i18n (en/zh), health/readyz | `proxy/handler.go` |

Key gaps vs. the market: no **rate** limits (only cumulative), no response **cache**,
no **fallback ordering** beyond round-robin, no **quota-aware routing**, no outbound
**alerting**, no predictive **health/ban** signal.

---

## 3. Feature catalogue

Each entry: value, mechanism, data source, effort, risk, and whether it depends on
anything unverified.

### Tier 1 — Kiro-Go-native (highest value)

#### F1. Account health score + pre-ban early warning
- **Value:** Accounts are bannable and unrecoverable. Catching a degrading account
  *before* a hard ban is the single most valuable protection this proxy can offer.
- **Mechanism:** Aggregate a rolling per-account signal from data we already emit:
  error-rate over a sliding window, streak of `auth`/`suspended`/`profile` error
  classes, 403/suspension responses, latency drift. Produce a 0-100 health score
  and auto-quarantine (cooldown, stop routing) below a threshold, with an alert.
- **Data source:** `classifyError` already buckets errors; `RequestLog` already
  carries `ErrorType`, `AccountID`, `Time`. `account_failover.go` already reacts to
  individual failures — this aggregates them over time instead of per-request.
- **Effort:** Medium-high. New rolling-window accumulator + score + quarantine hook.
- **Risk:** Medium. Auto-quarantine changes routing behaviour — must be opt-in and
  reversible, with a manual override.
- **UNVERIFIED DEPENDENCY:** How legible AWS's *pre-ban* signals actually are. Until
  validated against a real corpus of pre-ban error samples from request logs, this is
  a **heuristic health score**, not a guaranteed ban predictor. Do NOT market it as
  prediction until proven. Ship the score first; add "prediction" only after evidence.

#### F2. Quota-aware routing  ⭐ recommended first build
- **Value:** Today routing is blind round-robin, so one account can burn to zero while
  others sit idle — shortening fleet lifetime and risking quota-exhaustion errors.
  Routing by remaining quota maximizes fleet longevity with near-zero downside.
- **Mechanism:** In `pool.Pick`, bias selection toward the account with the most
  remaining period quota (`UsageLimit - UsageCurrent`), or weight round-robin by it.
  Keep round-robin as a fallback when quota data is stale/absent.
- **Data source:** `UsageCurrent`/`UsageLimit` already fetched every 30 min and on
  refresh. No new upstream calls.
- **Effort:** Low-medium. Single hot-path change + a config toggle + tests.
- **Risk:** Low. Pure selection-order change; falls back to current behaviour when
  data is missing. Fully reversible via toggle.
- **Caveat:** 30-min refresh granularity means quota is slightly stale between refreshes;
  acceptable because we also increment locally between refreshes.

#### F3. External-usage auto-action (closes the loop we just opened)
- **Value:** We now *detect* `external`/`strong_external` usage but only display it.
  This turns detection into protection.
- **Mechanism:** Optional policy: when an account crosses into `strong_external`
  (disabled-but-growing = unambiguous), auto-disable local routing and/or fire an
  alert. Threshold + action configurable; default off.
- **Data source:** `config/external_usage.go` verdict + the audit event already emitted
  on first crossing (`external_usage_detected`).
- **Effort:** Low. Hook onto the existing transition event.
- **Risk:** Low-medium. Auto-disable is reversible; must be opt-in.

#### F4. Fleet capacity forecast
- **Value:** Turns raw stats into an operations decision: "at current burn the fleet
  exhausts in ~X hours; account Y resets on Z." Answers "do I need more accounts?"
- **Mechanism:** Compute burn rate from per-period our-credits deltas + time; project
  against remaining quota and `NextResetDate`. Display-only panel.
- **Data source:** `UsageCurrent`, `NextResetDate`, `ExternalPeriodOurCredit` — all
  already persisted.
- **Effort:** Low-medium (mostly UI + a projection function with unit tests).
- **Risk:** Very low. Read-only.

### Tier 2 — proven gateway features worth porting

#### F5. Response cache (credit-saver)  ⭐ high ROI
- **Value:** Credits are the scarce resource. Every cache hit = 1 saved credit =
  extended fleet life. Industry reports cite up to ~95% cost reduction on cacheable
  workloads. Highest ROI of the "borrowed" features.
- **Mechanism:** Phase 1 exact-match: hash of (model + normalized messages + params) →
  cached response with TTL. Phase 2 (optional, later) semantic cache. Non-streaming
  first; streaming replay is a follow-up.
- **Data source:** Request bodies are already parsed by the translator layer.
- **Effort:** Medium. Cache store + key derivation + TTL + admin toggle + metrics.
- **Risk:** Medium. Correctness hazards: must NOT cache when `thinking` is on, when
  tools/streaming semantics differ, or across accounts if responses are user-scoped.
  Needs careful key design + tests. Start conservative (opt-in, exact-match only).

#### F6. Per-key rate limits (RPM/TPM, windowed)
- **Value:** Current API-key limits are *cumulative* (never reset). A windowed RPM/TPM
  limit prevents one key burning the whole fleet in minutes.
- **Mechanism:** Sliding-window counters per key; reject with 429 + `retry-after` when
  exceeded (matches gateway norms).
- **Data source:** `ApiKeyEntry` in `config/apikeys.go` — extend with rpm/tpm fields.
- **Effort:** Medium.
- **Risk:** Low-medium. In-process counters only (no Redis); single-instance assumption
  should be documented.

#### F7. Notification / webhook bus
- **Value:** We already generate the meaningful events (ban detected, external usage,
  quota low, account down) — they just die in the audit log. Shipping them to
  Slack/Discord/generic webhook makes the proxy operable without watching a dashboard.
- **Mechanism:** Fan out selected audit/security events to configured webhook URLs.
- **Data source:** existing `AuditLog` events.
- **Effort:** Low-medium.
- **Risk:** SECURITY — this is the one feature that makes **outbound requests to a
  third-party endpoint**. Must be opt-in per URL, never enabled by default, and must
  never include secrets or raw prompt content in the payload (safe fields only, mirror
  the audit-log redaction discipline).

#### F8. Model-availability matrix + capability-aware routing
- **Value:** Avoid routing a model request to an account that can't serve it — a failed
  call still burns error budget and nudges F1's health score down.
- **Mechanism:** Surface the per-account cached model list as a fleet grid; route model
  requests only to capable accounts.
- **Data source:** per-account model cache already exists (`/accounts/{id}/models/cached`).
- **Effort:** Low-medium.
- **Risk:** Low.

### Tier 3 — nice-to-have / polish

- **F9. Prometheus `/metrics` exposition.** We already have `/metrics/summary` (JSON);
  add a Prometheus text-format endpoint for standard scraping. Low effort, low risk.
- **F10. Per-model cost attribution on API keys.** Extend existing key usage counters to
  break down by model. Low effort.
- **F11. PII redaction** extending the prompt filter (regex redaction pass). Medium.
- **F12. Usage anomaly detection** (per-account/key spike vs. rolling baseline). Medium;
  overlaps with F1's accumulator — build together if both are wanted.

---

## 4. Dependency & sequencing map

```
Protect the fleet            Stretch the fleet
-----------------            -----------------
F1 health/ban  ---+          F2 quota routing (independent, low risk)
                  |          F5 response cache (independent, high ROI)
F3 ext auto-action|
(needs ext audit, +--> shares rolling-window accumulator with F12
 already built)   |
F12 anomaly    ---+

F4 forecast   (read-only, independent)
F7 webhook bus (consumes events from F1/F3 — build after at least one producer)
F8 model matrix (independent)
F6 rate limits (independent)
F9/F10 (independent polish)
```

Recommended order for maximum impact:
1. **F2 quota-aware routing** — highest leverage, data exists, low risk, reversible.
2. **F5 response cache** — biggest credit saver; start exact-match, opt-in.
3. **F3 external-usage auto-action** — cheap, closes the loop already opened.
4. **F1 health score** — highest ceiling, but validate pre-ban signal legibility first;
   ship as a heuristic score before claiming prediction.

Everything else is polish layered on top.

---

## 5. Cross-cutting requirements (apply to whatever we build)

- **Config-gated & reversible.** Any feature that changes routing or auto-disables an
  account must be opt-in with a manual override and default to current behaviour.
- **No new upstream calls on the hot path** unless explicitly justified (F1/F2/F4 all
  reuse the 30-min refresh data).
- **Redaction discipline.** No secrets or raw prompts in logs, webhooks, or caches
  keyed/exposed to other scopes — mirror the existing audit-log safe-fields pattern.
- **Backward-compatible schema.** New `config.Account` / `ApiKeyEntry` fields must be
  `omitempty` so existing `config.json` loads unchanged (same discipline as the
  external-usage fields).
- **Tests without ports.** Core logic (routing selection, cache key derivation, health
  scoring, forecast projection) must be pure and unit-testable without binding a port.
- **Single-instance assumption** is currently implicit (in-process counters, no Redis).
  Document it for F5/F6; multi-instance is out of scope unless we add a shared store.

---

## 6. Honest risk register

| Feature | Load-bearing assumption | If wrong |
|---|---|---|
| F1 pre-ban prediction | AWS emits legible pre-ban signals | Degrades to a heuristic health score, not prediction — must be labeled as such |
| F2 quota routing | `UsageCurrent`/`UsageLimit` are fresh enough at 30-min cadence | Slightly suboptimal balancing; still strictly better than blind round-robin |
| F5 response cache | Cacheable requests are truly idempotent (no thinking/tool/stream divergence) | Stale/incorrect responses — mitigated by conservative exact-match + opt-in |
| F6 rate limits | Single proxy instance | Under-counts across instances — document, or add shared store later |
| F7 webhook bus | Operator trusts the webhook endpoint | Data egress — opt-in per URL, safe fields only, off by default |

---

## 7. Decision needed

Pick a build target (or approve the recommended order F2 -> F5 -> F3 -> F1). Each is
independently shippable; none blocks another except F7 (wants an event producer first).
