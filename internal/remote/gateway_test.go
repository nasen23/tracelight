package remote

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"tracelight/internal/server"
	"tracelight/internal/trace"
)

// testRoot is a server root with one small trace, t.sqlite.
func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE StringIds (id INTEGER PRIMARY KEY, value TEXT)`,
		`INSERT INTO StringIds VALUES (1, 'k')`,
		`CREATE TABLE CUPTI_ACTIVITY_KIND_KERNEL (start INTEGER, end INTEGER, deviceId INTEGER, streamId INTEGER, shortName INTEGER)`,
		`INSERT INTO CUPTI_ACTIVITY_KIND_KERNEL VALUES (0, 10, 0, 7, 1), (20, 30, 0, 7, 1)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	return root
}

// ssoHost is a fake sign-in host: a login page, and a record of whether it
// was ever sent credentials.
type ssoHost struct {
	*httptest.Server
	mu       sync.Mutex
	hits     int
	gotCreds bool
}

func newSSO(t *testing.T) *ssoHost {
	h := &ssoHost{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.hits++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			h.gotCreds = true
		}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<!doctype html><title>Sign in</title><form>…</form>"))
	}))
	t.Cleanup(h.Close)
	return h
}

// gateway is a sign-in gateway in front of srv: unless the request has
// its session cookie, it redirects to the sign-in host, as a browser SSO
// gateway does; signed in, it serves srv under the prefix /tl/ (and
// redirects the old prefix /old/ there, and /tl to /tl/).
func gateway(t *testing.T, srv http.Handler, sso string) *httptest.Server {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("gw_session"); err != nil || c.Value != "ok" {
			http.Redirect(w, r, sso+"/cas/login?service="+url.QueryEscape("https://gateway.test"+r.URL.RequestURI()), http.StatusFound)
			return
		}
		switch p := r.URL.Path; {
		case p == "/tl":
			http.Redirect(w, r, "/tl/", http.StatusMovedPermanently)
		case strings.HasPrefix(p, "/old/"):
			http.Redirect(w, r, "/tl/"+strings.TrimPrefix(r.URL.RequestURI(), "/old/"), http.StatusPermanentRedirect)
		case strings.HasPrefix(p, "/tl/"):
			http.StripPrefix("/tl", srv).ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gw.Close)
	return gw
}

// Through a sign-in gateway that mounts the server under a prefix:
// without its session, the client says the server is behind a sign-in
// page and sends nothing to the sign-in host; with it, every call of the
// API goes through the prefix (given with or without a trailing slash,
// or an old prefix the gateway redirects on its host).
func TestGateway(t *testing.T) {
	root := testRoot(t)
	srv, err := server.New(server.Config{Root: root, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	sso := newSSO(t)
	gw := gateway(t, srv, sso.URL)

	c, _ := New(gw.URL+"/tl", "tok")
	_, err = c.Info()
	var se *SignInError
	if !errors.As(err, &se) || se.Status != http.StatusFound || !strings.HasPrefix(se.Location, sso.URL) {
		t.Fatalf("without the session: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "behind a sign-in page") || !strings.Contains(msg, strings.TrimPrefix(sso.URL, "http://")) || strings.Contains(msg, "service=") {
		t.Errorf("message %q", msg)
	}
	if sso.hits != 0 || sso.gotCreds {
		t.Errorf("the sign-in host got %d requests (credentials: %v)", sso.hits, sso.gotCreds)
	}

	for _, base := range []string{gw.URL + "/tl", gw.URL + "/tl/", gw.URL + "/tl/index.html?path=t.sqlite", gw.URL + "/old/"} {
		c, _ := New(base, "tok")
		c.SetCookie("Cookie: gw_session=ok")
		if _, err := c.Info(); err != nil {
			t.Fatalf("%s: Info: %v", base, err)
		}
		if _, entries, err := c.List(""); err != nil || len(entries) != 1 {
			t.Fatalf("%s: List: %+v %v", base, entries, err)
		}
		list, _, err := c.Session([]string{"t.sqlite"})
		if err != nil || len(list) != 1 {
			t.Fatalf("%s: Session: %v", base, err)
		}
		m, err := c.Meta(list[0].Path)
		if err != nil || m.Events != 2 {
			t.Fatalf("%s: Meta: %v", base, err)
		}
		if tl, err := c.Tile(list[0].Path, 0, 0); err != nil || len(tl.Rows) != 1 {
			t.Fatalf("%s: Tile: %v", base, err)
		}
		if st, err := c.Summary(list[0].Path, "kernel", 10, true, 0, 15); err != nil || len(st) != 1 {
			t.Fatalf("%s: Summary: %v", base, err)
		}
		if ev, err := c.Event(list[0].Path, trace.Ref{}); err != nil || ev.Name != "k" {
			t.Fatalf("%s: Event: %v", base, err)
		}
		if tr, err := c.Open(list[0].Path); err != nil || tr.EventCount() != 2 {
			t.Fatalf("%s: Open (snapshot): %v", base, err)
		}
	}
	if sso.hits != 0 {
		t.Errorf("the sign-in host got %d requests", sso.hits)
	}

	// A gateway that refuses with a page (401, 403) wants a sign-in too.
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("<html>Forbidden</html>"))
	}))
	defer refuse.Close()
	c, _ = New(refuse.URL, "")
	if _, err := c.Info(); !errors.As(err, &se) || se.Status != http.StatusForbidden {
		t.Errorf("403 page: %v", err)
	}
}
