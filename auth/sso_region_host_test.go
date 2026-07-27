package auth

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Every AWS OIDC/SSO base URL in this package is built by interpolating a
// caller-supplied region straight into the URL AUTHORITY:
//
//	fmt.Sprintf("https://oidc.%s.amazonaws.com", region)
//
// Nothing validates that the region is an AWS region label. A region containing
// "@" splits the authority into userinfo + host, so the effective host becomes
// whatever follows the "@" and ".amazonaws.com" is demoted to a path segment.
//
// This matters more here than for a plain SSRF, because of WHAT gets sent to
// that host. ImportFromSsoToken first obtains a REAL device-session token from
// the hardcoded legitimate portal (portal.sso.us-east-1.amazonaws.com), then
// posts that live credential to oidcBase:
//
//	acceptUserCode(oidcBase, userCode, deviceSessionToken)   // sso_token.go:48
//	  -> payload {"userCode":..., "userSessionId": deviceSessionToken}
//	     POST oidcBase + "/device_authorization/accept_user_code"
//
// So a crafted region exfiltrates a live AWS SSO device-session credential to
// an attacker-controlled host. Region reaches this function from the operator
// import surface, so this is a stored-input -> outbound-credential path.
//
// The same interpolation exists in iam_sso.go, builderid.go and oidc.go, so this
// test pins the shared property: a region must never be able to move the host.
func TestAuthOidcBaseCannotBeRetargetedByRegion(t *testing.T) {
	hostile := []string{
		"x@attacker.example",
		"x@attacker.example/",
		"x@attacker.example:8443",
		"attacker.example/..",
	}

	for _, region := range hostile {
		t.Run(region, func(t *testing.T) {
			// Go through the PRODUCTION funnel every oidc.* caller now uses
			// (awsOidcBase), not a hand-rolled fmt.Sprintf — otherwise this
			// test would bypass the code under audit and prove nothing.
			raw, err := awsOidcBase(region)
			if err != nil {
				// Refusing to build the base URL is the correct outcome:
				// nothing can be sent, so no credential can leak.
				return
			}

			u, err := url.Parse(raw)
			if err != nil {
				return // unparseable is a safe outcome: nothing can be sent
			}
			host := strings.ToLower(u.Hostname())

			if host != "" && !strings.HasSuffix(host, ".amazonaws.com") {
				// Confirm the request would actually be built and dispatched,
				// i.e. this is reachable rather than theoretical.
				req, reqErr := http.NewRequest(http.MethodPost,
					raw+"/device_authorization/accept_user_code", strings.NewReader("{}"))
				built := reqErr == nil && req != nil

				t.Fatalf("region %q retargeted the AWS OIDC base to host %q (url=%s); "+
					"request build succeeded=%v. ImportFromSsoToken posts a LIVE "+
					"device-session token to this host, so a crafted region "+
					"exfiltrates an AWS SSO credential",
					region, host, raw, built)
			}
		})
	}
}

// Positive control: real regions must still produce the correct AWS host.
// Without this, "reject everything" would pass while breaking every SSO import.
func TestAuthOidcBaseStillAcceptsRealRegions(t *testing.T) {
	for _, region := range []string{"us-east-1", "us-west-2", "eu-central-1", "ap-southeast-2"} {
		normalized, ok := validAWSRegionLabel(region)
		if !ok {
			t.Fatalf("legitimate region %q was rejected", region)
		}
		if normalized != region {
			t.Fatalf("region %q normalized to %q, want unchanged", region, normalized)
		}
		u, err := url.Parse(fmt.Sprintf("https://oidc.%s.amazonaws.com", normalized))
		if err != nil {
			t.Fatalf("region %q produced an unparseable URL: %v", region, err)
		}
		want := "oidc." + region + ".amazonaws.com"
		if got := u.Hostname(); got != want {
			t.Fatalf("region %q produced host %q, want %q", region, got, want)
		}
	}
}

// The validator must reject every hostile shape outright.
func TestValidAWSRegionLabelRejectsHostileShapes(t *testing.T) {
	for _, bad := range []string{
		"x@attacker.example",
		"x@attacker.example/",
		"x@attacker.example:8443",
		"attacker.example/..",
		"us-east-1/../..",
		"us_east_1",
		"US-EAST-1-",
		"-us-east-1",
		"us--east-1",
		"us.east.1",
		"us-east",
		"",
	} {
		if _, ok := validAWSRegionLabel(bad); ok {
			t.Fatalf("validAWSRegionLabel(%q) accepted a value unsafe for host construction", bad)
		}
	}
}
