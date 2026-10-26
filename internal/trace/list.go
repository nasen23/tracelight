package trace

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// NaturalCompare orders strings as people do: runs of digits by their
// value, so that rank2 comes before rank10.
func NaturalCompare(a, b string) int {
	for a != "" && b != "" {
		ca, cb := rune(a[0]), rune(b[0])
		if unicode.IsDigit(ca) && unicode.IsDigit(cb) {
			na, nb := digits(a), digits(b)
			va, vb := strings.TrimLeft(a[:na], "0"), strings.TrimLeft(b[:nb], "0")
			if len(va) != len(vb) {
				return len(va) - len(vb)
			}
			if c := strings.Compare(va, vb); c != 0 {
				return c
			}
			if na != nb {
				return na - nb // fewer leading zeros first
			}
			a, b = a[na:], b[nb:]
			continue
		}
		if ca != cb {
			return int(ca) - int(cb)
		}
		a, b = a[1:], b[1:]
	}
	return len(a) - len(b)
}

func digits(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

// SortTraces orders trace files naturally by name, then by path.
func SortTraces(files []string) {
	slices.SortFunc(files, func(a, b string) int {
		if c := NaturalCompare(filepath.Base(a), filepath.Base(b)); c != 0 {
			return c
		}
		return NaturalCompare(a, b)
	})
}

// DirTraces lists the traces in dir, not recursively, as a session opens
// them: what Open reads (hidden files and half-written exports left out),
// one of a report and its export, in natural order.
func (o OpenOptions) DirTraces(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.Contains(name, ".tmp-export.") {
			continue
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			continue
		}
		if Detect(p) != "" {
			files = append(files, p)
		}
	}
	files = o.Dedupe(files)
	SortTraces(files)
	return files, nil
}
