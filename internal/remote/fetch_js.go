//go:build js

package remote

import "net/http"

// inBrowser: requests go through the page's fetch, which sends the page's
// cookies (a sign-in gateway's session) itself.
const inBrowser = true

// browserRequest has fetch hand back a redirect instead of following it:
// for the API, only a gateway's sign-in page redirects, on another host
// that the page may not read (it answers as status 0, an opaque redirect).
func browserRequest(req *http.Request) { req.Header.Set("js.fetch:redirect", "manual") }
