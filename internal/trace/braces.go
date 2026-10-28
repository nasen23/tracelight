package trace

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Brace patterns, as a shell expands them, for picking traces: rank{0,3,5}
// names three files, rank{0..15} sixteen (rank{00..15}, zero-padded, as
// the shell does), and they nest and combine (job{1,2}/rank{0..3}). A
// path that exists as it is written (braces in a file's name) stays as it
// is. Globs (* ? [) still apply to what the braces make.

// maxBraceExpansion bounds what one pattern makes.
const maxBraceExpansion = 4096

// ExpandBraces returns the paths a brace pattern stands for, in its order;
// a path without braces, or that exists as written (exists says; nil asks
// the file system), is itself.
func ExpandBraces(path string, exists func(string) bool) ([]string, error) {
	if !strings.Contains(path, "{") {
		return []string{path}, nil
	}
	if exists == nil {
		exists = func(p string) bool { _, err := os.Stat(p); return err == nil }
	}
	if exists(path) {
		return []string{path}, nil
	}
	out, err := expandBraces(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

func expandBraces(s string) ([]string, error) {
	// The first brace that holds alternatives or a range: a "{" whose
	// matching "}" encloses a top-level "," or "..".
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		depth, j, comma := 0, i, false
		var parts []string
		last := i + 1
		for j = i; j < len(s); j++ {
			switch s[j] {
			case '{':
				depth++
			case '}':
				depth--
			case ',':
				if depth == 1 {
					comma = true
					parts = append(parts, s[last:j])
					last = j + 1
				}
			}
			if depth == 0 {
				break
			}
		}
		if depth != 0 {
			return nil, fmt.Errorf("a { without its }")
		}
		inner := s[i+1 : j]
		var alts []string
		switch {
		case comma:
			alts = append(parts, s[last:j])
		case strings.Contains(inner, ".."):
			r, ok := braceRange(inner)
			if !ok {
				continue // not a range: literal braces
			}
			if len(r) == 0 {
				return nil, fmt.Errorf("{%s} makes more than %d paths", inner, maxBraceExpansion)
			}
			alts = r
		default:
			continue // {x}: literal
		}
		pre, post := s[:i], s[j+1:]
		var out []string
		for _, a := range alts {
			rest, err := expandBraces(pre + a + post)
			if err != nil {
				return nil, err
			}
			out = append(out, rest...)
			if len(out) > maxBraceExpansion {
				return nil, fmt.Errorf("braces make more than %d paths", maxBraceExpansion)
			}
		}
		return out, nil
	}
	return []string{s}, nil
}

// braceRange reads a range "a..b" of integers (or of one letter each),
// zero-padded as its ends are: 00..15 is 00, 01, ..., 15.
func braceRange(s string) ([]string, bool) {
	a, b, ok := strings.Cut(s, "..")
	if !ok || strings.Contains(b, "..") {
		return nil, false
	}
	x, err1 := strconv.Atoi(a)
	y, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil {
		if len(a) == 1 && len(b) == 1 && isLetter(a[0]) && isLetter(b[0]) {
			var out []string
			for c := a[0]; ; {
				out = append(out, string(c))
				if c == b[0] {
					return out, true
				}
				if c < b[0] {
					c++
				} else {
					c--
				}
			}
		}
		return nil, false
	}
	if d := y - x; d > maxBraceExpansion || -d > maxBraceExpansion {
		return nil, true // too many: expandBraces says so
	}
	width := 0
	if (len(a) > 1 && strings.TrimLeft(a, "-")[0] == '0') || (len(b) > 1 && strings.TrimLeft(b, "-")[0] == '0') {
		width = max(len(a), len(b))
	}
	step := 1
	if y < x {
		step = -1
	}
	var out []string
	for n := x; ; n += step {
		out = append(out, fmt.Sprintf("%0*d", width, n))
		if n == y {
			return out, true
		}
	}
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
