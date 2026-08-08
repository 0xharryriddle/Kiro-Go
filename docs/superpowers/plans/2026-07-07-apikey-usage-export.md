# API Key Usage Export Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an export feature for proxy API keys that produces a masked usage report (JSON + CSV), not a re-importable backup.

**Architecture:** A new password-gated admin endpoint `POST /admin/api/api-keys/export` returns a masked JSON report with server-computed derived columns (percent-used, over-limit, expired). The frontend adds an "Export" button + modal in the API Keys tab mirroring the existing account export modal; CSV is built client-side from the same JSON payload so there is a single data source.

**Tech Stack:** Go stdlib `net/http` + `encoding/json`; vanilla JS frontend (`web/app.js`, `web/index.html`); JSON locale files.

## Global Constraints

- Single Go module, stdlib `net/http` only, one dependency (`github.com/google/uuid`). No new deps.
- API key values must ALWAYS be masked via `config.MaskApiKey`. Never emit the raw `sk-` value.
- Admin API endpoints are password-gated (`X-Admin-Password`) — routing lives in `proxy/handler.go` `handleAdminAPI`.
- Read-only feature: no config writes, no hot-path config mutation.
- Reuse `config.ApiKeyOverLimit` / `config.ApiKeyExpired` for limit/expiry logic — keep it in one place.
- Comments in the codebase mix English and Chinese; match the surrounding file.
- Tests live beside their code (`*_test.go`).

---

### Task 1: Backend export handler

**Files:**
- Modify: `proxy/admin_apikeys.go` (add view struct + handler; imports `time`)
- Modify: `proxy/handler.go:2273-2289` (register route in the `/api-keys` case block)
- Test: `proxy/admin_apikeys_export_test.go` (new)

**Interfaces:**
- Consumes: `config.ListApiKeys() []config.ApiKeyEntry`, `config.GetApiKeyEntry(id string) *config.ApiKeyEntry`, `config.MaskApiKey(key string) string`, `config.ApiKeyOverLimit(e config.ApiKeyEntry) (overToken, overCredit bool)`, `config.ApiKeyExpired(e config.ApiKeyEntry) bool`, `config.Version` (string const).
- Produces: `POST /admin/api/api-keys/export` accepting `{ "ids": []string }` (empty/missing = all), returning `{ version, exportedAt, apiKeys: [apiKeyExportView] }`. Handler method: `func (h *Handler) apiExportApiKeys(w http.ResponseWriter, r *http.Request)`.

- [ ] **Step 1: Write the failing test**

Create `proxy/admin_apikeys_export_test.go`:

```go
package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedExportKeys inserts four entries covering: unlimited, under-limit,
// over-token-limit, and expired. Returns their IDs in insertion order.
func seedExportKeys(t *testing.T) (unlimited, under, overTok, expired string) {
	t.Helper()

	e1, err := config.AddApiKey(config.ApiKeyEntry{Name: "unlimited", Key: "sk-unlimited-secret", Enabled: true})
	if err != nil {
		t.Fatalf("seed unlimited: %v", err)
	}
	e2, err := config.AddApiKey(config.ApiKeyEntry{Name: "under", Key: "sk-under-secret", Enabled: true, TokenLimit: 1000, CreditLimit: 10})
	if err != nil {
		t.Fatalf("seed under: %v", err)
	}
	e3, err := config.AddApiKey(config.ApiKeyEntry{Name: "overtok", Key: "sk-overtok-secret", Enabled: true, TokenLimit: 100})
	if err != nil {
		t.Fatalf("seed overtok: %v", err)
	}
	e4, err := config.AddApiKey(config.ApiKeyEntry{Name: "expired", Key: "sk-expired-secret", Enabled: true, ExpiresAt: time.Now().Unix() - 3600})
	if err != nil {
		t.Fatalf("seed expired: %v", err)
	}

	// Drive usage counters. under: 500/1000 tokens, 5/10 credits. overtok: 200/100 tokens.
	if err := config.RecordApiKeyUsage(e2.ID, 500, 5); err != nil {
		t.Fatalf("usage under: %v", err)
	}
	if err := config.RecordApiKeyUsage(e3.ID, 200, 0); err != nil {
		t.Fatalf("usage overtok: %v", err)
	}
	return e1.ID, e2.ID, e3.ID, e4.ID
}

func decodeExport(t *testing.T, body string, ids []string) map[string]apiKeyExportView {
	t.Helper()
	reqBody := "{}"
	if ids != nil {
		b, _ := json.Marshal(map[string][]string{"ids": ids})
		reqBody = string(b)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/api/api-keys/export", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	h := &Handler{}
	h.apiExportApiKeys(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Raw secret must never appear.
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("raw key value leaked in output: %s", w.Body.String())
	}

	var out struct {
		Version    string             `json:"version"`
		ExportedAt int64              `json:"exportedAt"`
		ApiKeys    []apiKeyExportView `json:"apiKeys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ExportedAt == 0 {
		t.Fatalf("expected exportedAt to be set")
	}
	byID := make(map[string]apiKeyExportView, len(out.ApiKeys))
	for _, v := range out.ApiKeys {
		byID[v.ID] = v
	}
	return byID
}

func TestExportApiKeysAllMaskedAndDerived(t *testing.T) {
	mustInitConfig(t)
	_, under, overTok, expired := seedExportKeys(t)

	got := decodeExport(t, "", nil)
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(got))
	}

	u := got[under]
	if !strings.HasPrefix(u.KeyMasked, "sk-") || !strings.Contains(u.KeyMasked, "***") {
		t.Fatalf("under key not masked: %q", u.KeyMasked)
	}
	if u.TokenPercentUsed != 50 {
		t.Fatalf("under tokenPercentUsed: want 50, got %v", u.TokenPercentUsed)
	}
	if u.CreditPercentUsed != 50 {
		t.Fatalf("under creditPercentUsed: want 50, got %v", u.CreditPercentUsed)
	}
	if u.OverToken || u.OverCredit || u.Expired {
		t.Fatalf("under should not be over/expired: %+v", u)
	}

	o := got[overTok]
	if !o.OverToken {
		t.Fatalf("overtok should be OverToken: %+v", o)
	}
	if o.TokenPercentUsed != 200 {
		t.Fatalf("overtok tokenPercentUsed: want 200, got %v", o.TokenPercentUsed)
	}

	e := got[expired]
	if !e.Expired {
		t.Fatalf("expired key should have Expired=true: %+v", e)
	}
	// Unlimited: no limits => percent 0.
	for _, v := range got {
		if v.Name == "unlimited" && (v.TokenPercentUsed != 0 || v.CreditPercentUsed != 0) {
			t.Fatalf("unlimited should have 0 percents: %+v", v)
		}
	}
}

func TestExportApiKeysFilterByIDs(t *testing.T) {
	mustInitConfig(t)
	unlimited, under, _, _ := seedExportKeys(t)

	got := decodeExport(t, "", []string{unlimited, under})
	if len(got) != 2 {
		t.Fatalf("expected 2 filtered entries, got %d", len(got))
	}
	if _, ok := got[unlimited]; !ok {
		t.Fatalf("expected unlimited in filtered result")
	}
	if _, ok := got[under]; !ok {
		t.Fatalf("expected under in filtered result")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./proxy/ -run TestExportApiKeys -v`
Expected: FAIL — compile error `undefined: apiKeyExportView` and `h.apiExportApiKeys`.

- [ ] **Step 3: Add the view struct and handler to `proxy/admin_apikeys.go`**

Add `"time"` to the import block (currently `encoding/json`, `kiro-go/config`, `net/http`, `strconv`). Then append at the end of the file:

```go
// apiKeyExportView is the masked usage-report row. It extends the masked fields
// with server-computed derived columns so the frontend renders both JSON and CSV
// from one source. Never contains the raw key value.
type apiKeyExportView struct {
	ID                string  `json:"id"`
	Name              string  `json:"name,omitempty"`
	KeyMasked         string  `json:"keyMasked"`
	Enabled           bool    `json:"enabled"`
	RequestsCount     int64   `json:"requestsCount"`
	TokensUsed        int64   `json:"tokensUsed"`
	CreditsUsed       float64 `json:"creditsUsed"`
	TokenLimit        int64   `json:"tokenLimit"`
	CreditLimit       float64 `json:"creditLimit"`
	ExpiresAt         int64   `json:"expiresAt"`
	CreatedAt         int64   `json:"createdAt"`
	LastUsedAt        int64   `json:"lastUsedAt"`
	TokenPercentUsed  float64 `json:"tokenPercentUsed"`
	CreditPercentUsed float64 `json:"creditPercentUsed"`
	OverToken         bool    `json:"overToken"`
	OverCredit        bool    `json:"overCredit"`
	Expired           bool    `json:"expired"`
}

func toApiKeyExportView(e config.ApiKeyEntry) apiKeyExportView {
	overToken, overCredit := config.ApiKeyOverLimit(e)
	tokenPct := 0.0
	if e.TokenLimit > 0 {
		tokenPct = float64(e.TokensUsed) / float64(e.TokenLimit) * 100
	}
	creditPct := 0.0
	if e.CreditLimit > 0 {
		creditPct = e.CreditsUsed / e.CreditLimit * 100
	}
	return apiKeyExportView{
		ID:                e.ID,
		Name:              e.Name,
		KeyMasked:         config.MaskApiKey(e.Key),
		Enabled:           e.Enabled,
		RequestsCount:     e.RequestsCount,
		TokensUsed:        e.TokensUsed,
		CreditsUsed:       e.CreditsUsed,
		TokenLimit:        e.TokenLimit,
		CreditLimit:       e.CreditLimit,
		ExpiresAt:         e.ExpiresAt,
		CreatedAt:         e.CreatedAt,
		LastUsedAt:        e.LastUsedAt,
		TokenPercentUsed:  tokenPct,
		CreditPercentUsed: creditPct,
		OverToken:         overToken,
		OverCredit:        overCredit,
		Expired:           config.ApiKeyExpired(e),
	}
}

// apiExportApiKeys handles POST /admin/api/api-keys/export. It returns a masked
// usage report (never re-importable). Body: {"ids": [...]}; empty/missing = all.
func (h *Handler) apiExportApiKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	// Empty/invalid body = export all.
	_ = json.NewDecoder(r.Body).Decode(&req)

	entries := config.ListApiKeys()
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool, len(req.IDs))
		for _, id := range req.IDs {
			idSet[id] = true
		}
		filtered := entries[:0]
		for _, e := range entries {
			if idSet[e.ID] {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	views := make([]apiKeyExportView, len(entries))
	for i, e := range entries {
		views[i] = toApiKeyExportView(e)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"version":    config.Version,
		"exportedAt": time.Now().Unix(),
		"apiKeys":    views,
	})
}
```

- [ ] **Step 4: Register the route in `proxy/handler.go`**

In `handleAdminAPI`, the `/api-keys/bulk` cases are at lines 2277-2280. Add the export case immediately after the `bulk` DELETE case (before the `reset-usage` prefix case at 2281) so the exact-match route is checked before the `HasPrefix(path, "/api-keys/")` cases:

```go
	case path == "/api-keys/export" && r.Method == "POST":
		h.apiExportApiKeys(w, r)
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./proxy/ -run TestExportApiKeys -v`
Expected: PASS (both `TestExportApiKeysAllMaskedAndDerived` and `TestExportApiKeysFilterByIDs`).

- [ ] **Step 6: Vet and full package test**

Run: `go vet ./... && go test ./proxy/`
Expected: no vet errors; `ok  	kiro-go/proxy`.

- [ ] **Step 7: Commit**

```bash
git add proxy/admin_apikeys.go proxy/handler.go proxy/admin_apikeys_export_test.go
git commit -m "feat(admin): add API key usage export endpoint"
```

---

### Task 2: Frontend export button + modal + CSV

**Files:**
- Modify: `web/index.html:257-266` (add Export button to API Keys toolbar)
- Modify: `web/app.js` (new modal functions + CSV builder + event wiring in the API Keys section)

**Interfaces:**
- Consumes: `POST /api-keys/export` (from Task 1, via the `api()` helper which prefixes `/admin/api`), `apiKeysCache` (existing array of `apiKeyView`), helpers `$`, `qsa`, `t`, `escapeHtml`, `escapeAttr`, `api`, `toastWarning`, `toastError`, `toast`, `copyText`, `openDialog`, `closeDialog`, `config.MaskApiKey`-produced `keyMasked`.
- Produces: button `#exportApiKeysBtn`; functions `showApiKeyExportModal`, `renderApiKeyExportModal`, `getApiKeyExportData`, `apiKeyExportShowJson`, `apiKeyExportCopyJson`, `apiKeyExportDownloadJson`, `apiKeyExportDownloadCsv`, `buildApiKeyCsv`; module-scope `let apiKeyExportSelectedIds = new Set();`.

Note: the account export modal reuses a shared `#exportModal`/`#exportBody` dialog. To avoid clashing with it, this task reuses the SAME `#exportModal` container but renders API-key rows into `#exportBody` via `showApiKeyExportModal`. Selection state is kept in a separate `apiKeyExportSelectedIds` set so the two modals never share state.

- [ ] **Step 1: Add the Export button to `web/index.html`**

In the API Keys toolbar (the `<div class="flex items-center gap-2" style="flex-wrap:wrap;">` at line 257, containing `bulkAddApiKeyBtn` and `addApiKeyBtn`), add an Export button as the first child:

```html
                <button class="btn btn-outline btn-sm" id="exportApiKeysBtn" type="button">
                  <i class="fa-solid fa-file-export"></i>
                  <span data-i18n="apiKeys.export.button"></span>
                </button>
```

Resulting block:

```html
              <div class="flex items-center gap-2" style="flex-wrap:wrap;">
                <button class="btn btn-outline btn-sm" id="exportApiKeysBtn" type="button">
                  <i class="fa-solid fa-file-export"></i>
                  <span data-i18n="apiKeys.export.button"></span>
                </button>
                <button class="btn btn-outline btn-sm" id="bulkAddApiKeyBtn" type="button">
                  <i class="fa-solid fa-layer-group"></i>
                  <span data-i18n="apiKeys.bulkAdd"></span>
                </button>
                <button class="btn btn-outline btn-sm" id="addApiKeyBtn" type="button">
                  <i class="fa-solid fa-plus"></i>
                  <span data-i18n="apiKeys.add"></span>
                </button>
              </div>
```

- [ ] **Step 2: Add module-scope selection state in `web/app.js`**

Next to the existing `let exportSelectedIds = new Set();` (line 25), add:

```javascript
  let apiKeyExportSelectedIds = new Set();
```

- [ ] **Step 3: Add modal + CSV functions in `web/app.js`**

Insert these functions immediately after `exportDownloadJson` (ends at line 3002, before the `// Version and update` comment at 3004):

```javascript
  // API key usage export modal (masked report, not re-importable)
  function showApiKeyExportModal() {
    if (!apiKeysCache.length) return toastWarning(t('apiKeys.export.empty'));
    apiKeyExportSelectedIds = new Set(apiKeysCache.map(k => k.id));
    renderApiKeyExportModal();
    openDialog('exportModal');
  }
  function renderApiKeyExportModal() {
    const body = $('exportBody');
    const all = apiKeyExportSelectedIds.size === apiKeysCache.length;
    body.innerHTML =
      '<div class="flex items-center justify-between mb-3">' +
      '<span class="text-sm muted-text">' + escapeHtml(t('export.selected', apiKeyExportSelectedIds.size)) + '</span>' +
      '<button class="btn btn-sm btn-outline" id="apiKeyExportToggleAllBtn" type="button">' + escapeHtml(all ? t('export.deselectAll') : t('export.selectAll')) + '</button>' +
      '</div>' +
      '<div class="export-list">' +
      apiKeysCache.map(k => {
        const checked = apiKeyExportSelectedIds.has(k.id);
        const label = k.name || k.keyMasked || k.id;
        const meta = (k.keyMasked || '') + ' · ' + t('apiKeys.export.reqMeta', k.requestsCount || 0) + (k.enabled ? '' : ' · ' + t('apiKeys.export.disabled'));
        return '<label class="export-row' + (checked ? ' selected' : '') + '">' +
          '<input type="checkbox" ' + (checked ? 'checked' : '') + ' data-apikey-export-toggle="' + escapeAttr(k.id) + '" />' +
          '<div class="export-row-text">' +
          '<div class="export-row-email">' + escapeHtml(label) + '</div>' +
          '<div class="export-row-meta">' + escapeHtml(meta) + '</div>' +
          '</div>' +
          '</label>';
      }).join('') +
      '</div>' +
      '<div id="apiKeyExportJsonPreview" class="hidden mb-3"><textarea id="apiKeyExportJsonText" readonly class="font-mono"></textarea></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="apiKeyExportCloseBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-outline" id="apiKeyExportShowJsonBtn" type="button">' + escapeHtml(t('export.showJson')) + '</button>' +
      '<button class="btn btn-outline" id="apiKeyExportCopyJsonBtn" type="button">' + escapeHtml(t('export.copyJson')) + '</button>' +
      '<button class="btn btn-outline" id="apiKeyExportCsvBtn" type="button">' + escapeHtml(t('apiKeys.export.downloadCsv')) + '</button>' +
      '<button class="btn btn-primary" id="apiKeyExportJsonBtn" type="button">' + escapeHtml(t('export.downloadJson')) + '</button>' +
      '</div>';
    $('apiKeyExportToggleAllBtn').addEventListener('click', () => {
      if (apiKeyExportSelectedIds.size === apiKeysCache.length) apiKeyExportSelectedIds.clear();
      else apiKeyExportSelectedIds = new Set(apiKeysCache.map(k => k.id));
      renderApiKeyExportModal();
    });
    $('apiKeyExportCloseBtn').addEventListener('click', () => closeDialog('exportModal'));
    $('apiKeyExportShowJsonBtn').addEventListener('click', apiKeyExportShowJson);
    $('apiKeyExportCopyJsonBtn').addEventListener('click', apiKeyExportCopyJson);
    $('apiKeyExportCsvBtn').addEventListener('click', apiKeyExportDownloadCsv);
    $('apiKeyExportJsonBtn').addEventListener('click', apiKeyExportDownloadJson);
    qsa('[data-apikey-export-toggle]', body).forEach(cb => cb.addEventListener('change', e => {
      const id = e.target.dataset.apikeyExportToggle;
      if (apiKeyExportSelectedIds.has(id)) apiKeyExportSelectedIds.delete(id);
      else apiKeyExportSelectedIds.add(id);
      renderApiKeyExportModal();
    }));
  }
  async function getApiKeyExportData() {
    if (apiKeyExportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return null; }
    const res = await api('/api-keys/export', { method: 'POST', body: JSON.stringify({ ids: Array.from(apiKeyExportSelectedIds) }) });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      toastError(t('common.failed') + ': ' + (err.error || t('common.unknownError')));
      return null;
    }
    return res.json();
  }
  async function apiKeyExportShowJson() {
    const data = await getApiKeyExportData();
    if (!data) return;
    $('apiKeyExportJsonPreview').classList.remove('hidden');
    $('apiKeyExportJsonText').value = JSON.stringify(data, null, 2);
  }
  async function apiKeyExportCopyJson() {
    if (apiKeyExportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return; }
    const jsonPromise = getApiKeyExportData().then(data => {
      if (!data) throw new Error('no-data');
      return JSON.stringify(data, null, 2);
    });
    try {
      await copyText(jsonPromise);
      toast(t('export.copied'), 'primary');
    } catch (e) {
      if (e && e.message !== 'no-data') toastError(t('common.failed'));
    }
  }
  async function apiKeyExportDownloadJson() {
    const data = await getApiKeyExportData();
    if (!data) return;
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'kiro-apikeys-' + new Date().toISOString().slice(0, 10) + '.json';
    a.click();
    URL.revokeObjectURL(url);
  }
  function csvCell(v) {
    const s = String(v == null ? '' : v);
    if (/[",\n]/.test(s)) return '"' + s.replace(/"/g, '""') + '"';
    return s;
  }
  function buildApiKeyCsv(data) {
    const cols = ['id', 'name', 'keyMasked', 'enabled', 'requestsCount', 'tokensUsed',
      'creditsUsed', 'tokenLimit', 'creditLimit', 'tokenPercentUsed', 'creditPercentUsed',
      'overToken', 'overCredit', 'expired', 'expiresAt', 'createdAt', 'lastUsedAt'];
    const rows = [cols.join(',')];
    (data.apiKeys || []).forEach(k => {
      rows.push(cols.map(c => csvCell(k[c])).join(','));
    });
    return rows.join('\r\n');
  }
  async function apiKeyExportDownloadCsv() {
    const data = await getApiKeyExportData();
    if (!data) return;
    const blob = new Blob([buildApiKeyCsv(data)], { type: 'text/csv;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'kiro-apikeys-' + new Date().toISOString().slice(0, 10) + '.csv';
    a.click();
    URL.revokeObjectURL(url);
  }
```

- [ ] **Step 4: Wire the Export button**

In the API Keys event-binding block (near lines 2026-2029, where `addApiKeyBtn` / `bulkAddApiKeyBtn` are wired), add:

```javascript
    const exportBtn = $('exportApiKeysBtn');
    if (exportBtn) exportBtn.addEventListener('click', showApiKeyExportModal);
```

- [ ] **Step 5: Manual verification — build and drive the UI**

Run: `go build -o kiro-go . && CONFIG_PATH=data/config.json ./kiro-go`
Then in a browser at the admin panel → API Keys tab:
- Create at least 2 API keys (one with a token limit, generate some usage if possible).
- Click **Export** → modal opens listing keys with masked values + request count.
- Toggle-all works; individual checkboxes work.
- **Show JSON** displays masked payload with `tokenPercentUsed`/`overToken`/`expired` fields; no raw `sk-...` value visible.
- **Copy JSON** copies; toast shows.
- **Download JSON** downloads `kiro-apikeys-YYYY-MM-DD.json`.
- **Download CSV** downloads `kiro-apikeys-YYYY-MM-DD.csv`; open it — header row + one row per key, numbers raw, masked key in `keyMasked` column.

Expected: all buttons work; no console errors; raw key never appears anywhere.

- [ ] **Step 6: Commit**

```bash
git add web/index.html web/app.js
git commit -m "feat(web): API key usage export modal with JSON/CSV download"
```

---

### Task 3: i18n strings

**Files:**
- Modify: `web/locales/en.json`
- Modify: `web/locales/vi.json`
- Modify: `web/locales/zh.json`

**Interfaces:**
- Consumes: nothing.
- Produces: keys `apiKeys.export.button`, `apiKeys.export.empty`, `apiKeys.export.disabled`, `apiKeys.export.reqMeta`, `apiKeys.export.downloadCsv` in all three locales. (The modal also uses existing `export.*` and `common.*` keys, which already exist in all locales.)

- [ ] **Step 1: Add keys to `web/locales/en.json`**

After the `"apiKeys.add": "Add Key",` line (line 478), add:

```json
  "apiKeys.export.button": "Export",
  "apiKeys.export.empty": "No API keys to export",
  "apiKeys.export.disabled": "disabled",
  "apiKeys.export.reqMeta": "{0} req",
  "apiKeys.export.downloadCsv": "Download CSV",
```

- [ ] **Step 2: Add keys to `web/locales/vi.json`**

Find the `"apiKeys.add"` line and add after it:

```json
  "apiKeys.export.button": "Xuất",
  "apiKeys.export.empty": "Không có API key để xuất",
  "apiKeys.export.disabled": "đã tắt",
  "apiKeys.export.reqMeta": "{0} req",
  "apiKeys.export.downloadCsv": "Tải CSV",
```

- [ ] **Step 3: Add keys to `web/locales/zh.json`**

After the `"apiKeys.add": "添加 Key",` line (line 478), add:

```json
  "apiKeys.export.button": "导出",
  "apiKeys.export.empty": "没有可导出的 API Key",
  "apiKeys.export.disabled": "已禁用",
  "apiKeys.export.reqMeta": "{0} 次请求",
  "apiKeys.export.downloadCsv": "下载 CSV",
```

- [ ] **Step 4: Validate JSON**

Run: `python -c "import json; [json.load(open('web/locales/'+f, encoding='utf-8')) for f in ['en.json','vi.json','zh.json']]" && echo OK`
(from the `web/locales` parent or adjust the path; alternatively use `node -e "['en','vi','zh'].forEach(f=>JSON.parse(require('fs').readFileSync('web/locales/'+f+'.json')))"`)
Expected: `OK` — no JSON parse error (catches trailing-comma / duplicate-key mistakes).

- [ ] **Step 5: Verify labels render**

Reload the admin panel in each language (language switcher) → API Keys tab. The Export button and modal CSV button show translated labels, not raw keys like `apiKeys.export.button`.

- [ ] **Step 6: Commit**

```bash
git add web/locales/en.json web/locales/vi.json web/locales/zh.json
git commit -m "i18n: API key export strings (en/vi/zh)"
```

---

## Self-Review

**Spec coverage:**
- New masked JSON endpoint → Task 1. ✓
- Filter by ids / all → Task 1 (handler + `TestExportApiKeysFilterByIDs`). ✓
- Derived columns server-side (percent, over, expired) → Task 1 (`toApiKeyExportView` + test). ✓
- Include `id` → Task 1 struct + CSV column order. ✓
- Export button + modal (select, toggle-all, show/copy/download JSON, download CSV) → Task 2. ✓
- CSV client-side from same JSON, exact column order, escaping, raw numbers → Task 2 (`buildApiKeyCsv`, `csvCell`). ✓
- i18n `apiKeys.export.*` in all locales → Task 3. ✓
- One Go test → Task 1. ✓
- Files touched match spec's list. ✓

**Placeholder scan:** No TBD/TODO; all code blocks complete.

**Type consistency:** `apiKeyExportView` field names (Go json tags) match the CSV column keys in `buildApiKeyCsv` (`id, name, keyMasked, enabled, requestsCount, tokensUsed, creditsUsed, tokenLimit, creditLimit, tokenPercentUsed, creditPercentUsed, overToken, overCredit, expired, expiresAt, createdAt, lastUsedAt`). Handler name `apiExportApiKeys` consistent across Task 1 route + test. `api('/api-keys/export')` matches route `path == "/api-keys/export"`.

**Note on route ordering:** `/api-keys/export` exact-match case MUST be registered before the `strings.HasPrefix(path, "/api-keys/")` GET/PUT/DELETE cases (2284-2289), otherwise a POST would fall through. Step 4 of Task 1 places it right after the `bulk` cases — verified correct.
