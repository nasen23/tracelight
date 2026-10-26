//go:build !js

package remote

import "net/http"

const inBrowser = false

func browserRequest(*http.Request) {}
