//go:build js

package trace

import "errors"

// In a browser (the web client, js/wasm) traces come from the server:
// there is no SQLite to read them with.

var errNoSQLite = errors.New("traces cannot be opened in the browser: they are read on the server")

// LoadSQLite cannot read a file in the browser.
func LoadSQLite(path string) (*Trace, error) { return nil, errNoSQLite }

// ReadInfo knows nothing of a file in the browser.
func (o OpenOptions) ReadInfo(path string) Info { return Info{} }
