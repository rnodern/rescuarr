package app

import (
	"context"
	"net/http"
)

type Readiness func(context.Context) error

func withStatus(ready Readiness, browser http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready == nil || ready(r.Context()) != nil {
			writeStatus(w, http.StatusServiceUnavailable, "unready")
			return
		}
		writeStatus(w, http.StatusOK, "ready")
	})
	mux.Handle("/", browser)
	return mux
}

func writeStatus(w http.ResponseWriter, status int, value string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"status":"` + value + `"}` + "\n"))
}
