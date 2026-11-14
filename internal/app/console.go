package app

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/rnodern/rescuarr/internal/access"
	"github.com/rnodern/rescuarr/internal/config"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

//go:embed assets/access.html
var consoleAssets embed.FS

var consoleTemplates = template.Must(template.ParseFS(consoleAssets, "assets/access.html"))

type BrowserSession struct {
	Actor  domain.ActorID
	CSRF   string
	Origin string
}

type browserSessionKey struct{}

// CurrentBrowserSession supplies an authenticated session to the console.
func CurrentBrowserSession(r *http.Request) BrowserSession {
	session, _ := r.Context().Value(browserSessionKey{}).(BrowserSession)
	return session
}

type console struct {
	cfg  config.Config
	auth *access.Auth
}

func ConsoleHandler(cfg config.Config, auth *access.Auth, local *store.Store, browser http.Handler) http.Handler {
	c := &console{cfg: cfg, auth: auth}
	mux := http.NewServeMux()
	base := cfg.BasePath
	mux.HandleFunc("GET "+base+"/setup", c.showSetup)
	mux.HandleFunc("POST "+base+"/setup", c.submitSetup)
	mux.HandleFunc("GET "+base+"/login", c.showLogin)
	mux.HandleFunc("POST "+base+"/login", c.submitLogin)
	mux.HandleFunc("POST "+base+"/logout", c.logout)
	mux.Handle(base+"/", c.authenticatedBrowser(browser))
	return withStatus(local.Ready, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers(w)
		if _, ok := c.origin(r); !ok {
			http.Error(w, "Unrecognised request host", http.StatusMisdirectedRequest)
			return
		}
		if base != "" && r.URL.Path == base && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			http.Redirect(w, r, base+"/", http.StatusSeeOther)
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

func (c *console) authenticatedBrowser(browser http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isHome := r.URL.Path == c.cfg.BasePath+"/" && (r.Method == http.MethodGet || r.Method == http.MethodHead)
		// Background status polling does not count as owner activity.
		extend := r.URL.Path != c.cfg.BasePath+"/fragments/status"
		session, err := c.auth.Authenticate(r.Context(), readCookie(r, "rescuarr_owner"), extend)
		if errors.Is(err, access.ErrDenied) && isHome {
			required, err := c.auth.SetupRequired(r.Context())
			if err != nil {
				unavailable(w)
				return
			}
			if required {
				c.redirect(w, r, "/setup")
			} else {
				c.redirect(w, r, "/login")
			}
			return
		}
		if errors.Is(err, access.ErrDenied) {
			http.Error(w, "Sign in again", http.StatusUnauthorized)
			return
		}
		if err != nil || browser == nil {
			unavailable(w)
			return
		}
		origin, _ := c.origin(r)
		scope := BrowserSession{Actor: access.Owner, CSRF: session.CSRF, Origin: origin}
		browser.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), browserSessionKey{}, scope)))
	})
}

func (c *console) showSetup(w http.ResponseWriter, r *http.Request) {
	c.renderAccess(w, r, true, http.StatusOK, "")
}

func (c *console) showLogin(w http.ResponseWriter, r *http.Request) {
	required, err := c.auth.SetupRequired(r.Context())
	if err != nil {
		unavailable(w)
		return
	}
	if required {
		c.redirect(w, r, "/setup")
		return
	}
	c.renderAccess(w, r, false, http.StatusOK, "")
}

func (c *console) renderAccess(w http.ResponseWriter, r *http.Request, setup bool, status int, message string) {
	data := struct {
		Title  string
		Action string
		CSRF   string
		Error  string
		Setup  bool
	}{"Sign in", c.cfg.BasePath + "/login", access.BrowserToken(), message, setup}
	if setup {
		data.Title = "Set owner password"
		data.Action = c.cfg.BasePath + "/setup"
	}
	c.setCookie(w, r, "rescuarr_form", data.CSRF, 900)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = consoleTemplates.ExecuteTemplate(w, "access", data)
}

func (c *console) submitSetup(w http.ResponseWriter, r *http.Request) {
	if !c.validatePost(w, r, readCookie(r, "rescuarr_form")) {
		return
	}
	err := c.auth.SetOwner(r.Context(), r.PostForm.Get("token"), r.PostForm.Get("password"))
	if c.authError(w, r, true, err) {
		return
	}
	c.setCookie(w, r, "rescuarr_form", "", -1)
	c.redirect(w, r, "/login")
}

func (c *console) submitLogin(w http.ResponseWriter, r *http.Request) {
	if !c.validatePost(w, r, readCookie(r, "rescuarr_form")) {
		return
	}
	session, err := c.auth.Login(r.Context(), r.PostForm.Get("password"), readCookie(r, "rescuarr_owner"))
	if c.authError(w, r, false, err) {
		return
	}
	c.setCookie(w, r, "rescuarr_form", "", -1)
	c.setCookie(w, r, "rescuarr_owner", session.Token, 86400)
	c.redirect(w, r, "/")
}

func (c *console) logout(w http.ResponseWriter, r *http.Request) {
	token := readCookie(r, "rescuarr_owner")
	session, err := c.auth.Authenticate(r.Context(), token, false)
	if errors.Is(err, access.ErrDenied) {
		http.Error(w, "Sign in again", http.StatusUnauthorized)
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	if !c.validatePost(w, r, session.CSRF) {
		return
	}
	if err := c.auth.Logout(r.Context(), token); err != nil {
		unavailable(w)
		return
	}
	c.setCookie(w, r, "rescuarr_owner", "", -1)
	c.redirect(w, r, "/login")
}

func (c *console) authError(w http.ResponseWriter, r *http.Request, setup bool, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, access.ErrDenied):
		c.renderAccess(w, r, setup, http.StatusUnauthorized, err.Error())
	case errors.Is(err, access.ErrPassword):
		c.renderAccess(w, r, setup, http.StatusBadRequest, err.Error())
	case errors.Is(err, access.ErrBusy):
		c.renderAccess(w, r, setup, http.StatusTooManyRequests, err.Error())
	default:
		unavailable(w)
	}
	return true
}

func (c *console) validatePost(w http.ResponseWriter, r *http.Request, expected string) bool {
	origin, _ := c.origin(r)
	value := r.Header.Get("Origin")
	isReferer := value == ""
	if isReferer {
		value = r.Header.Get("Referer")
	}
	from, err := url.Parse(value)
	if err != nil || from.User != nil || from.Scheme+"://"+strings.ToLower(from.Host) != origin ||
		(!isReferer && (from.Path != "" || from.RawQuery != "" || from.Fragment != "")) {
		http.Error(w, "A same-origin request is required", http.StatusForbidden)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return false
	}
	if csrf := r.PostForm["csrf"]; len(csrf) != 1 || !access.EqualToken(csrf[0], expected) {
		http.Error(w, "Invalid form token", http.StatusForbidden)
		return false
	}
	return true
}

func (c *console) origin(r *http.Request) (string, bool) {
	host, scheme := strings.ToLower(r.Host), "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if c.cfg.PublicURL != nil {
		peer, _, _ := net.SplitHostPort(r.RemoteAddr)
		addr, _ := netip.ParseAddr(peer)
		for _, proxy := range c.cfg.TrustedProxies {
			if proxy.Contains(addr.Unmap()) {
				host = strings.ToLower(r.Header.Get("X-Forwarded-Host"))
				scheme = r.Header.Get("X-Forwarded-Proto")
				break
			}
		}
		return scheme + "://" + host, host == strings.ToLower(c.cfg.PublicURL.Host) && scheme == c.cfg.PublicURL.Scheme
	}
	name, port, err := net.SplitHostPort(host)
	_, configuredPort, _ := net.SplitHostPort(c.cfg.Listen)
	return scheme + "://" + host, err == nil && port == configuredPort && config.LoopbackHost(name)
}

func (c *console) setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	origin, _ := c.origin(r)
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: c.cfg.BasePath + "/", MaxAge: maxAge,
		HttpOnly: true, Secure: strings.HasPrefix(origin, "https://"), SameSite: http.SameSiteStrictMode,
	})
}

func readCookie(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (c *console) redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, c.cfg.BasePath+path, http.StatusSeeOther)
}

func headers(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
}

func unavailable(w http.ResponseWriter) {
	http.Error(w, "Local service unavailable", http.StatusServiceUnavailable)
}
