package trace

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tracelight/internal/torchgen"
)

// watch samples p's phase and fraction while open runs, and returns what it
// saw: the phases in the order they came, and whether the fraction ever
// went back.
func watch(p *Progress, open func()) (phases []Phase, backwards bool) {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		last := -1.0
		for {
			ph, f := p.Phase(), p.Fraction()
			if len(phases) == 0 || phases[len(phases)-1] != ph {
				phases = append(phases, ph)
			}
			if f < last {
				backwards = true
			}
			last = f
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Microsecond):
			}
		}
	})
	open()
	close(stop)
	wg.Wait()
	if ph := p.Phase(); phases[len(phases)-1] != ph {
		phases = append(phases, ph)
	}
	return phases, backwards
}

func TestProgress(t *testing.T) {
	// A SQLite export: reading counts rows, by chunks.
	db := makeDB(t,
		`CREATE TABLE StringIds (id INTEGER PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO StringIds VALUES (1,'k')`,
		`CREATE TABLE CUPTI_ACTIVITY_KIND_KERNEL (start INTEGER, end INTEGER, deviceId INTEGER, streamId INTEGER, correlationId INTEGER, shortName INTEGER)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 200000)
			INSERT INTO CUPTI_ACTIVITY_KIND_KERNEL SELECT i*10, i*10+5, 0, 7, i, 1 FROM n`)
	var p Progress
	phases, back := watch(&p, func() {
		if _, err := OpenWith(db, OpenOptions{Progress: &p}); err != nil {
			t.Fatal(err)
		}
	})
	if p.Phase() != PhaseDone || p.Fraction() != 1 || back {
		t.Errorf("SQLite: phase %v, fraction %v, went backwards %v; phases seen %v", p.Phase(), p.Fraction(), back, phases)
	}
	if p.total.Load() < 200000 || p.done.Load() != p.total.Load() {
		t.Errorf("SQLite: %d of %d rows counted", p.done.Load(), p.total.Load())
	}

	// A PyTorch trace of several ranks merged: its bytes read, and the
	// ranks, opened after Members read it, follow that one read.
	dir := t.TempDir()
	merged := filepath.Join(dir, "job.json")
	if err := torchgen.WriteMerged(merged, torchgen.Options{Ranks: 3, Steps: 4, Layers: 2}, false); err != nil {
		t.Fatal(err)
	}
	var file Progress
	members := MembersProgress(merged, &file)
	read := file.source() // the file's one read, which file follows
	if len(members) != 3 || file.Phase() != PhaseDone || file.Fraction() != 1 || read.total.Load() == 0 || read.done.Load() != read.total.Load() {
		t.Fatalf("merged file: %d members, phase %v, %d of %d bytes", len(members), file.Phase(), read.done.Load(), read.total.Load())
	}
	for _, m := range members {
		var mp Progress
		if _, err := OpenWith(m, OpenOptions{Progress: &mp}); err != nil || mp.Phase() != PhaseDone || mp.Fraction() != 1 {
			t.Errorf("%s: phase %v fraction %v, err %v", m, mp.Phase(), mp.Fraction(), err)
		}
	}

	// A failure says why.
	var bad Progress
	if _, err := OpenWith(filepath.Join(dir, "missing.sqlite"), OpenOptions{Progress: &bad}); err == nil || bad.Phase() != PhaseFailed || bad.Err() == nil {
		t.Errorf("a missing file: phase %v, err %v", bad.Phase(), bad.Err())
	}

	// A nil progress takes reports and drops them.
	var np *Progress
	np.Report(PhaseReading, 1, 2)
	np.Add(1)
	np.Finish(nil)
	if np.Phase() != PhaseQueued || np.Fraction() != 0 {
		t.Error("a nil progress")
	}
	for ph := PhaseQueued; ph <= PhaseFailed; ph++ {
		if ParsePhase(ph.String()) != ph {
			t.Errorf("ParsePhase(%q)", ph.String())
		}
	}
}
