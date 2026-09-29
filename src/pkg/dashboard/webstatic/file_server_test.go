package webstatic

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestFileServerRevalidation(t *testing.T) {
	for _, name := range []string{"design-system.html", "tokens.css", "app.js", "static/logo.svg", "docs/index.html"} {
		t.Run(name, func(t *testing.T) {
			files := fstest.MapFS{name: &fstest.MapFile{Data: []byte("old content")}}
			handler := FileServer(files)
			url := "/" + name
			if name == "docs/index.html" {
				url = "/docs/"
			}
			request := func(h http.Handler, method, validator string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, url, nil)
				r.Header.Set("If-None-Match", validator)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			first := request(handler, http.MethodGet, "")
			etag := first.Header().Get("ETag")
			if first.Code != http.StatusOK || first.Body.String() != "old content" || etag == "" || first.Header().Get("Cache-Control") != "no-cache" {
				t.Fatalf("initial response: status=%d headers=%v body=%q", first.Code, first.Header(), first.Body.String())
			}
			for _, validator := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
				cached := request(handler, http.MethodGet, validator)
				if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 || cached.Header().Get("ETag") != etag || cached.Header().Get("Cache-Control") != "no-cache" {
					t.Fatalf("revalidation %q: status=%d headers=%v body=%q", validator, cached.Code, cached.Header(), cached.Body.String())
				}
			}
			head := request(handler, http.MethodHead, "")
			if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("ETag") != etag {
				t.Fatalf("HEAD: status=%d headers=%v body=%q", head.Code, head.Header(), head.Body.String())
			}
			// A new binary constructs a new handler over its new embedded bytes.
			upgraded := FileServer(fstest.MapFS{name: &fstest.MapFile{Data: []byte("new content")}})
			fresh := request(upgraded, http.MethodGet, etag)
			if fresh.Code != http.StatusOK || fresh.Body.String() != "new content" || fresh.Header().Get("ETag") == etag || fresh.Header().Get("ETag") == "" {
				t.Fatalf("upgrade: status=%d headers=%v body=%q", fresh.Code, fresh.Header(), fresh.Body.String())
			}
		})
	}
}

func TestFileServerMissingFile(t *testing.T) {
	w := httptest.NewRecorder()
	FileServer(fstest.MapFS{}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/missing.css", nil))
	if w.Code != http.StatusNotFound || w.Header().Get("ETag") != "" {
		t.Fatalf("missing file: status=%d headers=%v", w.Code, w.Header())
	}
}
