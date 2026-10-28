package trace

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestExpandBraces(t *testing.T) {
	for in, want := range map[string][]string{
		"/runs/rank0.sqlite":            {"/runs/rank0.sqlite"},
		"/runs/rank{0,3,5}.sqlite":      {"/runs/rank0.sqlite", "/runs/rank3.sqlite", "/runs/rank5.sqlite"},
		"/r/rank{0..3}.nsys-rep":        {"/r/rank0.nsys-rep", "/r/rank1.nsys-rep", "/r/rank2.nsys-rep", "/r/rank3.nsys-rep"},
		"/r/rank{08..11}":               {"/r/rank08", "/r/rank09", "/r/rank10", "/r/rank11"},
		"/r/rank{3..1}":                 {"/r/rank3", "/r/rank2", "/r/rank1"},
		"/r/job{1,2}/rank{0..1}.sqlite": {"/r/job1/rank0.sqlite", "/r/job1/rank1.sqlite", "/r/job2/rank0.sqlite", "/r/job2/rank1.sqlite"},
		"/r/{a,b{1,2}}.json":            {"/r/a.json", "/r/b1.json", "/r/b2.json"},
		"/r/{x}.json":                   {"/r/{x}.json"}, // no alternatives: literal
		"/r/{a..c}":                     {"/r/a", "/r/b", "/r/c"},
		"/r/rank{0..1}/*.sqlite":        {"/r/rank0/*.sqlite", "/r/rank1/*.sqlite"},
		"/r/job.json.gz#rank{0,8}":      {"/r/job.json.gz#rank0", "/r/job.json.gz#rank8"},
		"/r/{,pre}fix":                  {"/r/fix", "/r/prefix"},
		"/r/{1..2}{a,b}":                {"/r/1a", "/r/1b", "/r/2a", "/r/2b"},
		"/r/{-1..1}":                    {"/r/-1", "/r/0", "/r/1"},
	} {
		got, err := ExpandBraces(in, nil)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("ExpandBraces(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/r/rank{0,1", "/r/{0..100000}", "/r/{0..99}{0..99}"} {
		if got, err := ExpandBraces(bad, nil); err == nil {
			t.Errorf("ExpandBraces(%q) = %d paths, no error", bad, len(got))
		}
	}
	// A file whose name has braces stays as it is.
	p := filepath.Join(t.TempDir(), "run{a,b}.sqlite")
	os.WriteFile(p, nil, 0o644)
	if got, _ := ExpandBraces(p, nil); !slices.Equal(got, []string{p}) {
		t.Errorf("an existing path with braces: %q", got)
	}
}
