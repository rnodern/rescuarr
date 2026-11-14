package arradapter

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	packStatusRoute   = "/api/v3/system/status"
	packSettingsRoute = "/api/v3/config/downloadclient"
	packClientsRoute  = "/api/v3/downloadclient"
	packMappingRoute  = "/api/v3/remotepathmapping"

	packStatusResponse = `{
		"appName":"Sonarr","version":"5.0.0.2941","branch":"develop","packageVersion":"5.0.0.2941-ls208","packageAuthor":"linuxserver.io",
		"isLinux":true,"isOsx":false,"isWindows":false,"isDocker":true,"startupPath":"/private/bin","appData":"/private/config",
		"startTime":"2026-11-14T00:00:00Z"
	}`
	packSettingsResponse = `{
		"enableCompletedDownloadHandling":true,"autoRedownloadFailed":true,"autoRedownloadFailedFromInteractiveSearch":true
	}`
	packClientResponse = `{
		"id":1,"enable":true,"name":"Rescuarr Fixture SABnzbd","implementation":"Sabnzbd","protocol":"usenet",
		"removeCompletedDownloads":false,"removeFailedDownloads":false,
		"fields":[{"name":"apiKey","value":"private-sab-key"},{"name":"host","value":"private.sab.invalid"},{"name":"urlBase"}]
	}`
)

func probeFixtureInstance() domain.Instance {
	return domain.Instance{
		Ref: domain.InstanceRef{ID: "fixture-sonarr", Epoch: 2}, Product: domain.ProductSonarr,
		BaseURL: "https://fixture.invalid/sonarr/", ConnectionRevision: 4, UpstreamVersion: arrwire.SonarrIDOnlyImportVersion,
	}
}

func packProfileFixture(t *testing.T, overrides map[string]string) (FixtureTransport, *[]string) {
	t.Helper()
	bodies := map[string]string{
		packStatusRoute: packStatusResponse, packSettingsRoute: packSettingsResponse,
		packClientsRoute: "[" + packClientResponse + "]", packMappingRoute: "[]",
	}
	for route, body := range overrides {
		bodies[route] = body
	}
	var calls []string
	return func(req *http.Request) (*http.Response, error) {
		t.Helper()
		route, prefixed := strings.CutPrefix(req.URL.Path, "/sonarr")
		body, ok := bodies[route]
		if req.Method != http.MethodGet || !prefixed || !ok || req.URL.Host != "fixture.invalid" || req.URL.RawQuery != "" ||
			req.Header.Get("X-Api-Key") != "private-sonarr-key" || req.ContentLength != 0 {
			t.Fatalf("unexpected profile request for route %s", route)
		}
		calls = append(calls, route)
		return packResponse(http.StatusOK, body), nil
	}, &calls
}

func TestProbeEmptyPackProfilePinsObservedSettings(t *testing.T) {
	instance := probeFixtureInstance()
	exchange, calls := packProfileFixture(t, nil)
	profile, err := ProbeEmptyPackProfile(t.Context(), instance, "private-sonarr-key", exchange)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Version != instance.UpstreamVersion || profile.Branch != "develop" ||
		profile.Package != "5.0.0.2941-ls208" || profile.PackageAuthor != "linuxserver.io" ||
		profile.OSFamily != "linux" || profile.PathOS != "unix" || !profile.Docker ||
		profile.ClientID != 1 || profile.ClientName != "Rescuarr Fixture SABnzbd" || profile.ClientType != "Sabnzbd" ||
		!profile.CompletedHandling || profile.RemoveCompleted || profile.RemoveFailed ||
		!profile.AutoRedownloadFailed || !profile.AutoRedownloadInteractive || profile.RemoteMappingCount != 0 {
		t.Fatalf("unexpected projected profile: %+v", profile)
	}
	if !slices.Equal(*calls, []string{packStatusRoute, packSettingsRoute, packClientsRoute, packMappingRoute}) {
		t.Fatalf("unexpected probe routes: %v", *calls)
	}
	if profile.Fingerprint == ([32]byte{}) {
		t.Fatal("no evidence fingerprint")
	}
	encode, err := json.Marshal(profile)
	if err != nil || strings.Contains(string(encode), "private") {
		t.Fatal("profile serialization exposed connection details")
	}

	for _, test := range []struct {
		name      string
		overrides map[string]string
		changed   bool
	}{
		{"stable on restart", map[string]string{packStatusRoute: strings.Replace(packStatusResponse, "T00:00:00Z", "T01:00:00Z", 1)}, false},
		{"LinuxServer linked package author", map[string]string{packStatusRoute: strings.Replace(packStatusResponse,
			`"packageAuthor":"linuxserver.io"`, `"packageAuthor":"[linuxserver.io](https://linuxserver.io)"`, 1)}, false},
		{"ignores unrelated disabled client", map[string]string{packClientsRoute: "[" + packClientResponse + `,{"id":2,"enable":false}]`}, false},
		{"retention drift", map[string]string{packClientsRoute: "[" + strings.Replace(packClientResponse, `"removeFailedDownloads":false`, `"removeFailedDownloads":true`, 1) + "]"}, true},
		{"completed handling drift", map[string]string{packSettingsRoute: strings.Replace(packSettingsResponse, `"enableCompletedDownloadHandling":true`, `"enableCompletedDownloadHandling":false`, 1)}, true},
		{"client label drift", map[string]string{packClientsRoute: "[" + strings.Replace(packClientResponse, "Rescuarr Fixture SABnzbd", "Renamed SABnzbd", 1) + "]"}, true},
		{"client endpoint drift", map[string]string{packClientsRoute: "[" + strings.Replace(packClientResponse, "private.sab.invalid", "another.sab.invalid", 1) + "]"}, true},
		{"unset url base", map[string]string{packClientsRoute: "[" + strings.Replace(packClientResponse, `"name":"urlBase"`, `"name":"urlBase","value":null`, 1) + "]"}, false},
		{"configured url base", map[string]string{packClientsRoute: "[" + strings.Replace(packClientResponse, `"name":"urlBase"`, `"name":"urlBase","value":"/sab"`, 1) + "]"}, true},
		{"package drift", map[string]string{packStatusRoute: strings.Replace(packStatusResponse, "ls208", "ls209", 1)}, true},
		{"mapping drift", map[string]string{packMappingRoute: `[{"id":1,"host":"private.host","localPath":"/private/from","remotePath":"/private/to"}]`}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			exchange, _ := packProfileFixture(t, test.overrides)
			got, err := ProbeEmptyPackProfile(t.Context(), instance, "private-sonarr-key", exchange)
			if err != nil || (got.Fingerprint != profile.Fingerprint) != test.changed {
				t.Fatalf("unexpected fingerprint comparison: changed=%v, error=%v", got.Fingerprint != profile.Fingerprint, err)
			}
			encode, err := json.Marshal(got)
			if err != nil || strings.Contains(string(encode), "private") {
				t.Fatal("profile exposed provider or mapping fields")
			}
		})
	}
}

func TestProbeEmptyPackProfileRejectsIncompleteOrAmbiguousEvidence(t *testing.T) {
	instance := probeFixtureInstance()
	for _, test := range []struct {
		name  string
		route string
		body  string
		want  error
	}{
		{"current version changed", packStatusRoute, strings.ReplaceAll(packStatusResponse, "5.0.0.2941", "5.0.0.2942"), ErrQualification},
		{"conflicting os flags", packStatusRoute, strings.Replace(packStatusResponse, `"isWindows":false`, `"isWindows":true`, 1), ErrReadback},
		{"missing OS flags", packStatusRoute, `{"appName":"Sonarr","version":"5.0.0.2941","branch":"develop"}`, ErrReadback},
		{"settings missing", packSettingsRoute, `{"enableCompletedDownloadHandling":true}`, ErrReadback},
		{"two enabled clients", packClientsRoute, "[" + packClientResponse + "," + strings.Replace(packClientResponse, `"id":1`, `"id":2`, 1) + "]", ErrReadback},
		{"wrong client", packClientsRoute, "[" + strings.Replace(packClientResponse, `"implementation":"Sabnzbd"`, `"implementation":"Nzbget"`, 1) + "]", ErrReadback},
		{"no enabled clients", packClientsRoute, "[" + strings.Replace(packClientResponse, `"enable":true`, `"enable":false`, 1) + "]", ErrReadback},
		{"missing retention setting", packClientsRoute, "[" + strings.Replace(packClientResponse, `,"removeFailedDownloads":false`, "", 1) + "]", ErrReadback},
		{"unexpected mapping data", packMappingRoute, `[{"host":"private.host","remotePath":"/private/path"}]`, ErrReadback},
		{"null mappings", packMappingRoute, "null", ErrReadback},
	} {
		t.Run(test.name, func(t *testing.T) {
			exchange, calls := packProfileFixture(t, map[string]string{test.route: test.body})
			got, err := ProbeEmptyPackProfile(t.Context(), instance, "private-sonarr-key", exchange)
			if got != (EmptyPackProfile{}) || !errors.Is(err, test.want) || (*calls)[len(*calls)-1] != test.route {
				t.Fatalf("expected no profile after malformed response: %v", err)
			}
		})
	}
}

func TestProbeEmptyPackProfileKeepsTransportErrorPrivate(t *testing.T) {
	got, err := ProbeEmptyPackProfile(t.Context(), probeFixtureInstance(), "private-sonarr-key",
		func(*http.Request) (*http.Response, error) {
			return nil, errors.New("private endpoint, key and provider details")
		})
	if got != (EmptyPackProfile{}) || !errors.Is(err, ErrReadback) || strings.Contains(err.Error(), "private") {
		t.Fatalf("transport error escaped redaction: %v", err)
	}
}
