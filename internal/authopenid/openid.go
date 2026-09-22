// Package authopenid wraps the small surface of github.com/yohcop/openid-go
// that gather-your-party uses (RedirectURL + Verify), so that:
//
//   - the auth handlers depend on this local package instead of the
//     third-party directly, keeping the OpenID call sites narrow;
//   - the verify step can be stubbed in tests by overriding the
//     package-level VerifyFn variable (CLM-5, CLM-6 unit tests).
//
// The realm/return_to values are ALWAYS derived from APP_BASE_URL config
// by the callers (view.LoginRedirect / view.SteamCallback). This wrapper
// does not read r.Host / r.Header / r.URL.Host anywhere. CLM-2.
package authopenid

import (
	"net/http"

	"github.com/yohcop/openid-go"
)

// SteamOpenIDEndpoint is Steam's OpenID 2.0 provider URL. CLM-1.
const SteamOpenIDEndpoint = "https://steamcommunity.com/openid/login"

// DiscoveryCache and NonceStore are the yohcop-provided in-memory
// implementations; a single instance is used process-wide.
var (
	nonceStore     = openid.NewSimpleNonceStore()
	discoveryCache = openid.NewSimpleDiscoveryCache()
)

// RedirectURL builds the URL to send the user to Steam's OpenID provider.
// realm and returnTo MUST already have been derived from APP_BASE_URL
// config by the caller. This function does not observe the incoming request.
//
// We call yohcop's BuildRedirectURL directly with the known Steam
// endpoint + the identifier_select convention (Steam's discovery would
// return exactly this). Using BuildRedirectURL avoids yohcop's Discover
// step, which would perform a network fetch on every /login request and
// would also make unit tests network-dependent.
func RedirectURL(returnTo, realm string) (string, error) {
	// identifier_select — Steam authenticates the user server-side and
	// returns the actual SteamID in the claimed_id of the assertion.
	const idSelect = "http://specs.openid.net/auth/2.0/identifier_select"
	return openid.BuildRedirectURL(SteamOpenIDEndpoint, idSelect, idSelect, returnTo, realm)
}

// VerifyFn is the callback-verification hook. It is a package-level
// variable so tests can stub the OpenID round-trip without touching the
// network. Real callers do not reassign this.
//
// The default implementation calls openid.Verify with the process-wide
// nonce store + discovery cache. It performs the check_authentication
// round-trip AND realm/return_to matching. CLM-5.
var VerifyFn = func(fullURL string) (claimedID string, err error) {
	return openid.Verify(fullURL, discoveryCache, nonceStore)
}

// Verify is the exported entry point handlers call. It just delegates to
// VerifyFn so tests can swap the implementation.
func Verify(fullURL string) (claimedID string, err error) {
	return VerifyFn(fullURL)
}

// CallbackFullURL reconstructs the URL yohcop's Verify needs (the full
// URL Steam redirected the browser to, INCLUDING the query string).
//
// The scheme+host portion is taken from appBaseURL (config), NOT from
// the request — CLM-2 MUST-forbid. Only the path+query are lifted from
// the request, which is safe because the assertion payload lives in the
// query and yohcop uses the URL only to re-parse those params.
func CallbackFullURL(appBaseURL string, r *http.Request) string {
	// Strip a trailing "/" so we don't build ...base///path.
	base := appBaseURL
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	req := r.URL.RequestURI()
	return base + req
}
