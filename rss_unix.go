//go:build unix

package main

import (
	"runtime"
	"syscall"
)

// peakRSS is the process's peak resident set size in bytes.
func peakRSS() int64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss) // bytes on macOS
	}
	return int64(ru.Maxrss) * 1024 // KiB on Linux and the BSDs
}
