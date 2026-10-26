package trace

import (
	"bytes"
	"compress/gzip"
	"path/filepath"
	"reflect"
	"testing"
)

func checkSnapshot(t *testing.T, tr *Trace) int {
	t.Helper()
	var buf bytes.Buffer
	if err := tr.WriteSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	size := buf.Len()
	got, err := ReadSnapshot(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Start != tr.Start || got.End != tr.End || !reflect.DeepEqual(got.Names, tr.Names) ||
		!reflect.DeepEqual(got.Counts, tr.Counts) || len(got.Kernels) != len(tr.Kernels) || len(got.Tracks) != len(tr.Tracks) {
		t.Fatal("snapshot metadata differs")
	}
	for i, tk := range tr.Tracks {
		g := got.Tracks[i]
		if g.Name != tk.Name || g.Group != tk.Group || g.Device != tk.Device || len(g.Lanes) != len(tk.Lanes) {
			t.Fatalf("track %d differs", i)
		}
		for li := range tk.Lanes {
			if !reflect.DeepEqual(g.Lanes[li].Events, tk.Lanes[li].Events) {
				t.Fatalf("track %d lane %d: events differ", i, li)
			}
			if g.Lanes[li].BusyBetween(0, g.Lanes[li].Len()) != tk.Lanes[li].BusyBetween(0, tk.Lanes[li].Len()) {
				t.Fatalf("track %d lane %d: prefix sums differ", i, li)
			}
		}
	}
	// Correlation works on the copy, from its own index.
	for ti, tk := range got.Tracks {
		if tk.Kind == TrackStream && tk.Lanes[0].Len() > 0 {
			r := Ref{Track: int32(ti)}
			_, ok1 := got.Correlated(r)
			_, ok2 := tr.Correlated(r)
			if ok1 != ok2 {
				t.Fatal("correlation differs on the snapshot")
			}
			break
		}
	}
	return size
}

func TestSnapshotRoundTrip(t *testing.T) {
	path := filepath.Join("..", "..", "samples", "sample.sqlite")
	if !exists(path) {
		t.Skip("run go run ./cmd/gensample first")
	}
	tr, err := LoadSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	checkSnapshot(t, tr)
}

func TestSnapshotRealTrace(t *testing.T) {
	tr := loadReal(t)
	size := checkSnapshot(t, tr)
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestSpeed)
	tr.WriteSnapshot(zw)
	zw.Close()
	t.Logf("snapshot of %d events: %.1f MB, %.1f MB gzipped", tr.EventCount(), float64(size)/1e6, float64(gz.Len())/1e6)
}

func TestSnapshotRejectsGarbage(t *testing.T) {
	if _, err := ReadSnapshot(bytes.NewReader([]byte("hello"))); err == nil {
		t.Error("garbage read as a snapshot")
	}
	var buf bytes.Buffer
	b := NewBuilder("x")
	b.AddKernel(Event{Start: 1, End: 2, Name: b.Intern("k")}, KernelInfo{})
	b.Finish().WriteSnapshot(&buf)
	data := buf.Bytes()
	if _, err := ReadSnapshot(bytes.NewReader(data[:len(data)-3])); err == nil {
		t.Error("a truncated snapshot read without error")
	}
}
