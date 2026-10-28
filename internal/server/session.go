package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"tracelight/internal/trace"
)

// A session is several traces shown on one time axis, as the reports of the
// ranks of a distributed job: /api/session expands the paths it is given
// (files, directories, globs) into trace files, and says when and where each
// was captured, without loading them; the client then loads each through
// /api/trace and its tiles, as it shows them.

// maxSessionTraces bounds what one session opens.
const maxSessionTraces = 256

type sessionTrace struct {
	Path string `json:"path"` // absolute, as the client passes it back
	Name string `json:"name"`
	// Format is the trace's format (trace.Format), Size its file's.
	Format string `json:"format"`
	Size   int64  `json:"size"`
	Ready  bool   `json:"ready"`
	Note   string `json:"note,omitempty"`
	// SessionStart is when the capture started, in ns since 1970, 0 if
	// unknown; Host the machine's name; Rank the rank the file name
	// says, -1 if none.
	SessionStart int64 `json:"sessionStart"`
	// SessionMono is the same on the host's monotonic clock (reports say
	// only this one), 0 if unknown.
	SessionMono int64 `json:"sessionMono"`
	// SessionStartNs is SessionStart in decimal: exact in browsers, where
	// numbers of 19 digits are not.
	SessionStartNs string `json:"sessionStartNs"`
	Host           string `json:"host,omitempty"`
	Rank           int    `json:"rank"`
	// Clock names the clock SessionMono counts on, for ranks merged into
	// one file (trace.Trace.Clock).
	Clock string `json:"clock,omitempty"`
}

func (s *Server) session(r *http.Request) (any, error) {
	inputs := r.URL.Query()["path"]
	if len(inputs) == 0 {
		return nil, badRequest("give at least one path")
	}
	files, warnings, err := s.root.expand(inputs, s.cfg.Open)
	if err != nil {
		return nil, err
	}
	// A file of several ranks merged is a trace a rank (trace.Members),
	// which reading it tells: how far that is, /api/progress says.
	var members []string
	for _, f := range files {
		members = append(members, s.members(f)...)
	}
	if len(members) > maxSessionTraces {
		return nil, badRequest("more than %d traces: open fewer at once", maxSessionTraces)
	}
	files = members
	out := make([]sessionTrace, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, f := range files {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			file, rank, member := trace.SplitMember(f)
			fi, _ := os.Stat(file)
			format, ready, note := s.cfg.Open.Probe(f)
			info := s.cfg.Open.ReadInfo(f)
			name := filepath.Base(f)
			if member {
				name = "rank" + strconv.Itoa(rank)
			}
			st := sessionTrace{Path: f, Name: name, Format: string(format), Ready: ready, Note: note, Clock: info.Clock,
				SessionStart: info.SessionStartUTC, SessionStartNs: strconv.FormatInt(info.SessionStartUTC, 10), SessionMono: info.SessionStartMono, Host: info.Host, Rank: rankOf(filepath.Base(f))}
			if format == trace.FormatTorch && info.Rank >= 0 {
				st.Rank = info.Rank // the trace's distributedInfo
			}
			if fi != nil {
				st.Size = fi.Size()
			}
			out[i] = st
		})
	}
	wg.Wait()
	if warnings == nil {
		warnings = []string{}
	}
	return map[string]any{"traces": out, "warnings": warnings}, nil
}

var rankRe = regexp.MustCompile(`(?i)rank[-_.]?(\d+)`)

// rankOf is the rank a file's name says, as rank3 or rank_03, or -1.
func rankOf(name string) int {
	if m := rankRe.FindStringSubmatch(name); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	return -1
}

func hasGlob(p string) bool { return strings.ContainsAny(p, "*?[") }

// expand turns the client's paths into the absolute paths of trace files
// inside the root, in natural order, as they are named (a link stays a
// trace of its own name, which resolves again on each request, and shares
// the load of the file it leads to): a file stands for itself, a directory for the
// traces in it (not in its subdirectories), and a glob for the traces it
// matches. Paths the client names that are outside the root, missing or no
// traces fail; what a glob or a directory holds outside the root is left
// out, with a warning.
func (r *root) expand(inputs []string, o trace.OpenOptions) ([]string, []string, error) {
	// Brace patterns first: rank{0,3,5}, rank{0..15} (trace.ExpandBraces).
	var expanded []string
	for _, in := range inputs {
		ps, err := trace.ExpandBraces(strings.TrimSpace(in), r.exists)
		if err != nil {
			return nil, nil, badRequest("%v", err)
		}
		expanded = append(expanded, ps...)
	}
	inputs = expanded
	var files, warnings []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}
	for _, in := range inputs {
		in = strings.TrimSpace(in)
		switch {
		case in == "":
			continue
		case hasGlob(in):
			matched, outside, err := r.glob(in, o)
			if err != nil {
				return nil, nil, err
			}
			if outside > 0 {
				warnings = append(warnings, fmt.Sprintf("%s: %d matches outside the server's root left out", in, outside))
			}
			if len(matched) == 0 {
				return nil, nil, httpErr{http.StatusNotFound, fmt.Sprintf("%s: no trace files match", in)}
			}
			for _, m := range matched {
				add(m)
			}
		default:
			// One rank of a merged file (as a session lists it).
			if file, rank, ok := trace.SplitMember(in); ok {
				real, err := r.resolve(file)
				if err != nil {
					return nil, nil, fmt.Errorf("%s: %w", in, err)
				}
				if trace.Detect(real) != trace.FormatTorch {
					return nil, nil, badRequest("%s: not a trace file tracelight reads", in)
				}
				add(trace.MemberPath(r.absolute(file), rank))
				continue
			}
			real, err := r.resolve(in)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", in, err)
			}
			listed := r.absolute(in)
			fi, err := os.Stat(real)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", in, err)
			}
			if !fi.IsDir() {
				if trace.Detect(real) == "" {
					return nil, nil, badRequest("%s: not a trace file tracelight reads", in)
				}
				add(listed)
				continue
			}
			inDir, skipped, err := r.traceFiles(listed, o)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", in, err)
			}
			if skipped > 0 {
				warnings = append(warnings, fmt.Sprintf("%s: %d links outside the server's root left out", in, skipped))
			}
			if len(inDir) == 0 {
				return nil, nil, httpErr{http.StatusNotFound, fmt.Sprintf("%s: no .nsys-rep or .sqlite trace files in this directory", in)}
			}
			for _, f := range inDir {
				add(f)
			}
		}
		if len(files) > maxSessionTraces {
			return nil, nil, badRequest("more than %d traces: open fewer at once", maxSessionTraces)
		}
	}
	if len(files) == 0 {
		return nil, nil, badRequest("give at least one path")
	}
	// Natural order of the names, then of the directories.
	slices.SortStableFunc(files, func(a, b string) int {
		if c := natCompare(filepath.Base(a), filepath.Base(b)); c != 0 {
			return c
		}
		return natCompare(a, b)
	})
	return files, warnings, nil
}

// exists reports whether the client's path p names something inside the
// root (a rank of a merged file: its file).
func (r *root) exists(p string) bool {
	file, _, _ := trace.SplitMember(p)
	real, err := r.resolve(file)
	if err != nil {
		return false
	}
	_, err = os.Stat(real)
	return err == nil
}

// glob returns the trace files a pattern matches inside the root, and how
// many matches were outside it.
func (r *root) glob(pattern string, o trace.OpenOptions) ([]string, int, error) {
	full := filepath.Clean(pattern)
	if !filepath.IsAbs(pattern) && !strings.HasPrefix(pattern, "/") {
		c, err := clean(pattern)
		if err != nil {
			return nil, 0, err
		}
		full = filepath.Join(r.dir, filepath.FromSlash(c))
	}
	matches, err := filepath.Glob(full)
	if err != nil {
		return nil, 0, badRequest("%s: %v", pattern, err)
	}
	var out []string
	outside := 0
	for _, m := range matches {
		real, err := r.resolve(m)
		if errors.Is(err, errOutside) {
			outside++
			continue
		}
		if err != nil || trace.Detect(real) == "" || isDir(real) {
			continue
		}
		out = append(out, m)
	}
	return o.Dedupe(out), outside, nil
}

// traceFiles lists the trace files of a directory, and how many links in
// it lead out of the root.
func (r *root) traceFiles(dir string, o trace.OpenOptions) ([]string, int, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	var out []string
	skipped := 0
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, ".") || strings.Contains(name, ".tmp-export.") {
			continue
		}
		real, err := filepath.EvalSymlinks(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if !r.contains(real) {
			skipped++
			continue
		}
		if isDir(real) || trace.Detect(real) == "" { // trace decides what it reads
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return o.Dedupe(out), skipped, nil
}

// absolute is the client's path p made absolute: as given, or under the
// root. (resolve has checked it.)
func (r *root) absolute(p string) string {
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	c, _ := clean(p)
	return filepath.Join(r.dir, filepath.FromSlash(c))
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// natCompare orders strings as people do (trace.NaturalCompare).
func natCompare(a, b string) int { return trace.NaturalCompare(a, b) }

// members lists the traces of the file f (trace.Members), its read's
// progress kept in s.listing meanwhile, for /api/progress.
func (s *Server) members(f string) []string {
	real, err := s.root.resolve(f)
	if err != nil || trace.Detect(real) != trace.FormatTorch {
		return trace.Members(f)
	}
	p := &trace.Progress{}
	s.listing.Store(real, p)
	defer s.listing.CompareAndDelete(real, p)
	return trace.MembersProgress(f, p)
}

// progressJSON is how far a trace's load has got.
type progressJSON struct {
	Phase    string  `json:"phase"`
	Fraction float64 `json:"fraction"`
	Error    string  `json:"error,omitempty"`
}

// progress says how far the loads of the traces named have got, in their
// order (path repeated: as /api/session lists them, or a merged file being
// listed): "queued" for one not asked for yet, "done" once loaded. Clients
// ask it every few hundred milliseconds while traces load.
func (s *Server) progress(r *http.Request) (any, error) {
	paths := r.URL.Query()["path"]
	if len(paths) > maxSessionTraces {
		return nil, badRequest("at most %d paths", maxSessionTraces)
	}
	out := make([]progressJSON, len(paths))
	for i, p := range paths {
		file, rank, member := trace.SplitMember(p)
		real, err := s.root.resolve(file)
		if err != nil {
			out[i] = progressJSON{Phase: trace.PhaseFailed.String(), Error: err.Error()}
			continue
		}
		key := real
		if member {
			key = trace.MemberPath(real, rank)
		}
		pr := s.cache.progress(key)
		if pr == nil {
			if v, ok := s.listing.Load(real); ok { // its file is being listed (read)
				pr = v.(*trace.Progress)
			}
		}
		if pr == nil {
			out[i] = progressJSON{Phase: trace.PhaseQueued.String()}
			continue
		}
		out[i] = progressJSON{Phase: pr.Phase().String(), Fraction: pr.Fraction()}
		if err := pr.Err(); err != nil {
			out[i].Error = err.Error()
		}
	}
	return map[string]any{"files": out}, nil
}
