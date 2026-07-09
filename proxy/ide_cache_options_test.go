package proxy

import "testing"

func TestApplyIdeCacheImportOptionsEnterpriseM365(t *testing.T) {
	req := importCredentialRequest{AuthMethod: "external_idp", Provider: "ExternalIdp"}
	got := applyIdeCacheImportOptions(req, ideCacheImportOptions{Mode: "enterprise_m365"})
	if got.AuthMethod != "external_idp" {
		t.Fatalf("authMethod = %q, want external_idp", got.AuthMethod)
	}
	if got.Provider != "AzureAD" {
		t.Fatalf("provider = %q, want AzureAD", got.Provider)
	}
	if got.ProxyURL != directProxyOptOut {
		t.Fatalf("proxyURL = %q, want direct opt-out", got.ProxyURL)
	}
}

func TestApplyIdeCacheImportOptionsCanDisableDirectProxy(t *testing.T) {
	directProxy := false
	req := importCredentialRequest{AuthMethod: "external_idp", Provider: "ExternalIdp"}
	got := applyIdeCacheImportOptions(req, ideCacheImportOptions{Mode: "enterprise_m365", DirectProxy: &directProxy})
	if got.ProxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty when directProxy=false", got.ProxyURL)
	}
}

func TestApplyIdeCacheImportOptionsForceProvider(t *testing.T) {
	req := importCredentialRequest{AuthMethod: "external_idp", Provider: "ExternalIdp"}
	got := applyIdeCacheImportOptions(req, ideCacheImportOptions{Mode: "enterprise_m365", ForceProvider: "ContosoIDP"})
	if got.Provider != "ContosoIDP" {
		t.Fatalf("provider = %q, want forced provider", got.Provider)
	}
}
