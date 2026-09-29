package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticAssetCacheRevalidation(t *testing.T) {
	s := NewHubServer(0, slog.Default(), "test", "v5")
	for _, url := range []string{"/static/og-card.png", "/static/tokens.css", "/static/learn.html"} {
		t.Run(url, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
			etag := w.Header().Get("ETag")
			if w.Code != http.StatusOK || etag == "" || w.Header().Get("Cache-Control") != "no-cache" {
				t.Fatalf("initial response: status=%d headers=%v", w.Code, w.Header())
			}
			r := httptest.NewRequest(http.MethodGet, url, nil)
			r.Header.Set("If-None-Match", etag)
			w = httptest.NewRecorder()
			s.mux.ServeHTTP(w, r)
			if w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("Cache-Control") != "no-cache" {
				t.Fatalf("revalidation: status=%d headers=%v body length=%d", w.Code, w.Header(), w.Body.Len())
			}
		})
	}
}
