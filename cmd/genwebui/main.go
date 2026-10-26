// Command genwebui builds the WebAssembly web client (cmd/webui) into
// internal/server/webui, which tracelight serve embeds: app.wasm.gz
// (reproducible: -trimpath, and gzip without a name or a time), Go's
// wasm_exec.js of the same toolchain, and BUILD, the toolchain it was
// built with. It builds with our patches to MyGo in place
// (third_party/mygo, go build -overlay). Run it from the module's root, or
// by go generate ./internal/server; with -wasm FILE, it only writes the
// WebAssembly to FILE.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// patches are the files of MyGo replaced, by the version they were made
// from and the SHA-256 of the original (third_party/mygo/README.md).
var patches = []struct{ module, version, file, sha256 string }{
	{"github.com/egoist/mygo", "v0.5.5", "internal/web/surface.go", "2a75c5f1b94844746e78a9b3c2b230fd4f629997a5dbc1832785e80f9968feef"},
	{"github.com/egoist/mygo", "v0.5.5", "internal/raster/raster.go", "10094ccfc68a856b19f6c9cd4daae934c3e6c3b3d675cb838123fe7d01ad0e0b"},
}

func main() {
	only := flag.String("wasm", "", "only build the WebAssembly, into this file")
	flag.BoolVar(&symbols, "symbols", false, "keep the symbols (for profiling; with -wasm)")
	flag.Parse()
	root, err := moduleRoot()
	if err != nil {
		log.Fatal(err)
	}
	out := filepath.Join(root, "internal", "server", "webui")
	wasm, err := Build(root)
	if err != nil {
		log.Fatal(err)
	}
	if *only != "" {
		must(os.WriteFile(*only, wasm, 0o644))
		return
	}
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	zw.Write(wasm)
	zw.Close()
	goroot := strings.TrimSpace(run(root, nil, "go", "env", "GOROOT"))
	execJS, err := os.ReadFile(filepath.Join(goroot, "lib", "wasm", "wasm_exec.js"))
	if err != nil {
		log.Fatal(err)
	}
	must(os.WriteFile(filepath.Join(out, "app.wasm.gz"), gz.Bytes(), 0o644))
	must(os.WriteFile(filepath.Join(out, "wasm_exec.js"), execJS, 0o644))
	must(os.WriteFile(filepath.Join(out, "BUILD"), []byte(Toolchain(root)+"\n"), 0o644))
	fmt.Printf("genwebui: %d bytes of WebAssembly, %d gzipped\n", len(wasm), gz.Len())
}

// symbols keeps the WebAssembly's symbols, for profiles.
var symbols bool

// Build builds cmd/webui for browsers and returns the WebAssembly.
func Build(root string) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "genwebui-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	defer os.RemoveAll(filepath.Join(root, ".genwebui"))
	modfile, err := patchedModFile(root, tmp)
	if err != nil {
		return nil, err
	}
	// The patches change how pixels are drawn, never which.
	stock := run(root, nil, "go", "run", "./cmd/genwebui/rastercheck")
	patched := run(root, nil, "go", "run", "-modfile="+modfile, "./cmd/genwebui/rastercheck")
	if stock != patched {
		return nil, fmt.Errorf("with third_party/mygo, MyGo draws other pixels (rastercheck: %s, patched %s)", strings.TrimSpace(stock), strings.TrimSpace(patched))
	}
	f := filepath.Join(tmp, "app.wasm")
	ldflags := "-ldflags=-s -w"
	if symbols {
		ldflags = "-ldflags="
	}
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", ldflags, "-tags=mygo_noinspector", "-modfile="+modfile, "-o", f, "./cmd/webui")
	cmd.Dir, cmd.Stderr = root, os.Stderr
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0")
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return os.ReadFile(f)
}

// patchedModFile copies each patched module into dir, with our files in
// place of their originals, and writes a go.mod (and go.sum) that is the
// module's own, but for replacing those modules with the copies; it
// returns the go.mod, for go build -modfile.
func patchedModFile(root, dir string) (string, error) {
	os.RemoveAll(filepath.Join(root, ".genwebui")) // of a build interrupted
	var replaces []string
	for _, p := range patches {
		cmd := exec.Command("go", "list", "-m", "-f", "{{.Version}} {{.Dir}}", p.module)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
		b, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("go list -m %s: %v", p.module, err)
		}
		version, modDir, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
		if version != p.version {
			return "", fmt.Errorf("%s is %s, but third_party/mygo patches %s: port the patch (see third_party/mygo/README.md)", p.module, version, p.version)
		}
		data, err := os.ReadFile(filepath.Join(modDir, filepath.FromSlash(p.file)))
		if err != nil {
			return "", err
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != p.sha256 {
			return "", fmt.Errorf("%s's %s is not the one third_party/mygo patches", p.module, p.file)
		}
		// In the module's root (removed after the build): a go.mod given
		// with -modfile resolves relative paths against the main module's.
		cp := filepath.Join(root, ".genwebui", strings.ReplaceAll(p.module, "/", "_"))
		if _, err := os.Stat(cp); err != nil {
			if err := copyTree(modDir, cp); err != nil {
				return "", err
			}
			// Relative to the go.mod: the build records it as written, the
			// same in every build.
			rel, _ := filepath.Rel(root, cp)
			replaces = append(replaces, fmt.Sprintf("replace %s => ./%s", p.module, filepath.ToSlash(rel)))
		}
		ours, err := os.ReadFile(filepath.Join(root, "third_party", "mygo", filepath.FromSlash(p.file)+".overlay"))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(cp, filepath.FromSlash(p.file)), ours, 0o644); err != nil {
			return "", err
		}
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	sum, _ := os.ReadFile(filepath.Join(root, "go.sum"))
	f := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(f, append(mod, []byte("\n"+strings.Join(replaces, "\n")+"\n")...), 0o644); err != nil {
		return "", err
	}
	return f, os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644)
}

// copyTree copies the files of src into dst, writable.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(to, data, 0o644)
	})
}

// Toolchain is the Go version that builds it (the same one must, for the
// same bytes and a matching wasm_exec.js).
func Toolchain(root string) string {
	return strings.TrimSpace(run(root, nil, "go", "env", "GOVERSION"))
}

func run(dir string, env []string, name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
	b, err := cmd.Output()
	if err != nil {
		log.Fatalf("%s %v: %v", name, args, err)
	}
	return string(b)
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above the working directory")
		}
		dir = parent
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
