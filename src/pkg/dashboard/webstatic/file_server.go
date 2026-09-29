package webstatic

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
)

// FileServer serves immutable embedded files with content validators. Their URLs
// are not versioned, so browsers must revalidate them after a binary upgrade.
// Do not use it for mutable files: validators are cached for the handler's life.
func FileServer(files fs.FS) http.Handler {
	server := http.FileServerFS(files)
	var etags sync.Map
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "."
		}
		if info, err := fs.Stat(files, name); err == nil && info.IsDir() {
			name = path.Join(name, "index.html")
		}
		if etag, ok := etags.Load(name); ok {
			w.Header().Set("ETag", etag.(string))
		} else if data, err := fs.ReadFile(files, name); err == nil {
			etag := fmt.Sprintf(`"%x"`, sha256.Sum256(data))
			etags.Store(name, etag)
			w.Header().Set("ETag", etag)
		}
		// Let net/http handle conditional requests, HEAD, ranges, redirects and
		// errors, using the validator set above even though embed.FS has no mtime.
		server.ServeHTTP(w, r)
	})
}
