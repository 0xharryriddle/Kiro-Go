package auth

import (
	"fmt"
	"strings"
)

// validAWSRegionLabel normalizes and validates an AWS region label for safe use
// in HOST construction. Returns (normalized, ok); ok is false for anything that
// could move the host.
//
// This exists because every AWS endpoint in this package is built by
// interpolating a region into the URL authority, e.g.
// fmt.Sprintf("https://oidc.%s.amazonaws.com", region). A region containing "@"
// splits the authority into userinfo + host, so the effective host becomes
// whatever follows the "@" while the string still LOOKS like an AWS URL. The SSO
// import flow posts a live device-session token to that base, so an unvalidated
// region is a credential-exfiltration primitive, not just an SSRF.
//
// The accepted shape mirrors proxy.validateRegionOverride: lowercase DNS-safe
// segments (letters/digits) joined by single hyphens, at least three segments,
// last segment all digits — permissive enough for us-gov-east-1 and future
// regions, strict enough that the result cannot contain "@", ".", "/", ":" or
// any other authority delimiter. That function is not reused directly because it
// lives in package proxy, which imports auth (reusing it would be an import
// cycle); the two must stay behaviourally aligned.
//
// Unlike validateRegionOverride, empty is REJECTED here: callers that want a
// default must apply it before validating, so an empty region can never silently
// produce "https://oidc..amazonaws.com".
func validAWSRegionLabel(region string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(region))
	if s == "" || len(s) > 40 {
		return "", false
	}
	if strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") || strings.Contains(s, "--") {
		return "", false
	}
	segments := strings.Split(s, "-")
	if len(segments) < 3 {
		return "", false
	}
	for _, seg := range segments {
		if seg == "" {
			return "", false
		}
		for _, r := range seg {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
				return "", false
			}
		}
	}
	last := segments[len(segments)-1]
	for _, r := range last {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return s, true
}

// awsOidcBase returns the AWS OIDC base URL for a region, failing closed when the
// region is not a valid region label. Every caller that builds an oidc.* base must
// go through this so the validation cannot be forgotten at one site.
func awsOidcBase(region string) (string, error) {
	normalized, ok := validAWSRegionLabel(region)
	if !ok {
		return "", fmt.Errorf("invalid AWS region %q", region)
	}
	return "https://oidc." + normalized + ".amazonaws.com", nil
}
