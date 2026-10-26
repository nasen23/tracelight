## Downloads

| | |
|---|---|
| **macOS** (Apple silicon) | `Tracelight-{{VERSION}}-macos-arm64.dmg`, or the same app zipped: `Tracelight-{{VERSION}}-macos-arm64.app.zip` |
| **Linux** (x86-64, arm64) | `tracelight-{{VERSION}}-linux-<arch>.tar.gz` with `install.sh`, or `tracelight_{{VERSION}}_<arch>.deb` |
| **traceshot** (CLI) | `traceshot-{{VERSION}}-<os>-<arch>.tar.gz` for linux-amd64, linux-arm64, darwin-arm64, darwin-amd64: a static binary |

`SHA256SUMS` has the checksums of every file: `sha256sum -c --ignore-missing SHA256SUMS`
(on macOS, `shasum -a 256 -c --ignore-missing SHA256SUMS`).

**Linux.** The tarball holds the `tracelight` binary, which runs as it is
(`./tracelight trace.nsys-rep`, or `./tracelight serve`). To install it for
your user in `~/.local`, with a menu entry, put `install.sh` next to the
tarball and run `sh install.sh`. The window needs `libgtk-3-0` and
`libepoxy0`.

**macOS.** The app is not signed with a Developer ID nor notarized, so
Gatekeeper blocks it the first time ("cannot be opened" or "is damaged").
After dragging it to Applications, either open System Settings → Privacy &
Security and click *Open Anyway*, or run:

```sh
xattr -dr com.apple.quarantine /Applications/Tracelight.app
```

**traceshot.** `tar xzf traceshot-{{VERSION}}-linux-amd64.tar.gz && ./traceshot -h`.
On macOS, a binary downloaded with a browser also needs
`xattr -d com.apple.quarantine traceshot` (downloads with `curl` don't).
