// Package webconsole renders authenticated local recovery projections.
package webconsole

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"strings"

	"github.com/rnodern/rescuarr/internal/app"
)

// Session carries values issued by the authentication boundary. Origin is the
// canonical external HTTP origin derived from validated host/proxy settings.
type Session = app.BrowserSession

// Authorize authenticates the current owner. An empty session denies access.
type Authorize func(*http.Request) Session

type Options struct {
	// BasePath is a previously validated, canonical path or the empty string.
	BasePath  string
	Read      app.ConsoleReader
	Commands  app.ConsoleCommands
	Authorize Authorize
	Clock     app.Clock
}

type console struct {
	Options
	key [32]byte
}

//go:embed templates/*.tmpl assets/*
var files embed.FS

var views = template.Must(template.New("").Funcs(template.FuncMap{
	"state": stateLabel, "mode": modeLabel, "reason": reasonLabel,
	"target": targetLabel, "effect": effectLabel, "verification": verificationLabel,
	"action": actionLabel, "intent": intentLabel, "when": formatTime,
}).ParseFS(files, "templates/*.tmpl"))

func New(options Options) http.Handler {
	if options.Clock == nil {
		options.Clock = app.SystemClock{}
	}
	c := console{Options: options}
	rand.Read(c.key[:])
	mux := http.NewServeMux()
	base := c.BasePath
	mux.HandleFunc("GET "+base+"/{$}", c.overview)
	mux.HandleFunc("GET "+base+"/cases", c.cases)
	mux.HandleFunc("GET "+base+"/cases/{caseID}", c.caseDetail)
	mux.HandleFunc("GET "+base+"/cases/{caseID}/review/{proposalID}", c.review)
	mux.HandleFunc("POST "+base+"/cases/{caseID}/actions", c.approve)
	mux.HandleFunc("GET "+base+"/actions/{intentID}", c.intent)
	mux.HandleFunc("GET "+base+"/fragments/status", c.statusFragment)
	assets, err := fs.Sub(files, "assets")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"console.css", "console.js"} {
		mux.HandleFunc("GET "+base+"/assets/"+name, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, assets, name)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if c.Authorize != nil && c.Read != nil {
			session := c.Authorize(r)
			if session.Actor != "" && session.CSRF != "" && canonicalOrigin(session.Origin) {
				mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, session)))
				return
			}
		}
		c.errorPage(w, r, http.StatusUnauthorized, "", "Sign in to continue", "Your session is missing or has expired.")
	})
}

type sessionKey struct{}

func currentSession(r *http.Request) Session {
	return r.Context().Value(sessionKey{}).(Session)
}

type page struct {
	Base, Title, Active, CSRF string
	Status                    app.Overview
	Body                      any
}

func (c console) render(w http.ResponseWriter, r *http.Request, code int, name string, p page) {
	p.Base = c.BasePath
	session, _ := r.Context().Value(sessionKey{}).(Session)
	p.CSRF = session.CSRF
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = views.ExecuteTemplate(w, name, p)
}

func (c console) errorPage(w http.ResponseWriter, r *http.Request, code int, active, title, message string) {
	c.render(w, r, code, "error", page{Title: title, Active: active, Body: message})
}

func (c console) readError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, app.ErrNotFound) {
		c.errorPage(w, r, http.StatusNotFound, "cases", "Record unavailable", "Return to Cases to choose a current record.")
		return
	}
	c.errorPage(w, r, http.StatusServiceUnavailable, "", "Local data is unavailable", "Try refreshing after the local service recovers.")
}

func (c console) loadStatus(w http.ResponseWriter, r *http.Request) (app.Overview, bool) {
	status, err := c.Read.Overview(r.Context())
	if err != nil {
		c.readError(w, r, err)
	}
	return status, err == nil
}

func (c console) overview(w http.ResponseWriter, r *http.Request) {
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "overview", page{Title: "Overview", Active: "overview", Status: status})
}

func (c console) statusFragment(w http.ResponseWriter, r *http.Request) {
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	session := currentSession(r)
	tag := c.signedRequest(session, "status", etagFields(status))
	etag := `"` + tag + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	for value := range strings.SplitSeq(r.Header.Get("If-None-Match"), ",") {
		match := strings.TrimPrefix(strings.TrimSpace(value), "W/")
		if match == etag || match == "*" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	c.render(w, r, http.StatusOK, "status", page{Status: status})
}

func equalSecret(got, expected string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

func localID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, ch := range id {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_", ch) {
			return false
		}
	}
	return true
}
