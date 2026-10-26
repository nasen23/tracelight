// Command gentorch writes synthetic traces of PyTorch's profiler (Kineto's
// Chrome-trace JSON) for the ranks of a data-parallel training job, into
// a directory (internal/torchgen says what they hold):
//
//	go run ./cmd/gentorch -ranks 4 -o samples/torch
//	go run ./cmd/gentorch -ranks 4 -merged -gz -no-stack -o samples/torch-merged
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"tracelight/internal/torchgen"
)

func main() {
	out := flag.String("o", "samples/torch", "directory to write rank<N>.pt.trace.json into")
	ranks := flag.Int("ranks", 4, "ranks of the job")
	steps := flag.Int("steps", 8, "profiled training steps")
	layers := flag.Int("layers", 6, "transformer blocks")
	gz := flag.Bool("gz", false, "write .json.gz")
	noStack := flag.Bool("no-stack", false, "leave out the Python calls (as without with_stack)")
	merged := flag.Bool("merged", false, "write the ranks merged into one file, job.json (as tools that merge ranks' traces do)")
	flag.Parse()
	o := torchgen.Options{Ranks: *ranks, Steps: *steps, Layers: *layers, NoStack: *noStack}
	if *merged {
		p := filepath.Join(*out, "job.json")
		if *gz {
			p += ".gz"
		}
		if err := os.MkdirAll(*out, 0o755); err != nil {
			log.Fatal(err)
		}
		if err := torchgen.WriteMerged(p, o, *gz); err != nil {
			log.Fatal(err)
		}
		fmt.Println(p)
		return
	}
	paths, err := torchgen.WriteDir(*out, o, *gz)
	if err != nil {
		log.Fatal(err)
	}
	for _, p := range paths {
		fmt.Println(p)
	}
}
