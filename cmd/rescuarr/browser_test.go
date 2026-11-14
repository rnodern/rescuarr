package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/access"
	"github.com/rnodern/rescuarr/internal/config"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/store"
)

func TestBrowserUsesOwnerSessionAndLocalState(t *testing.T) {
	const base = "/rescuarr"
	cfg, err := config.Parse([]string{"--base-path", base, "--data-dir", filepath.Join(t.TempDir(), "state")}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	local, err := store.Open(t.Context(), cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	auth := access.New(local, cfg.DataDir, time.Now)
	if err := auth.PrepareSetup(t.Context()); err != nil {
		t.Fatal(err)
	}
	const key = "synthetic-connection-key"
	const keyFile = "fixture-key"
	const upstream = "http://sonarr.invalid/arr"
	calls := 0
	observations := observeservice.New(local, nil, browserTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.Host != "sonarr.invalid" || r.Header.Get("X-Api-Key") != key {
			t.Fatalf("unexpected upstream request")
		}
		var body string
		switch r.URL.Path {
		case "/arr/api/v3/system/status":
			body = `{"appName":"Sonarr","version":"4.0.20.3012"}`
		case "/arr/api/v3/queue":
			body = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":4,"title":"Fixture.Show.S02E04.1080p","seriesId":8,"episodeId":11,"series":{"id":8,"title":"Fixture Show"},"episode":{"id":11,"seriesId":8,"title":"Fixture Episode","seasonNumber":2,"episodeNumber":4},"downloadId":"fixture-job","downloadClient":"lab","protocol":"usenet","status":"completed","trackedDownloadState":"importPending","outputPath":"/private/fixture-path","statusMessages":[{"messages":["Found matching series via grab history, but release was matched to series by ID. Automatic import is not possible. See the FAQ for details.","fixture unclassified message"]}]}]}`
		default:
			t.Fatalf("unexpected upstream request path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
	}))
	handler := browserHandler(cfg, auth, local, observations, nil)
	request := func(method, path, token, origin string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, strings.NewReader(form.Encode()))
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if token != "" {
			request.AddCookie(&http.Cookie{Name: "rescuarr_owner", Value: token})
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, request)
		return out
	}
	perform := func(path, token string) *httptest.ResponseRecorder {
		return request(http.MethodGet, path, token, "", nil)
	}
	if response := perform(base+"/", ""); response.Code != http.StatusSeeOther || response.Header().Get("Location") != base+"/setup" {
		t.Fatalf("fresh console returned %d", response.Code)
	}
	for _, route := range []string{"/cases", "/connections", "/assets/console.css", "/fragments/status"} {
		if status := perform(base+route, "").Code; status != http.StatusUnauthorized {
			t.Errorf("anonymous %s returned %d", route, status)
		}
	}
	if err := auth.IssueRecovery(t.Context()); err != access.ErrNoOwner {
		t.Fatalf("recovery before setup: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.DataDir, access.TokenFilename))
	if err != nil {
		t.Fatal(err)
	}
	const password = "synthetic console test password"
	if err := auth.SetOwner(t.Context(), strings.TrimSpace(string(data)), password); err != nil {
		t.Fatal(err)
	}
	session, err := auth.Login(t.Context(), password, "")
	if err != nil {
		t.Fatal(err)
	}
	home := perform(base+"/", session.Token)
	if home.Code != http.StatusOK {
		t.Fatalf("authenticated overview returned %d", home.Code)
	}
	for _, expected := range []string{"<h1>Overview</h1>", "Observe", `action="/rescuarr/logout"`, fmt.Sprintf(`value="%s"`, session.CSRF)} {
		if !strings.Contains(home.Body.String(), expected) {
			t.Fatal("overview did not render the current owner shell")
		}
	}
	for route, media := range map[string]string{
		"/cases": "text/html", "/connections": "text/html", "/assets/console.css": "text/css",
		"/assets/console.js": "javascript", "/fragments/status": "text/html",
	} {
		response := perform(base+route, session.Token)
		if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), media) {
			t.Errorf("authenticated %s returned %d with type %q", route, response.Code, response.Header().Get("Content-Type"))
		}
	}
	const origin = "http://127.0.0.1:8787"
	privateDir := filepath.Join(cfg.DataDir, "secrets")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateDir, keyFile), []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"csrf": {session.CSRF}, "product": {"sonarr"}, "name": {"Test Sonarr <lab>"},
		"url": {upstream}, "secret_ref": {keyFile}}
	if response := request(http.MethodPost, base+"/connections", session.Token, "https://external.invalid", form); response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin connection add returned %d", response.Code)
	}
	if response := request(http.MethodPost, base+"/connections", session.Token, origin, form); response.Code != http.StatusSeeOther || response.Header().Get("Location") != base+"/connections" || calls != 0 {
		t.Fatalf("connection add returned %d with %d upstream requests", response.Code, calls)
	}
	connections := perform(base+"/connections", session.Token).Body.String()
	check := regexp.MustCompile(`action="(/rescuarr/connections/[^/]+/check)"`).FindStringSubmatch(connections)
	if len(check) != 2 || !strings.Contains(connections, "Awaiting first check") || !strings.Contains(connections, "Test Sonarr &lt;lab&gt;") {
		t.Fatal("saved connection or its check form did not render")
	}
	if response := perform(check[1], session.Token); response.Code != http.StatusMethodNotAllowed || calls != 0 {
		t.Fatalf("GET check returned %d with %d upstream requests", response.Code, calls)
	}
	if response := request(http.MethodPost, check[1], session.Token, origin, url.Values{"csrf": {"invalid"}}); response.Code != http.StatusForbidden || calls != 0 {
		t.Fatalf("invalid token check returned %d with %d upstream requests", response.Code, calls)
	}
	if response := request(http.MethodPost, check[1], session.Token, origin, url.Values{"csrf": {session.CSRF}}); response.Code != http.StatusSeeOther || calls != 2 {
		t.Fatalf("queue check returned %d with %d upstream requests", response.Code, calls)
	}
	connections = perform(base+"/connections", session.Token).Body.String()
	cases := perform(base+"/cases", session.Token).Body.String()
	detailPath := regexp.MustCompile(`href="(/rescuarr/cases/[^/"]+)"`).FindStringSubmatch(cases)
	if len(detailPath) != 2 || !strings.Contains(connections, "4.0.20.3012") || !strings.Contains(connections, "Complete") {
		t.Fatal("saved status or observed queue case did not render")
	}
	detail := perform(detailPath[1], session.Token).Body.String()
	if !strings.Contains(cases, "Fixture Show · S02E04") || !strings.Contains(cases, "Fixture.Show.S02E04.1080p") ||
		!strings.Contains(detail, "S02E04 · Fixture Episode") || !strings.Contains(detail, "Import preview is currently supported on Sonarr 4.0.20.3014") ||
		!strings.Contains(detail, "Sonarr found the series by ID") || !strings.Contains(detail, "choose Manual Import") ||
		strings.Contains(detail, "Check Arr import candidates") {
		t.Fatal("case did not show the job identity and version-specific limitation")
	}
	for _, query := range []string{"fixture+show", "s02e04", "fixture+episode", "1080p"} {
		if !strings.Contains(perform(base+"/cases?q="+query, session.Token).Body.String(), detailPath[1]) {
			t.Fatalf("search did not find the saved case for %q", query)
		}
	}
	for _, private := range []string{key, keyFile, upstream, "fixture-job", "/private/fixture-path", "fixture unclassified message"} {
		if strings.Contains(connections, private) || strings.Contains(cases, private) || strings.Contains(detail, private) {
			t.Fatal("private connection or queue data appeared in the console")
		}
	}

}

type browserTransport func(*http.Request) (*http.Response, error)

func (f browserTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
