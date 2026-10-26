package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

// uiHandler serves the web clients, whose pages are both at the server's
// root, so that their API is "api/…" relative to them under any prefix:
// the JavaScript one (web/, as classic.html) and the desktop app's views
// in WebAssembly (webui/, as app.html: its loader, Go's wasm_exec.js and
// the app, kept compressed and sent so to browsers that take gzip, all of
// them). ui picks the one at / ("wasm" or "js").
func uiHandler(ui string) http.Handler {
	web, _ := fs.Sub(webFS, "web")
	webui, _ := fs.Sub(webuiFS, "webui")
	classic := http.FileServerFS(web)
	app := http.FileServerFS(webui)
	wasm, _ := fs.ReadFile(webui, "app.wasm.gz")
	page := func(fsys fs.FS, w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/" || p == "/index.html":
			if ui == "wasm" {
				page(webui, w, r)
			} else {
				page(web, w, r)
			}
		case p == "/app.html":
			page(webui, w, r)
		case p == "/classic.html":
			page(web, w, r)
		case p == "/app.wasm":
			h := w.Header()
			h.Set("Content-Type", "application/wasm")
			h.Add("Vary", "Accept-Encoding")
			if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				h.Set("Content-Encoding", "gzip")
				w.Write(wasm)
				return
			}
			gunzip(w, wasm)
		case p == "/wasm_exec.js" || p == "/boot.js" || p == "/boot.css":
			app.ServeHTTP(w, r)
		default:
			classic.ServeHTTP(w, r)
		}
	})
}

// gunzip writes gz decompressed.
func gunzip(w http.ResponseWriter, gz []byte) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	io.Copy(w, zr)
}
