package remote

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"tracelight/internal/server"
)

func TestClient(t *testing.T) {
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
	os.Mkdir(filepath.Join(root, "sub"), 0o755)
	s, err := server.New(server.Config{Root: root, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()

	c, _ := New(ts.URL, "tok")
	dir, entries, err := c.List("")
	if err != nil || dir != "" || len(entries) != 2 || entries[0].Type != "dir" || entries[1].Name != "t.sqlite" || !entries[1].Ready {
		t.Fatalf("List: %q %+v %v", dir, entries, err)
	}
	tr, err := c.Open("t.sqlite")
	if err != nil || tr.EventCount() != 2 || !strings.HasSuffix(tr.Path, "/t.sqlite") {
		t.Fatalf("Open: %v", err)
	}
	if _, err := c.Open("../etc/passwd"); err == nil {
		t.Error("a path outside the root opened")
	}
	bad, _ := New(ts.URL, "wrong")
	if _, _, err := bad.List(""); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("wrong token: %v", err)
	}
	if _, err := New("ftp://x", ""); err == nil {
		t.Error("an ftp URL was accepted")
	}
}

// Whatever is copied from a browser reads as the server, the traces it
// links to and its token.
// A link's view and alignment (#v=a,b, #align=), as both web clients write
// them.
func TestParseLinkView(t *testing.T) {
	for _, c := range []struct {
		in    string
		view  [2]float64
		align string
	}{
		{"http://h:1/?path=/a#v=1000,2000&align=start", [2]float64{1000, 2000}, "start"},
		{"http://h:1/app.html?path=/a#v=1000%2C2000", [2]float64{1000, 2000}, ""},
		{"http://h:1/?path=/a#v=2000,1000&align=sideways", [2]float64{}, ""},
		{"http://h:1/?path=/a#token=t&align=abs", [2]float64{}, "abs"},
	} {
		l, err := ParseLink(c.in)
		if err != nil || l.View != c.view || l.Align != c.align {
			t.Errorf("ParseLink(%q): view %v align %q, want %v %q", c.in, l.View, l.Align, c.view, c.align)
		}
	}
}

func TestParseLink(t *testing.T) {
	for _, c := range []struct {
		in, base, token string
		paths           []string
	}{
		{"http://127.0.0.1:8737", "http://127.0.0.1:8737/", "", nil},
		{"http://127.0.0.1:8737/", "http://127.0.0.1:8737/", "", nil},
		{"  127.0.0.1:8737  ", "http://127.0.0.1:8737/", "", nil},
		{"localhost:8737/", "http://localhost:8737/", "", nil},
		{"http://127.0.0.1:8737/index.html", "http://127.0.0.1:8737/", "", nil},
		{"https://gpu.example.org/tl/index.html#token=s3", "https://gpu.example.org/tl/", "s3", nil},
		{"https://gpu.example.org/tl", "https://gpu.example.org/tl/", "", nil},
		{"http://127.0.0.1:8737/?path=/data/job7", "http://127.0.0.1:8737/", "", []string{"/data/job7"}},
		{"http://127.0.0.1:8737/?path=/data/job7/rank0.sqlite&path=/data/job7/rank1.sqlite#v=1,2&sel=0:1:2:3&token=abc",
			"http://127.0.0.1:8737/", "abc", []string{"/data/job7/rank0.sqlite", "/data/job7/rank1.sqlite"}},
		{"http://127.0.0.1:8737/?path=/data/job*/rank%5B0-3%5D.sqlite#align=start", "http://127.0.0.1:8737/", "", []string{"/data/job*/rank[0-3].sqlite"}},
		{"http://127.0.0.1:8737/index.html?path=runs/a.sqlite", "http://127.0.0.1:8737/", "", []string{"runs/a.sqlite"}},
		{"http://127.0.0.1:8737/#path=runs/a.sqlite&token=t", "http://127.0.0.1:8737/", "t", []string{"runs/a.sqlite"}},
		{"http://user:pw@127.0.0.1:8737/", "http://127.0.0.1:8737/", "", nil},
	} {
		l, err := ParseLink(c.in)
		if err != nil || l.Base != c.base || l.Token != c.token || !slices.Equal(l.Paths, c.paths) {
			t.Errorf("ParseLink(%q) = %+v, %v; want %s %v %q", c.in, l, err, c.base, c.paths, c.token)
		}
	}
	for _, in := range []string{"", "ftp://host/", "http://"} {
		if _, err := ParseLink(in); err == nil {
			t.Errorf("ParseLink(%q): no error", in)
		}
	}
}

// An answer that is not Tracelight's API (a web page, a proxy's page,
// plain text) says so: what answered which request, with what status.
func TestNotAPIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<!doctype html><title>Sign in</title>")
		case "/api/files":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(404)
			io.WriteString(w, "404 page not found\n")
		default:
			w.WriteHeader(502)
			io.WriteString(w, "<html>Bad gateway</html>")
		}
	}))
	defer ts.Close()
	c, err := New(ts.URL+"/?path=/x", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Info()
	want := "the server at " + ts.URL + " answered /api/info with HTML (200 OK), not Tracelight's API — is this the right address?"
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Info: %v\nwant %s", err, want)
	}
	if _, _, err = c.List(""); err == nil || !strings.Contains(err.Error(), "answered /api/files with text/plain (404 Not Found)") {
		t.Errorf("List: %v", err)
	}
	if _, err = c.Meta("/x"); err == nil || !strings.Contains(err.Error(), "answered /api/trace with HTML (502 Bad Gateway)") {
		t.Errorf("Meta: %v", err)
	}
}

// A session's link says which traces it shows: #only= or #omit=, names
// separated by commas or repeated.
func TestParseLinkTraces(t *testing.T) {
	for _, c := range []struct {
		in         string
		only, omit []string
	}{
		{"http://h:8737/?path=/d#only=rank0,rank8", []string{"rank0", "rank8"}, nil},
		{"http://h:8737/?path=/d#omit=rank3&omit=rank5,%20rank6", nil, []string{"rank3", "rank5", "rank6"}},
		{"http://h:8737/?path=/d#v=1,2&only=", nil, nil},
		{"http://h:8737/?path=/d", nil, nil},
	} {
		l, err := ParseLink(c.in)
		if err != nil || !slices.Equal(l.Only, c.only) || !slices.Equal(l.Omit, c.omit) {
			t.Errorf("ParseLink(%q): only %q omit %q, want %q %q (%v)", c.in, l.Only, l.Omit, c.only, c.omit, err)
		}
	}
}
