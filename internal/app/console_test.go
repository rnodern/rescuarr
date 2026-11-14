package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/access"
	"github.com/rnodern/rescuarr/internal/config"
	"github.com/rnodern/rescuarr/internal/store"
)

const browserURL = "https://console.example.test"

func TestConsoleSetupAuthenticationAndCSRF(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "state")
	local, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	auth := access.New(local, dir, time.Now)
	if err := auth.PrepareSetup(ctx); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(dir, access.TokenFilename))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]string{
		"--data-dir", dir, "--base-path", "/tools", "--public-url", browserURL + "/tools",
		"--trusted-proxies", "192.0.2.0/24",
	}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	browser := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := CurrentBrowserSession(r)
		if session.Actor != access.Owner || session.Origin != browserURL || session.CSRF == "" {
			t.Errorf("incorrect browser session: %v", session.Actor)
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := ConsoleHandler(cfg, auth, local, browser)

	request := func(method, path, origin string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://container.example.test"+path, strings.NewReader(form.Encode()))
		r.RemoteAddr = "192.0.2.21:29000"
		r.Header.Set("X-Forwarded-Host", "console.example.test")
		r.Header.Set("X-Forwarded-Proto", "https")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	status := func(w *httptest.ResponseRecorder, expected int) {
		t.Helper()
		if w.Code != expected {
			t.Fatalf("got %d want %d: %s", w.Code, expected, w.Body.String())
		}
	}
	status(request(http.MethodGet, "/healthz", "", nil), http.StatusOK)
	status(request(http.MethodGet, "/readyz", "", nil), http.StatusOK)
	initial := request(http.MethodGet, "/tools/", "", nil)
	status(initial, http.StatusSeeOther)
	if initial.Header().Get("Location") != "/tools/setup" {
		t.Fatal("new installation did not direct operator to setup")
	}

	setup := request(http.MethodGet, "/tools/setup", "", nil)
	status(setup, http.StatusOK)
	preAuth := namedCookie(t, setup, "rescuarr_form")
	const password = "synthetic browser passphrase"
	form := url.Values{"password": {password}, "token": {strings.TrimSpace(string(token))}, "csrf": {preAuth.Value}}
	status(request(http.MethodPost, "/tools/setup", "", form, preAuth), http.StatusForbidden)
	status(request(http.MethodPost, "/tools/setup", browserURL+".evil.test", form, preAuth), http.StatusForbidden)
	status(request(http.MethodPost, "/tools/setup", browserURL, form), http.StatusForbidden)
	status(request(http.MethodPost, "/tools/setup", browserURL, form, preAuth), http.StatusSeeOther)
	status(request(http.MethodPost, "/tools/setup", browserURL, form, preAuth), http.StatusUnauthorized)

	login := request(http.MethodGet, "/tools/login", "", nil)
	status(login, http.StatusOK)
	preAuth = namedCookie(t, login, "rescuarr_form")
	loggedIn := request(http.MethodPost, "/tools/login", browserURL, url.Values{"password": {password}, "csrf": {preAuth.Value}}, preAuth)
	status(loggedIn, http.StatusSeeOther)
	owner := namedCookie(t, loggedIn, "rescuarr_owner")
	if !owner.Secure || !owner.HttpOnly || owner.SameSite != http.SameSiteStrictMode || owner.Path != "/tools/" {
		t.Fatalf("incorrect session attributes: %+v", owner)
	}
	status(request(http.MethodGet, "/tools/", "", nil, owner), http.StatusOK)
	status(request(http.MethodGet, "/tools/cases", "", nil, owner), http.StatusOK)
	status(request(http.MethodGet, "/tools/fragments/status", "", nil), http.StatusUnauthorized)
	status(request(http.MethodPost, "/tools/cases/example/actions", browserURL, nil), http.StatusUnauthorized)
	status(request(http.MethodGet, "/tools/", "", nil), http.StatusSeeOther)
	status(request(http.MethodPost, "/tools/logout", browserURL, nil), http.StatusUnauthorized)
	status(request(http.MethodPost, "/tools/logout", browserURL, url.Values{"csrf": {access.BrowserToken()}}, owner), http.StatusForbidden)
	session, err := auth.Authenticate(ctx, owner.Value, false)
	if err != nil {
		t.Fatal(err)
	}
	status(request(http.MethodPost, "/tools/logout", browserURL, url.Values{"csrf": {session.CSRF}}, owner), http.StatusSeeOther)
	status(request(http.MethodGet, "/tools/", "", nil, owner), http.StatusSeeOther)

	hostile := httptest.NewRequest(http.MethodGet, "http://container.example.test/tools/login", nil)
	hostile.RemoteAddr = "198.51.100.21:29000"
	hostile.Header.Set("X-Forwarded-Host", "console.example.test")
	hostile.Header.Set("X-Forwarded-Proto", "https")
	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, hostile)
	status(rejected, http.StatusMisdirectedRequest)
}

func namedCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("missing %s cookie", name)
	return nil
}
