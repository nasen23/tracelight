package trace

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"tracelight/internal/nsysrep"
)

// OpenOptions say how Open converts .nsys-rep reports.
type OpenOptions struct {
	// Nsys is the nsys executable; empty uses $NSYS, then nsys on the PATH.
	Nsys string
	// CacheDir holds converted reports that cannot go next to their report;
	// empty is the user cache directory's tracelight folder.
	CacheDir string
}

// Format is a kind of file Open reads.
type Format string

const (
	// FormatSQLite is an export of `nsys export --type sqlite`.
	FormatSQLite Format = "sqlite"
	// FormatReport is an Nsight Systems report (.nsys-rep, .qdrep).
	FormatReport Format = "report"
)

// IsTorchName reports whether path is named as PyTorch's profiler names
// its traces (.json, .json.gz): Detect then reads its start to make sure.
func IsTorchName(path string) bool {
	l := strings.ToLower(path)
	return strings.HasSuffix(l, ".json") || strings.HasSuffix(l, ".json.gz")
}

// sqliteMagic starts every SQLite database file.
const sqliteMagic = "SQLite format 3\x00"

// IsReport reports whether path names an Nsight Systems report rather than
// a SQLite export.
func IsReport(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".nsys-rep" || ext == ".qdrep"
}

// Detect returns the format of the file at path, by its extension and then
// by its first bytes, or "" if Open does not read it. It is the one place
// that decides how a file is read: OpenWith and Probe both go by it.
func Detect(path string) Format {
	if file, _, ok := SplitMember(path); ok {
		if Detect(file) == FormatTorch {
			return FormatTorch // a rank of a merged trace
		}
		return ""
	}
	if IsReport(path) {
		return FormatReport
	}
	f, err := os.Open(path)
	if err != nil {
		if strings.EqualFold(filepath.Ext(path), ".sqlite") {
			return FormatSQLite // let Open report why it cannot be read
		}
		return ""
	}
	defer f.Close()
	head := make([]byte, len(sqliteMagic))
	if _, err := io.ReadFull(f, head); err == nil && string(head) == sqliteMagic {
		return FormatSQLite
	}
	if strings.EqualFold(filepath.Ext(path), ".sqlite") {
		return FormatSQLite // named so: Open says it is not a database
	}
	if IsTorchName(path) {
		f.Seek(0, io.SeekStart)
		if isTorch(f) {
			return FormatTorch
		}
	}
	return ""
}

// Probe says whether Open reads the file at path, as what format, and
// whether it can open it now with these options; when it cannot, note says
// why. A server lists files with it, without knowing about formats itself.
func (o OpenOptions) Probe(path string) (f Format, ready bool, note string) {
	switch f = Detect(path); f {
	case FormatSQLite:
		return f, true, ""
	case FormatTorch:
		return f, true, "PyTorch profiler"
	case FormatReport:
		if nativeReady(path) {
			return f, true, "read natively"
		}
		if _, ok := o.Converted(path); ok {
			return f, true, "converted already"
		}
		if _, err := o.FindNsys(); err != nil {
			return f, false, "needs nsys to convert"
		}
		return f, true, "converted on open"
	}
	return "", false, ""
}

// nativeReady reports whether the native decoder would read the report at
// path (its version is one it knows, or TRACELIGHT_NATIVE allows it), from
// its header alone.
func nativeReady(path string) bool {
	mode := Getenv("NATIVE")
	if mode == "off" {
		return false
	}
	return nsysrep.Check(path, nsysrep.Options{AnyVersion: mode == "force"}) == nil
}

// Notes describes what these options cannot open, for a server to say as
// it starts.
func (o OpenOptions) Notes() []string {
	if _, err := o.FindNsys(); err != nil {
		return []string{"nsys not found (set --nsys or NSYS): .nsys-rep reports of supported versions open natively; others only if converted already"}
	}
	return nil
}

// Open loads a trace from an nsys SQLite export, or from an .nsys-rep
// report: natively (see LoadReport), or where the native decoder cannot
// read it, by converting it with nsys (see ExportReport).
func Open(path string) (*Trace, error) { return OpenWith(path, OpenOptions{}) }

// OpenWith is Open with options. It is the single entry point that turns a
// file into a trace: every reader of the program (the window, the server,
// bench) goes through it, and new formats plug in here.
func OpenWith(path string, o OpenOptions) (*Trace, error) {
	switch Detect(path) {
	case FormatReport:
		// The native decoder first; nsys export (or an export made earlier)
		// where it cannot read the report.
		t, err := loadReport(path, o.ExportReport)
		if t != nil && t.SessionStartUTC == 0 {
			// A report without the reading that gives its wall-clock
			// start (nsysrep.Session): take that from an export, if any.
			info := o.ReadInfo(path)
			t.SessionStartUTC = info.SessionStartUTC
			t.SessionStartMono = cmp.Or(t.SessionStartMono, info.SessionStartMono)
			t.Host = cmp.Or(t.Host, info.Host)
		}
		return t, err
	case FormatSQLite:
		return LoadSQLite(path)
	case FormatTorch:
		return LoadTorch(path)
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%s: not a trace tracelight reads (a SQLite export of nsys, or an .nsys-rep report)", filepath.Base(path))
}

// ExportReport converts an .nsys-rep report into a SQLite file with the
// nsys CLI, with the default options.
func ExportReport(report string) (string, error) { return OpenOptions{}.ExportReport(report) }

// FindNsys returns the nsys executable: o.Nsys, $NSYS, or nsys on the PATH.
func (o OpenOptions) FindNsys() (string, error) {
	for _, p := range []string{o.Nsys, os.Getenv("NSYS")} {
		if p != "" {
			return exec.LookPath(p)
		}
	}
	return exec.LookPath("nsys")
}

// ExportReport converts an .nsys-rep report into a SQLite file with nsys
// (`nsys export --type sqlite`) and returns its path. The file goes next to
// the report, or into the cache directory where that is not writable, and
// is reused while it is at least as new as the report.
func (o OpenOptions) ExportReport(report string) (string, error) {
	if _, err := os.Stat(report); err != nil {
		return "", err
	}
	if c, ok := o.Converted(report); ok {
		return c, nil
	}
	candidates := o.exportPaths(report)
	nsys, err := o.FindNsys()
	if err != nil {
		return "", fmt.Errorf("%s is an Nsight Systems report: install Nsight Systems so that `nsys` is on the PATH (or set NSYS to it), or convert it with `nsys export --type sqlite %s` and open the .sqlite file", filepath.Base(report), filepath.Base(report))
	}
	var errs []error
	for _, out := range candidates {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		// Write to a temporary name (ending in .sqlite, which nsys would
		// otherwise append), so that a failed or concurrent export never
		// leaves a half-written file that looks newer than the report.
		tmp := strings.TrimSuffix(out, ".sqlite") + ".tmp-export.sqlite"
		cmd := exec.Command(nsys, "export", "--type", "sqlite", "--force-overwrite", "true", "--output", tmp, report)
		msg, err := cmd.CombinedOutput()
		if err == nil {
			err = os.Rename(tmp, out)
		}
		if err == nil {
			return out, nil
		}
		os.Remove(tmp)
		errs = append(errs, fmt.Errorf("nsys export: %v: %s", err, strings.TrimSpace(string(msg))))
	}
	return "", errors.Join(errs...)
}

// exportPaths are where the export of report may go: next to it, then the
// cache directory.
func (o OpenOptions) exportPaths(report string) []string {
	paths := []string{strings.TrimSuffix(report, filepath.Ext(report)) + ".sqlite"}
	if c := o.cachedPath(report); c != "" {
		paths = append(paths, c)
	}
	return paths
}

// Converted returns the export of report made earlier, if there is one at
// least as new as the report: opening it then needs no nsys.
func (o OpenOptions) Converted(report string) (string, bool) {
	info, err := os.Stat(report)
	if err != nil {
		return "", false
	}
	for _, c := range o.exportPaths(report) {
		// At least as new: file times tick in milliseconds on Linux, so an
		// export made right after its report can have the same time.
		if s, err := os.Stat(c); err == nil && !s.ModTime().Before(info.ModTime()) {
			return c, true
		}
	}
	return "", false
}

// cachedPath is where the cache directory keeps the export of report.
func (o OpenOptions) cachedPath(report string) string {
	dir := o.CacheDir
	if dir == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(d, "tracelight")
	}
	abs, err := filepath.Abs(report)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(abs))
	base := strings.TrimSuffix(filepath.Base(report), filepath.Ext(report))
	return filepath.Join(dir, base+"-"+hex.EncodeToString(sum[:6])+".sqlite")
}

// Dedupe keeps one of each report and its export found together (X.nsys-rep
// and X.sqlite): the report where the native decoder reads it, the export
// otherwise. Other files stay, in order.
func (o OpenOptions) Dedupe(files []string) []string {
	byStem := map[string][]string{}
	var order []string
	for _, f := range files {
		stem := strings.TrimSuffix(f, filepath.Ext(f))
		if _, ok := byStem[stem]; !ok {
			order = append(order, stem)
		}
		byStem[stem] = append(byStem[stem], f)
	}
	var out []string
	for _, stem := range order {
		group := byStem[stem]
		var report, export string
		for _, f := range group {
			switch Detect(f) {
			case FormatReport:
				report = f
			case FormatSQLite:
				export = f
			}
		}
		switch {
		case len(group) != 2 || report == "" || export == "":
			out = append(out, group...)
		case nativeReady(report):
			out = append(out, report)
		default:
			out = append(out, export)
		}
	}
	return out
}
