package ingress

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDispatchExactPathsAndBodyLimit(t *testing.T) {
	var got string
	h := New(map[string]http.Handler{"/hooks/a": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		got = string(body)
	})})
	do := func(path, body string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
		return rec.Code
	}
	if code := do("/hooks/a", "event"); code != 200 || got != "event" {
		t.Fatalf("not dispatched: %d %q", code, got)
	}
	for _, path := range []string{"/hooks/b", "/hooks/a/x", "/hooks", "/"} {
		if code := do(path, ""); code != 404 {
			t.Errorf("%s: got %d, want 404", path, code)
		}
	}
	if code := do("/hooks/a", strings.Repeat("x", int(MaxBody)+1)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body limit not enforced: %d", code)
	}
}
