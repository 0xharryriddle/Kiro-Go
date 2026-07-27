package proxy

import (
	"strings"
	"testing"

	"kiro-go/config"
)

// A Bedrock account's Region is interpolated straight into the request URL's
// AUTHORITY:
//
//	fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/%s", region, ...)
//
// Nothing validates that region is an AWS region label. A region containing an
// "@" splits the authority into userinfo + host, so the real host becomes
// whatever follows the "@" — and `.amazonaws.com` is demoted to a path/suffix of
// the attacker's choosing rather than the host.
//
// authorizeBedrockRequest then attaches the account's live credential to that
// request: an `Authorization: Bearer <BedrockAPIKey>` header, or a SigV4
// signature carrying the access-key ID (and STS session token when present).
//
// Consequence: one crafted region field exfiltrates the account credential to an
// attacker-controlled host on the first invocation. Region is operator-supplied
// via the admin/import surfaces, so this is a stored-input → outbound-credential
// path, not a hypothetical.
//
// This test asserts the HOST, not the error, so it holds regardless of how the
// fix is implemented (reject the region, or build the URL safely).
func TestBedrockEndpointCannotBeRetargetedByRegion(t *testing.T) {
	hostile := []struct {
		name   string
		region string
		// host that must NEVER be contacted
		attacker string
	}{
		{"userinfo split", "x@attacker.example", "attacker.example"},
		{"userinfo split with path", "x@attacker.example/", "attacker.example"},
		{"port injection", "x@attacker.example:8443", "attacker.example"},
	}

	for _, tc := range hostile {
		t.Run(tc.name, func(t *testing.T) {
			raw := bedrockEndpoint(tc.region, "anthropic.claude-3-5-sonnet-20241022-v2:0", false)

			// Go through the PRODUCTION request funnel that every Bedrock
			// caller uses (invoke and Converse both reach
			// newBedrockRequestForURL), not a hand-rolled http.NewRequest —
			// otherwise this test would bypass the code under audit.
			req, err := newBedrockRequestForURL(raw, []byte("{}"))
			if err != nil {
				// Refusing to build the request is the correct outcome:
				// nothing can be sent, so no credential can leak.
				return
			}

			host := req.URL.Hostname()
			if host == tc.attacker {
				// Show that the credential would ride along to that host.
				acct := &config.Account{
					AuthMethod:    "bedrock",
					Region:        tc.region,
					BedrockAPIKey: "ABSKtestcredentialvalue123456",
				}
				_ = authorizeBedrockRequest(acct, req, []byte("{}"), tc.region)
				authz := req.Header.Get("Authorization")

				t.Fatalf("region %q retargeted the Bedrock endpoint to host %q "+
					"(url=%s); credential attached: Authorization header present=%v. "+
					"A crafted region exfiltrates the account credential to an "+
					"attacker-controlled host on first invoke",
					tc.region, host, raw, authz != "")
			}

			if !strings.HasSuffix(host, ".amazonaws.com") {
				t.Fatalf("region %q produced non-AWS host %q (url=%s); the Bedrock "+
					"endpoint host must always be an amazonaws.com host",
					tc.region, host, raw)
			}
		})
	}
}

// Positive control: legitimate AWS regions must still build the correct endpoint.
// Without this, "reject everything" would look like a valid fix while breaking
// every Bedrock account.
func TestBedrockEndpointStillAcceptsRealRegions(t *testing.T) {
	for _, region := range []string{"us-east-1", "us-west-2", "eu-central-1", "ap-southeast-2"} {
		raw := bedrockEndpoint(region, "anthropic.claude-3-5-sonnet-20241022-v2:0", true)
		req, err := newBedrockRequestForURL(raw, []byte("{}"))
		if err != nil {
			t.Fatalf("legitimate region %q failed to build a request: %v", region, err)
		}
		want := "bedrock-runtime." + region + ".amazonaws.com"
		if got := req.URL.Hostname(); got != want {
			t.Fatalf("region %q produced host %q, want %q", region, got, want)
		}
		if !strings.HasSuffix(raw, "/invoke-with-response-stream") {
			t.Fatalf("streaming endpoint lost its verb: %s", raw)
		}
	}
}
