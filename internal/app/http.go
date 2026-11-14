package app

import (
	"context"
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed assets/index.html
var indexHTML string

var index = template.Must(template.New("index").Parse(indexHTML))

type Readiness func(context.Context) error

func Handler(basePath string, ready Readiness) http.Handler {
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
	mux.HandleFunc("GET "+basePath+"/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		_ = index.Execute(w, nil)
	})
	return mux
}

func writeStatus(w http.ResponseWriter, status int, value string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"status":"` + value + `"}` + "\n"))
}
