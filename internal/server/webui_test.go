package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"tracelight/internal/trace"
)

// The WebAssembly client is served at app.html (and at / with UI "wasm"),
// relative to the page: its loader, Go's wasm_exec.js and the app,
// compressed for browsers that take gzip, with a policy that lets
// WebAssembly run and MyGo style its canvas.
func TestWebUI(t *testing.T) {
	for _, ui := range []string{"js", "wasm"} {
		srv, err := New(Config{Root: t.TempDir(), Open: trace.OpenOptions{Nsys: "/nonexistent/nsys"}, UI: ui})
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(srv)
		get := func(path, enc string) (*http.Response, []byte) {
			req, _ := http.NewRequest("GET", ts.URL+path, nil)
			if enc != "" {
				req.Header.Set("Accept-Encoding", enc)
			}
			res, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			return res, b
		}
		_, page := get("/app.html", "")
		if !bytes.Contains(page, []byte(`src="boot.js"`)) || !bytes.Contains(page, []byte(`src="wasm_exec.js"`)) || bytes.Contains(page, []byte(`src="/`)) {
			t.Errorf("%s: app.html is not the WebAssembly page, with relative paths: %.300s", ui, page)
		}
		res, b := get("/app.wasm", "gzip")
		if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/wasm" || res.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("%s: app.wasm: %d %v", ui, res.StatusCode, res.Header)
		}
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		wasm, _ := io.ReadAll(zr)
		if !bytes.HasPrefix(wasm, []byte("\x00asm")) {
			t.Errorf("%s: app.wasm is not WebAssembly", ui)
		}
		res, plain := get("/app.wasm", "identity")
		if res.Header.Get("Content-Encoding") != "" || !bytes.Equal(plain, wasm) {
			t.Errorf("%s: app.wasm without gzip: %v, %d bytes", ui, res.Header, len(plain))
		}
		csp := res.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "'wasm-unsafe-eval'") || strings.Contains(csp, "'unsafe-eval'") && !strings.Contains(csp, "'wasm-unsafe-eval'") {
			t.Errorf("policy %q", csp)
		}
		_, root := get("/", "")
		_, classic := get("/classic.html", "")
		isWasm := bytes.Contains(root, []byte(`src="boot.js"`))
		if isWasm != (ui == "wasm") || !bytes.Contains(classic, []byte(`src="app.js"`)) {
			t.Errorf("UI %s: / is the WebAssembly page: %v; classic.html: %.100s", ui, isWasm, classic)
		}
		ts.Close()
	}
}

// The WebAssembly committed is cmd/webui's as it is now, with our patches
// to MyGo (go generate ./internal/server builds it so, and checks that
// they draw the same pixels), when the toolchain is the one it was built
// with. Slow (a build for js/wasm): not with -short.
func TestWebUIUpToDate(t *testing.T) {
	if testing.Short() {
		t.Skip("builds cmd/webui for js/wasm")
	}
	want, _ := os.ReadFile("webui/BUILD")
	got, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		t.Skip(err)
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Skipf("built with %s, this is %s", strings.TrimSpace(string(want)), strings.TrimSpace(string(got)))
	}
	// As go generate builds it: with our patches to MyGo (cmd/genwebui).
	out := filepath.Join(t.TempDir(), "app.wasm")
	cmd := exec.Command("go", "run", "./cmd/genwebui", "-wasm", out)
	cmd.Dir = "../.."
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	fresh, _ := os.ReadFile(out)
	gz, _ := os.ReadFile("webui/app.wasm.gz")
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	committed, _ := io.ReadAll(zr)
	if !bytes.Equal(fresh, committed) {
		t.Fatalf("internal/server/webui/app.wasm.gz is out of date (%d bytes, cmd/webui now builds %d): run go generate ./internal/server", len(committed), len(fresh))
	}
}
