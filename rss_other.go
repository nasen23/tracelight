//go:build !unix

package main

// peakRSS is not measured on this platform.
func peakRSS() int64 { return 0 }
