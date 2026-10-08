package main

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

// staticgz.go serves the embedded /static/ files gzipped once instead of on
// every request. They never change while px0 runs, and every PR review is a
// fresh px0 on its own port, so the browser's cache never helps: compressing
// app.js anew cost about 100ms on each window's first load.

type staticGz struct {
	body  []byte
	ctype string
}

var (
	staticGzMu    sync.Mutex
	staticGzCache = map[string]*staticGz{} // file under web/ -> its gzip; nil if it cannot be served this way
)

// staticGzFile compresses an embedded file the first time it is asked for.
func staticGzFile(name string) *staticGz {
	staticGzMu.Lock()
	defer staticGzMu.Unlock()
	if e, ok := staticGzCache[name]; ok {
		return e
	}
	var e *staticGz
	if raw, err := fs.ReadFile(assets, "web/"+name); err == nil {
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
		zw.Write(raw)
		zw.Close()
		ctype := mime.TypeByExtension(path.Ext(name))
		if ctype == "" {
			ctype = http.DetectContentType(raw)
		}
		e = &staticGz{body: b.Bytes(), ctype: ctype}
	}
	staticGzCache[name] = e
	return e
}

// warmStaticGz compresses the large files the page loads first, before the
// browser asks for them.
func warmStaticGz() {
	for _, name := range []string{"app.js", "style.css"} {
		staticGzFile(name)
	}
}

// servePrecompressed answers a GET or HEAD for an embedded /static/ file
// with its cached gzip, and reports whether it did. themes.css is generated
// per request and is left to its handler, as is anything not embedded.
func (s *Server) servePrecompressed(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	prefix := s.routePath("/static/")
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	name := strings.TrimPrefix(r.URL.Path, prefix)
	if name == "" || name == "themes.css" || strings.Contains(name, "..") {
		return false
	}
	e := staticGzFile(name)
	if e == nil {
		return false
	}
	h := w.Header()
	h.Set("Content-Type", e.ctype)
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Set("Content-Length", strconv.Itoa(len(e.body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		w.Write(e.body)
	}
	return true
}
