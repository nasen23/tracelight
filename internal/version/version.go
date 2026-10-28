// Package version is Tracelight's version: the one place it is written.
// The command (--version, `tracelight version`), the server (/api/info, so
// the web UI shows it), the client's User-Agent and the desktop app's
// welcome screen read it; a test keeps mygo.json's (the packaged app's) the
// same.
package version

// Version is the app's version.
const Version = "0.6.5"
