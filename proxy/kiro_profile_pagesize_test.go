package proxy

import "testing"

// ListAvailableProfiles rejects any maxResults above 10 with
// HTTP 400 {"message":"Improperly formed request.","reason":"REQUEST_BODY_INVALID"}.
//
// This was established by a boundary sweep against the live CodeWhisperer
// endpoint using two different real accounts:
//
//	maxResults =  1, 5, 10          -> HTTP 200, profiles returned
//	maxResults = 11,15,20,25,30     -> HTTP 400 REQUEST_BODY_INVALID
//	maxResults = 40,49,50,100       -> HTTP 400 REQUEST_BODY_INVALID
//
// The pre-merge fork sent {"maxResults":10} and worked. Upstream v1.1.5's
// paginated rewrite hardcoded 50, and the merge adopted upstream's version
// wholesale — which broke ListAvailableProfiles for EVERY account that did not
// already have a cached profileArn.
//
// Why this test is a constant assertion rather than an HTTP round-trip:
// kiroRestAPIBase is a const, so there is no seam to point a httptest server at,
// and adding a production seam purely to enable a test would be a worse trade
// than pinning the one number that actually matters. The upstream contract is
// recorded in the comment above; this test guards the value against being
// "helpfully" raised again by a future merge or a well-meaning optimization.
//
// The failure mode is worth spelling out because it is indirect and was
// misdiagnosed as a bad account: profile resolution 400s, so GetUsageLimits
// fails, so subscriptionInfo/usageBreakdown are never populated, so the admin UI
// falls back to showing "Free" for an account on a genuine paid plan. Nothing in
// the error surfaced mentions maxResults.
func TestKiroProfilePageSizeWithinUpstreamLimit(t *testing.T) {
	const upstreamMax = 10

	if kiroProfilePageSize > upstreamMax {
		t.Fatalf("kiroProfilePageSize = %d, but ListAvailableProfiles rejects anything above %d "+
			"with HTTP 400 REQUEST_BODY_INVALID. Raising this silently breaks profile "+
			"resolution for every account without a cached profileArn, which surfaces as "+
			"accounts displaying \"Free\" on a paid plan.",
			kiroProfilePageSize, upstreamMax)
	}
	if kiroProfilePageSize < 1 {
		t.Fatalf("kiroProfilePageSize = %d, must be at least 1 or no profile is ever returned",
			kiroProfilePageSize)
	}
}

// The page size must not be so small that pagination is required for a normal
// single-profile account: that would turn every resolution into two round-trips
// for no benefit. 10 is both the upstream maximum and comfortably above the
// realistic profile count, so it should be exactly at the limit.
func TestKiroProfilePageSizeUsesFullAllowance(t *testing.T) {
	const upstreamMax = 10
	if kiroProfilePageSize != upstreamMax {
		t.Errorf("kiroProfilePageSize = %d, want %d (the full upstream allowance). "+
			"A smaller value still works but costs extra round-trips; if it was "+
			"reduced deliberately, update this test with the reason.",
			kiroProfilePageSize, upstreamMax)
	}
}
