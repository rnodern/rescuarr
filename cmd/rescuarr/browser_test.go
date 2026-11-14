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
	const upstream = "http://sonarr.invalid/arr"
	calls := 0
	var importedAt time.Time
	var importDownload string
	observations := observeservice.New(local, nil, browserTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.Host != "sonarr.invalid" || r.Header.Get("X-Api-Key") != key {
			t.Fatalf("unexpected upstream request")
		}
		var body string
		switch r.URL.Path {
		case "/arr/api/v3/system/status":
			body = `{"appName":"Sonarr","version":"5.0.0.2941","osName":"Linux","isLinux":true}`
		case "/arr/api/v3/queue":
			if !importedAt.IsZero() {
				body = `{"page":1,"pageSize":100,"totalRecords":0,"records":[]}`
				break
			}
			body = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":4,"title":"Fixture.Show.S02E04.1080p","seriesId":8,"episodeId":11,"series":{"id":8,"title":"Fixture Show"},"episode":{"id":11,"seriesId":8,"title":"Fixture Episode","seasonNumber":2,"episodeNumber":4},"downloadId":"fixture-job","downloadClient":"lab","protocol":"usenet","status":"completed","trackedDownloadState":"importBlocked","outputPath":"/private/fixture-path/Fixture.Show.S02E04.1080p","statusMessages":[{"messages":["Found matching series via grab history, but release was matched to series by ID. Automatic import is not possible. See the FAQ for details.","fixture unclassified message"]}]}]}`
		case "/arr/api/v3/history":
			if query := r.URL.Query(); query.Get("episodeId") != "11" || query.Get("seriesIds") != "8" || query.Get("eventType") != "3" {
				t.Fatal("history query was not scoped to one episode")
			}
			body = fmt.Sprintf(`{"page":1,"pageSize":50,"totalRecords":1,"records":[{"id":50,"seriesId":8,"episodeId":11,"downloadId":%q,"eventType":"downloadFolderImported","date":%q,"data":{"fileId":"23","downloadClientName":"lab"}}]}`,
				importDownload, importedAt.UTC().Format(time.RFC3339Nano))
		case "/arr/api/v3/episode/11":
			body = `{"id":11,"seriesId":8,"episodeFileId":23,"hasFile":true}`
		case "/arr/api/v3/rootfolder":
			body = `[{"id":1,"path":"/private/library"}]`
		case "/arr/api/v3/manualimport":
			if query := r.URL.Query(); len(query) != 2 || query.Get("downloadId") != "fixture-job" || query.Get("filterExistingFiles") != "false" {
				t.Fatal("automatic check used an unexpected Sonarr query")
			}
			body = `[{"path":"/private/fixture-path/Fixture.Show.S02E04.1080p/Fixture.Show.S02E04.1080p.mkv","size":4096}]`
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
	for _, route := range []string{"/cases", "/connections", "/activity", "/assets/console.css", "/fragments/status"} {
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
	form := url.Values{"csrf": {session.CSRF}, "product": {"sonarr"}, "name": {"Test Sonarr <lab>"},
		"url": {upstream}, "api_key": {key}}
	if response := request(http.MethodPost, base+"/connections", session.Token, "https://external.invalid", form); response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin connection add returned %d", response.Code)
	}
	if response := request(http.MethodPost, base+"/connections", session.Token, origin, form); response.Code != http.StatusSeeOther || response.Header().Get("Location") != base+"/connections" || calls != 0 {
		t.Fatalf("connection add returned %d with %d upstream requests", response.Code, calls)
	}
	connections := perform(base+"/connections", session.Token).Body.String()
	check := regexp.MustCompile(`action="(/rescuarr/connections/[^/]+/check)"`).FindStringSubmatch(connections)
	remove := regexp.MustCompile(`href="(/rescuarr/connections/[^/]+/remove)"`).FindStringSubmatch(connections)
	if len(check) != 2 || len(remove) != 2 || !strings.Contains(connections, "Awaiting first check") ||
		!strings.Contains(connections, "Test Sonarr &lt;lab&gt;") || !strings.Contains(connections, `name="api_key" type="password"`) {
		t.Fatal("saved connection or its check form did not render")
	}
	if response := perform(check[1], session.Token); response.Code != http.StatusMethodNotAllowed || calls != 0 {
		t.Fatalf("GET check returned %d with %d upstream requests", response.Code, calls)
	}
	if response := request(http.MethodPost, check[1], session.Token, origin, url.Values{"csrf": {"invalid"}}); response.Code != http.StatusForbidden || calls != 0 {
		t.Fatalf("invalid token check returned %d with %d upstream requests", response.Code, calls)
	}
	if response := request(http.MethodPost, check[1], session.Token, origin, url.Values{"csrf": {session.CSRF}}); response.Code != http.StatusSeeOther || calls != 4 {
		t.Fatalf("queue check returned %d with %d upstream requests", response.Code, calls)
	}
	connections = perform(base+"/connections", session.Token).Body.String()
	cases := perform(base+"/cases", session.Token).Body.String()
	detailPath := regexp.MustCompile(`href="(/rescuarr/cases/[^/"]+)"`).FindStringSubmatch(cases)
	if len(detailPath) != 2 || !strings.Contains(connections, "5.0.0.2941") || !strings.Contains(connections, "Complete") {
		t.Fatal("saved status or observed queue case did not render")
	}
	detail := perform(detailPath[1], session.Token).Body.String()
	if !strings.Contains(cases, "Fixture Show · S02E04") || !strings.Contains(cases, "Fixture.Show.S02E04.1080p") || !strings.Contains(cases, "Likely episode match") ||
		!strings.Contains(detail, "S02E04 · Fixture Episode") || !strings.Contains(detail, "Likely episode match") ||
		!strings.Contains(detail, "Fixture.Show.S02E04.1080p.mkv") || !strings.Contains(detail, "Recheck episode filename") ||
		!strings.Contains(detail, "Sonarr found the series by ID") ||
		strings.Contains(detail, "Check Arr import candidates") {
		t.Fatal("case did not show the job identity and available filename check")
	}
	response := perform(base+"/activity", session.Token)
	activity := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(activity, "Fixture Show · S02E04") ||
		!strings.Contains(activity, "Automatic import is unavailable for this setup") || !strings.Contains(activity, detailPath[1]) {
		t.Fatalf("activity did not show the case's persisted filename assessment: HTTP %d", response.Code)
	}
	for _, query := range []string{"fixture+show", "s02e04", "fixture+episode", "1080p"} {
		if !strings.Contains(perform(base+"/cases?q="+query, session.Token).Body.String(), detailPath[1]) {
			t.Fatalf("search did not find the saved case for %q", query)
		}
	}
	for _, private := range []string{key, upstream, "fixture-job", "/private/fixture-path", "fixture unclassified message"} {
		if strings.Contains(connections, private) || strings.Contains(cases, private) || strings.Contains(detail, private) || strings.Contains(activity, private) {
			t.Fatal("private connection or queue data appeared in the console")
		}
	}
	importedAt = time.Now().UTC()
	importDownload = "different-download"
	queueCheck := func() {
		t.Helper()
		if response := request(http.MethodPost, check[1], session.Token, origin, url.Values{"csrf": {session.CSRF}}); response.Code != http.StatusSeeOther {
			t.Fatalf("queue refresh returned %d", response.Code)
		}
	}
	queueCheck()
	queueCheck()
	if body := perform(detailPath[1], session.Token).Body.String(); !strings.Contains(body, "An import could not be verified") ||
		strings.Contains(body, "Sonarr imported this download and linked the episode") {
		t.Fatal("an unrelated download was marked as a verified import")
	}
	importDownload = "fixture-job"
	queueCheck()
	queueCheck()
	if body := perform(detailPath[1], session.Token).Body.String(); !strings.Contains(body, "Sonarr imported this download and linked the episode") {
		t.Fatal("verified native import did not close the case")
	}
	activity = perform(base+"/activity", session.Token).Body.String()
	if strings.Count(activity, "Sonarr import verified. Case closed.") != 1 || !strings.Contains(activity, "An import could not be verified") {
		t.Fatal("import verification and preceding closure did not appear exactly once in activity")
	}

	beforeRemove := calls
	if response := perform(remove[1], session.Token); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Past activity stays available") || calls != beforeRemove {
		t.Fatalf("connection removal confirmation returned %d", response.Code)
	}
	csrf := url.Values{"csrf": {session.CSRF}}
	for _, attempt := range []struct {
		origin string
		form   url.Values
	}{{"https://external.invalid", csrf}, {origin, url.Values{"csrf": {"invalid"}}}} {
		if response := request(http.MethodPost, remove[1], session.Token, attempt.origin, attempt.form); response.Code != http.StatusForbidden {
			t.Fatalf("untrusted removal returned %d", response.Code)
		}
	}
	if response := request(http.MethodPost, remove[1], session.Token, origin, csrf); response.Code != http.StatusSeeOther ||
		response.Header().Get("Location") != base+"/connections" {
		t.Fatalf("connection removal returned %d", response.Code)
	}
	if response := request(http.MethodPost, check[1], session.Token, origin, csrf); response.Code != http.StatusNotFound || calls != beforeRemove {
		t.Fatalf("removed queue check returned %d with %d additional upstream requests", response.Code, calls-beforeRemove)
	}
	if strings.Contains(perform(base+"/connections", session.Token).Body.String(), "Test Sonarr") ||
		!strings.Contains(perform(base+"/activity", session.Token).Body.String(), "Sonarr import verified. Case closed.") ||
		!strings.Contains(perform(detailPath[1], session.Token).Body.String(), "Sonarr imported this download and linked the episode") {
		t.Fatal("the removed connection remained listed or its recovery history disappeared")
	}
}

type browserTransport func(*http.Request) (*http.Response, error)

func (f browserTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
