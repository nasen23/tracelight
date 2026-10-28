// Package remote is the desktop app's client of `tracelight serve`: it lists
// the traces on a server, and streams them as the web client does (the
// trace's rows first, then tiles of the timeline as they show, summaries
// and events' details on demand), or fetches one whole as a snapshot to
// keep for offline.
package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"tracelight/internal/trace"
)

// UserAgent is the User-Agent of requests ("Tracelight/<version>").
var UserAgent = "Tracelight"

// Client talks to a server.
type Client struct {
	base  *url.URL
	token string
	// cookie is a Cookie header for a gateway in front of the server that
	// signs browsers in (see SetCookie); it goes to the server's host only.
	// onSignIn is told when a request finds that the gateway wants the user
	// to sign in again. Both under mu: requests run in parallel.
	mu       sync.Mutex
	cookie   string
	onSignIn func(*SignInError)
	http     *http.Client
}

// SetCookie sets the Cookie header sent with every request: the session of
// a sign-in gateway in front of the server, from the app's sign-in window
// or pasted from a browser. It is sent to the server's host only.
func (c *Client) SetCookie(header string) {
	c.mu.Lock()
	c.cookie = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "Cookie:"))
	c.mu.Unlock()
}

// OnSignIn sets fn to be told, from the request's goroutine, when a request
// finds that the gateway in front of the server wants the user to sign in
// (again: the session ended), whatever the request was.
func (c *Client) OnSignIn(fn func(*SignInError)) {
	c.mu.Lock()
	c.onSignIn = fn
	c.mu.Unlock()
}

// SignInError says that a gateway in front of the server wants the user
// to sign in: it sent the API's request to a sign-in page (a redirect to
// another host), or answered with a page or refused it (401, 403).
type SignInError struct {
	Server   string // the server's URL
	Endpoint string // the request, "/api/info"
	Status   int
	// Location is where the gateway redirected, if it did (its host only
	// is shown: the rest can carry tickets).
	Location string
}

func (e *SignInError) Error() string {
	why := fmt.Sprintf("answered %s with %d %s", e.Endpoint, e.Status, http.StatusText(e.Status))
	if e.Location != "" {
		host := e.Location
		if u, err := url.Parse(e.Location); err == nil && u.Host != "" {
			host = u.Host
		}
		why = "sent " + e.Endpoint + " to a sign-in page on " + host
	}
	return "the server at " + e.Server + " is behind a sign-in page (it " + why + "): sign in from Tracelight, or give the browser's Cookie header"
}

// Entry is a file or directory on the server.
type Entry struct {
	Name  string    `json:"name"`
	Path  string    `json:"path"`
	Type  string    `json:"type"` // "dir", or the trace's format ("sqlite", "report")
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
	// Ready says whether the server can open the trace now; Note, how or
	// why not.
	Ready bool   `json:"ready"`
	Note  string `json:"note"`
}

// Link is what a URL copied from the browser says: the server, the
// traces the web page shows, and its token.
type Link struct {
	// Base is the server's URL, ending in "/".
	Base string
	// Paths are the ?path= of a web link (files, directories, globs).
	Paths []string
	// Token is the #token= (or ?token=) of the link.
	Token string
	// View is the #v=a,b of the link, the times shown (ns from the start;
	// zero if none); Align its #align= (the traces' alignment), if any.
	View  [2]float64
	Align string
	// Only and Omit are the #only= and #omit= of a session's link: the
	// traces shown, or those left out, by name (rank3 of rank3.sqlite or
	// of job.json.gz#rank3), separated by commas.
	Only, Omit []string
	// All is #all=1: open every trace the paths name, however many, without
	// asking which (the chooser).
	All bool
}

// LinkURL is the link to paths on the server at base, as the web page's
// address says it (?path=, repeated): one to paste in the path box.
func LinkURL(base string, paths []string) string {
	u := strings.TrimSuffix(base, "/") + "/"
	for i, p := range paths {
		sep := "&"
		if i == 0 {
			sep = "?"
		}
		u += sep + "path=" + strings.ReplaceAll(url.QueryEscape(p), "%2F", "/")
	}
	return u
}

// ParseLink reads a server's address as people paste it: a base URL with
// or without "/" or a scheme ("127.0.0.1:8737"), the web page's URL
// (".../index.html"), or a link to traces as the web client makes them
// ("http://host:8737/?path=/abs/dir&path=/abs/b.sqlite#v=…&token=…"). The
// base is the URL's directory: the page's file and the query and fragment
// left out.
func ParseLink(raw string) (Link, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Link{}, errors.New("no server address")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Link{}, err
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return Link{}, fmt.Errorf("%q is not an http:// or https:// URL", raw)
	}
	var l Link
	l.Paths = u.Query()["path"]
	l.Token = u.Query().Get("token")
	frag, _ := url.ParseQuery(u.Fragment)
	if t := frag.Get("token"); t != "" {
		l.Token = t
	}
	if len(l.Paths) == 0 {
		l.Paths = frag["path"] // the older #path= links
	}
	if v := strings.Split(frag.Get("v"), ","); len(v) == 2 {
		a, err1 := strconv.ParseFloat(v[0], 64)
		b, err2 := strconv.ParseFloat(v[1], 64)
		if err1 == nil && err2 == nil && b > a {
			l.View = [2]float64{a, b}
		}
	}
	if a := frag.Get("align"); a == "abs" || a == "start" || a == "collectives" {
		l.Align = a
	}
	l.Only, l.Omit = names(frag["only"]), names(frag["omit"])
	l.All = frag.Get("all") == "1"
	// The page's directory: "/index.html" or "/app/index.html" are pages
	// of the server at "/" or "/app/"; any other path is a prefix the
	// server is mounted under ("/proxy/10.0.0.5:8737" too).
	p := u.Path
	if i := strings.LastIndex(p, "/"); i >= 0 {
		if last := strings.ToLower(p[i+1:]); strings.HasSuffix(last, ".html") || strings.HasSuffix(last, ".htm") {
			p = p[:i+1]
		}
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment, u.User = p, "", "", "", nil
	l.Base = u.String()
	return l, nil
}

// New makes a client of the server at rawURL (any form ParseLink reads;
// what it says besides the server is left out).
func New(rawURL, token string) (*Client, error) {
	l, err := ParseLink(rawURL)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(l.Base)
	// Tiles come several at once: keep a connection for each.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 16
	c := &Client{base: u, token: token}
	c.http = &http.Client{Timeout: 10 * time.Minute, Transport: tr, CheckRedirect: c.redirect}
	return c, nil
}

// errOtherHost stops a redirect to another host: for the API, a gateway's
// sign-in page; the token and cookie are never sent there.
var errOtherHost = errors.New("redirected to another host")

// redirect follows redirects on the server's host (to https too, and a
// path the server moved), at most 5, and no other: the request is
// answered with the redirect, which get reports as a sign-in.
func (c *Client) redirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if !sameHost(req.URL, c.base) {
		return http.ErrUseLastResponse
	}
	return nil
}

// sameHost reports whether u is on base's host: the same host name and
// port, or the same name upgraded from http to https.
func sameHost(u, base *url.URL) bool {
	if u.Host == base.Host && u.Scheme == base.Scheme {
		return true
	}
	return base.Scheme == "http" && u.Scheme == "https" && u.Hostname() == base.Hostname()
}

// String is the server's URL.
func (c *Client) String() string { return c.base.String() }

func (c *Client) get(endpoint string, params url.Values) (*http.Response, error) {
	c.mu.Lock()
	sent := c.cookie
	c.mu.Unlock()
	res, err := c.request(endpoint, params)
	var se *SignInError
	if errors.As(err, &se) {
		c.mu.Lock()
		fn, renewed := c.onSignIn, c.cookie != sent
		c.mu.Unlock()
		if fn != nil && !renewed { // not a request of the session before
			fn(se)
		}
	}
	return res, err
}

func (c *Client) request(endpoint string, params url.Values) (*http.Response, error) {
	u := c.base.JoinPath("api", endpoint)
	u.RawQuery = params.Encode()
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	c.mu.Lock()
	cookie := c.cookie
	c.mu.Unlock()
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if UserAgent != "" && !inBrowser {
		req.Header.Set("User-Agent", UserAgent)
	}
	browserRequest(req)
	res, err := c.http.Do(req) // the transport asks for gzip and inflates it
	if err != nil {
		return nil, err
	}
	// Tracelight's API answers in JSON (snapshots in binary, as
	// application/octet-stream); anything else is not it: a web page, a
	// proxy's or a login page, another server on the port.
	ct := res.Header.Get("Content-Type")
	isJSON := strings.Contains(ct, "json")
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		server := strings.TrimSuffix(c.base.String(), "/")
		switch {
		case res.StatusCode == 0 && inBrowser:
			// The browser's opaque redirect: to a sign-in page.
			return nil, &SignInError{Server: server, Endpoint: "/api/" + endpoint, Status: http.StatusFound}
		case res.StatusCode >= 300 && res.StatusCode < 400:
			// A redirect to another host (CheckRedirect stopped it).
			return nil, &SignInError{Server: server, Endpoint: "/api/" + endpoint, Status: res.StatusCode, Location: res.Header.Get("Location")}
		case (res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden) && !isJSON:
			return nil, &SignInError{Server: server, Endpoint: "/api/" + endpoint, Status: res.StatusCode}
		}
		if !isJSON {
			return nil, c.notAPI(res, endpoint)
		}
		var e struct{ Error string }
		json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&e)
		if res.StatusCode == http.StatusUnauthorized {
			if c.token != "" {
				return nil, errors.New("the server refused the token")
			}
			return nil, errors.New("the server needs a token")
		}
		if e.Error == "" {
			e.Error = res.Status
		}
		return nil, errors.New(e.Error)
	}
	if !isJSON && !(endpoint == "snapshot" && strings.HasPrefix(ct, "application/octet-stream")) {
		res.Body.Close()
		return nil, c.notAPI(res, endpoint)
	}
	return res, nil
}

// notAPI is the error of an answer that is not Tracelight's API.
func (c *Client) notAPI(res *http.Response, endpoint string) error {
	what := res.Header.Get("Content-Type")
	head := make([]byte, 64)
	n, _ := io.ReadFull(res.Body, head)
	body := strings.TrimSpace(string(head[:n]))
	switch {
	case strings.HasPrefix(what, "text/html") || strings.HasPrefix(body, "<"):
		what = "HTML"
	case what == "":
		what = "an untyped body"
	default:
		what, _, _ = strings.Cut(what, ";")
	}
	return fmt.Errorf("the server at %s answered /api/%s with %s (%d %s), not Tracelight's API — is this the right address? (If it opens in a browser only after signing in, sign in from Tracelight.)",
		strings.TrimSuffix(c.base.String(), "/"), endpoint, what, res.StatusCode, http.StatusText(res.StatusCode))
}

// List returns the directory at dir ("" for the root) and its entries.
func (c *Client) List(dir string) (string, []Entry, error) {
	res, err := c.get("files", url.Values{"dir": {dir}})
	if err != nil {
		return "", nil, err
	}
	defer res.Body.Close()
	var out struct {
		Dir     string  `json:"dir"`
		Entries []Entry `json:"entries"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", nil, err
	}
	return out.Dir, out.Entries, nil
}

// Open fetches the trace at path, parsed by the server. Its Path is the
// trace's URL on the server.
func (c *Client) Open(path string) (*trace.Trace, error) {
	res, err := c.get("snapshot", url.Values{"path": {path}})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	tr, err := trace.ReadSnapshot(res.Body)
	if err != nil {
		return nil, err
	}
	tr.Path = c.base.String() + path
	return tr, nil
}

// names reads the trace names of #only= or #omit= values: separated by
// commas, the values repeated or not.
func names(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}
