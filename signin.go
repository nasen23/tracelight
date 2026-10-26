package main

import (
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
)

// signIn signs in to a gateway in front of a server (one that sends the
// API's requests to a browser sign-in page): it opens the server's address
// in a window of the system web view, where the user signs in as in a
// browser, and polls the window's cookies for the server's host; once they
// get through to the API (check), it closes the window and hands them to
// done, as a Cookie header. Closing the window gives up. The cookies are
// never logged.
func signIn(base string, check func(cookie string) error, done func(cookie string, err error)) {
	host := base
	if u, err := url.Parse(base); err == nil {
		host = u.Host
	}
	win := mygo.NewWindow(mygo.WindowOptions{
		Title:  "Sign in to " + host + " — Tracelight",
		URL:    base,
		Width:  980,
		Height: 760,
	})
	stop := make(chan struct{})
	var once sync.Once
	finish := func(cookie string, err error) {
		once.Do(func() {
			close(stop)
			done(cookie, err)
		})
	}
	win.OnClosed(func() { finish("", errors.New("the sign-in window was closed before signing in")) })
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		tried := ""
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			cookies, err := mygo.Cookies.List(mygo.CookieFilter{URL: base + "api/info"})
			if err != nil || len(cookies) == 0 {
				continue
			}
			parts := make([]string, len(cookies))
			for i, c := range cookies {
				parts[i] = c.Name + "=" + c.Value
			}
			header := strings.Join(parts, "; ")
			if header == tried {
				continue // the same cookies: not signed in yet
			}
			if check(header) == nil {
				finish(header, nil)
				win.Close()
				return
			}
			tried = header
		}
	}()
}
