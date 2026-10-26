package server

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"tracelight/internal/trace"
)

// Errors of path resolution, which the handlers turn into status codes.
var (
	errOutside  = errors.New("that path is outside the directory this server serves (its --root)")
	errBadPath  = errors.New("bad path")
	errNotFound = errors.New("no such file or directory on the server")
)

// root confines file access to one directory.
//
// Clients name files by slash-separated paths relative to the root, or by
// absolute paths. A relative path is rejected if it contains "..", NUL or a
// backslash; any path is rejected if it resolves, once every symbolic link
// in it is followed, to anywhere outside the root (itself resolved the same
// way). The check uses the real path the
// file is then opened by, so a link pointing out of the root cannot be used
// even when it sits inside it.
type root struct {
	dir string // absolute, symbolic links resolved
}

func newRoot(dir string) (*root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errors.New(dir + " is not a directory")
	}
	return &root{dir: real}, nil
}

// clean checks a client's path and returns it in canonical slash form, ""
// for the root.
func clean(p string) (string, error) {
	if strings.ContainsAny(p, "\x00\\") {
		return "", errBadPath
	}
	if p == "" || p == "." || p == "/" {
		return "", nil
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return "", errOutside
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", errOutside
		}
	}
	c := path.Clean(p)
	if c == "." {
		return "", nil
	}
	return c, nil
}

// resolve returns the real path of the client's path p, which must exist
// and be inside the root. p is relative to the root (and may not contain
// ".."), or absolute: an absolute path is resolved as the server's file
// system has it, and is accepted only if, once every symbolic link in it is
// followed, it is inside the root (anywhere with a root of "/").
func (r *root) resolve(p string) (string, error) {
	var full string
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		if strings.ContainsRune(p, 0) {
			return "", errBadPath
		}
		full = filepath.Clean(p)
	} else {
		c, err := clean(p)
		if err != nil {
			return "", err
		}
		full = filepath.Join(r.dir, filepath.FromSlash(c))
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errNotFound
		}
		return "", err
	}
	if !r.contains(real) {
		return "", errOutside
	}
	return real, nil
}

// contains reports whether the real path p is the root or inside it.
func (r *root) contains(p string) bool {
	rel, err := filepath.Rel(r.dir, p)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// rel is the client's path of the real path p inside the root.
func (r *root) rel(p string) string {
	rel, err := filepath.Rel(r.dir, p)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// entry is a file or directory in a listing.
type entry struct {
	Name string `json:"name"`
	// Path is the client's path of the entry, to pass back.
	Path string `json:"path"`
	// Type is "dir", or the trace's format (trace.Format: "sqlite",
	// "report").
	Type  string    `json:"type"`
	Size  int64     `json:"size,omitempty"`
	MTime time.Time `json:"mtime"`
	// Ready says whether the server can open the trace now, and Note, if
	// set, how or why not (as trace.OpenOptions.Probe says).
	Ready bool   `json:"ready,omitempty"`
	Note  string `json:"note,omitempty"`
}

// list returns the subdirectories and traces of the directory at client
// path p. Hidden entries, other files, and links that lead out of the root
// or nowhere are left out.
func (r *root) list(p string, opts trace.OpenOptions) (string, []entry, error) {
	real, err := r.resolve(p)
	if err != nil {
		return "", nil, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", nil, err
	}
	if !fi.IsDir() {
		return "", nil, errBadPath
	}
	des, err := os.ReadDir(real)
	if err != nil {
		return "", nil, err
	}
	dir := r.rel(real)
	var out []entry
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, ".") || strings.Contains(name, ".tmp-export.") {
			continue
		}
		target, err := filepath.EvalSymlinks(filepath.Join(real, name))
		if err != nil || !r.contains(target) {
			continue
		}
		info, err := os.Stat(target)
		if err != nil {
			continue
		}
		e := entry{Name: name, Path: path.Join(dir, name), MTime: info.ModTime().UTC()}
		if info.IsDir() {
			e.Type = "dir"
		} else {
			// Whether and how it opens is the trace package's to say.
			f, ready, note := opts.Probe(target)
			if f == "" {
				continue
			}
			e.Type, e.Size, e.Ready, e.Note = string(f), info.Size(), ready, note
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b entry) int {
		if (a.Type == "dir") != (b.Type == "dir") {
			if a.Type == "dir" {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return dir, out, nil
}
