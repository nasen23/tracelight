package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"tracelight/internal/server"
	"tracelight/internal/trace"
)

// serveMain is `tracelight serve`: serve the traces under a directory to
// browsers and desktop clients.
func serveMain(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	root := fs.String("root", ".", "directory whose traces (.sqlite, .nsys-rep) are served; nothing outside it is read")
	addr := fs.String("addr", "127.0.0.1:8737", "address to listen on; use :PORT to accept other machines")
	token := fs.String("token", trace.Getenv("TOKEN"), "require this bearer token for the API ($TRACELIGHT_TOKEN); \"auto\" makes a random one")
	nsys := fs.String("nsys", "", "nsys executable for converting .nsys-rep reports (default: $NSYS, then nsys on the PATH)")
	cache := fs.String("cache", "", "directory for converted reports that cannot be written next to them (default: the user cache directory)")
	maxTraces := fs.Int("max-traces", 16, "traces kept loaded in memory (a session of ranks loads one per rank)")
	quiet := fs.Bool("quiet", false, "do not log requests")
	ui := fs.String("ui", "js", "web client at /: js (the JavaScript one) or wasm (the desktop app's views, in WebAssembly; always at /app.html)")
	latency := fs.String("latency", "", "for testing: delay every request by this round trip, a duration or a range lo-hi drawn from uniformly (30ms-80ms), as a network would")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: tracelight serve [flags]\n\nServes a web UI and an API for the traces under -root.\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if *ui != "js" && *ui != "wasm" {
		fmt.Fprintf(os.Stderr, "tracelight serve: --ui is js or wasm, not %q\n", *ui)
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fs.Usage()
		os.Exit(2)
	}
	if *token == "auto" {
		b := make([]byte, 18)
		rand.Read(b)
		*token = hex.EncodeToString(b)
	}
	cfg := server.Config{
		Root:      *root,
		Token:     *token,
		Open:      trace.OpenOptions{Nsys: *nsys, CacheDir: *cache},
		MaxTraces: *maxTraces,
		UI:        *ui,
	}
	if !*quiet {
		cfg.Log = log.New(os.Stderr, "", log.LstdFlags)
	}
	s, err := server.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	shown := host
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		shown, _ = os.Hostname()
	}
	url := "http://" + net.JoinHostPort(shown, port) + "/"
	fmt.Fprintf(os.Stderr, "tracelight: serving the traces under %s\n", s.Root())
	if *token != "" {
		fmt.Fprintf(os.Stderr, "tracelight: open %s#token=%s\n", url, *token)
	} else {
		fmt.Fprintf(os.Stderr, "tracelight: open %s\n", url)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			fmt.Fprintf(os.Stderr, "tracelight: warning: no --token, and listening beyond this machine: anyone who can reach %s can read these traces\n", ln.Addr())
		}
	}
	for _, n := range cfg.Open.Notes() {
		fmt.Fprintf(os.Stderr, "tracelight: %s\n", n)
	}

	var handler http.Handler = s
	if *latency != "" {
		lo, hi, err := parseRTT(*latency)
		if err != nil {
			log.Fatal(err)
		}
		handler = delayed(s, lo, hi)
		fmt.Fprintf(os.Stderr, "tracelight: delaying every request by %v to %v (--latency)\n", lo, hi)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		close(done)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-done
}

// delayed delays every request by a round trip drawn from [lo, hi], as a
// network would (serve --latency, bench -remote).
func delayed(h http.Handler, lo, hi time.Duration) http.Handler {
	var mu sync.Mutex
	rng := mrand.New(mrand.NewPCG(1, 2))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := lo
		if hi > lo {
			mu.Lock()
			d += time.Duration(rng.Int64N(int64(hi - lo)))
			mu.Unlock()
		}
		time.Sleep(d)
		h.ServeHTTP(w, r)
	})
}
