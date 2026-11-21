// Package ingress serves inbound webhook endpoints for frontends that need
// them. Each path is matched exactly; signature verification belongs to the
// frontend handler.
package ingress

import "net/http"

// MaxBody caps webhook request bodies. Platform event payloads are small;
// media is downloaded separately by the adapter.
const MaxBody int64 = 1 << 20

// New returns a handler dispatching each exact path to its frontend handler.
// Unknown paths receive 404.
func New(handlers map[string]http.Handler) http.Handler {
	mux := http.NewServeMux()
	for path, h := range handlers {
		mux.Handle(path, http.MaxBytesHandler(h, MaxBody))
	}
	return mux
}
