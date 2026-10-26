//go:build !js

package trace

import (
	"errors"
	"fmt"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// conn is a read-only SQLite connection that talks to modernc's translation
// of the SQLite C library directly, without database/sql.
//
// database/sql costs a lot per row: the driver opens every connection
// SQLITE_OPEN_FULLMUTEX, so every column read takes and releases a mutex,
// and every value is boxed into an interface and converted again by Scan.
// Reading a million rows that way took about 2 s; this takes a fraction of
// it, and connections of their own let tables be read in parallel.
//
// A conn must be used by one goroutine at a time.
type conn struct {
	tls *libc.TLS
	db  uintptr
}

func openReadOnly(path string) (*conn, error) {
	tls := libc.NewTLS()
	cpath, err := libc.CString(path)
	if err != nil {
		tls.Close()
		return nil, err
	}
	defer libc.Xfree(tls, cpath)
	pdb := libc.Xmalloc(tls, 8)
	if pdb == 0 {
		tls.Close()
		return nil, errors.New("out of memory")
	}
	defer libc.Xfree(tls, pdb)
	*(*uintptr)(cmem(pdb)) = 0
	rc := sqlite3.Xsqlite3_open_v2(tls, cpath, pdb, sqlite3.SQLITE_OPEN_READONLY|sqlite3.SQLITE_OPEN_NOMUTEX, 0)
	c := &conn{tls: tls, db: *(*uintptr)(cmem(pdb))}
	if rc != sqlite3.SQLITE_OK {
		err := fmt.Errorf("open: %s", c.errmsg(rc))
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *conn) close() {
	if c.db != 0 {
		sqlite3.Xsqlite3_close_v2(c.tls, c.db)
		c.db = 0
	}
	c.tls.Close()
}

func (c *conn) errmsg(rc int32) string {
	if c.db != 0 {
		if msg := libc.GoString(sqlite3.Xsqlite3_errmsg(c.tls, c.db)); msg != "" {
			return msg
		}
	}
	return libc.GoString(sqlite3.Xsqlite3_errstr(c.tls, rc))
}

// stmt is a statement stepping through its rows.
type stmt struct {
	c *conn
	p uintptr
}

// query runs q with args bound to its ? parameters and calls fn for each
// row; fn's error stops it.
func (c *conn) query(q string, args []int64, fn func(*stmt) error) error {
	cq, err := libc.CString(q)
	if err != nil {
		return err
	}
	defer libc.Xfree(c.tls, cq)
	pp := libc.Xmalloc(c.tls, 8)
	if pp == 0 {
		return errors.New("out of memory")
	}
	defer libc.Xfree(c.tls, pp)
	*(*uintptr)(cmem(pp)) = 0
	if rc := sqlite3.Xsqlite3_prepare_v2(c.tls, c.db, cq, -1, pp, 0); rc != sqlite3.SQLITE_OK {
		return errors.New(c.errmsg(rc))
	}
	s := &stmt{c: c, p: *(*uintptr)(cmem(pp))}
	if s.p == 0 {
		return nil // q was empty
	}
	defer sqlite3.Xsqlite3_finalize(c.tls, s.p)
	for i, a := range args {
		if rc := sqlite3.Xsqlite3_bind_int64(c.tls, s.p, int32(i+1), a); rc != sqlite3.SQLITE_OK {
			return errors.New(c.errmsg(rc))
		}
	}
	for {
		switch rc := sqlite3.Xsqlite3_step(c.tls, s.p); rc {
		case sqlite3.SQLITE_ROW:
			if err := fn(s); err != nil {
				return err
			}
		case sqlite3.SQLITE_DONE:
			return nil
		default:
			return errors.New(c.errmsg(rc))
		}
	}
}

// int64 is column i as an integer, 0 for NULL.
func (s *stmt) int64(i int) int64 { return sqlite3.Xsqlite3_column_int64(s.c.tls, s.p, int32(i)) }

// null reports whether column i is NULL.
func (s *stmt) null(i int) bool {
	return sqlite3.Xsqlite3_column_type(s.c.tls, s.p, int32(i)) == sqlite3.SQLITE_NULL
}

// bytes is column i as text, in memory SQLite owns: valid only until the
// next row. Use it for lookups that need no copy.
func (s *stmt) bytes(i int) []byte {
	p := sqlite3.Xsqlite3_column_text(s.c.tls, s.p, int32(i))
	if p == 0 {
		return nil
	}
	n := sqlite3.Xsqlite3_column_bytes(s.c.tls, s.p, int32(i))
	return unsafe.Slice((*byte)(cmem(p)), n)
}

// text is column i as a Go string.
func (s *stmt) text(i int) string { return string(s.bytes(i)) }

// cmem turns an address in the C library's heap, which modernc's libc
// allocates outside Go's (so the garbage collector neither moves nor frees
// it), into a pointer.
func cmem(p uintptr) unsafe.Pointer { return unsafe.Add(unsafe.Pointer(nil), p) }
