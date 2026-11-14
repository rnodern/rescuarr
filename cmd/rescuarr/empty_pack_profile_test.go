package main

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/store"
)

func TestCheckEmptyPackProfileOnlyReadsSelectedConnection(t *testing.T) {
	ctx := t.Context()
	local, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	const apiKey = "synthetic-secret"
	const status = `{"appName":"Sonarr","version":"5.0.0.2941","branch":"develop","isWindows":true,"isLinux":false,"isOsx":false,"isDocker":false}`
	calls := 0
	exchange := func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.Host != "fixture.invalid" || r.Header.Get("X-Api-Key") != apiKey || r.URL.RawQuery != "" && r.URL.Path != "/api/v3/queue" {
			t.Fatal("unexpected upstream request")
		}
		var body string
		switch r.URL.Path {
		case "/api/v3/system/status":
			body = status
		case "/api/v3/queue":
			body = `{"page":1,"pageSize":100,"totalRecords":0,"records":[]}`
		case "/api/v3/config/downloadclient":
			body = `{"enableCompletedDownloadHandling":true,"autoRedownloadFailed":true,"autoRedownloadFailedFromInteractiveSearch":true}`
		case "/api/v3/downloadclient":
			body = `[{"id":1,"enable":true,"name":"Fixture SAB","implementation":"Sabnzbd","protocol":"usenet","removeCompletedDownloads":false,"removeFailedDownloads":false}]`
		case "/api/v3/remotepathmapping":
			body = "[]"
		default:
			t.Fatal("unexpected upstream route")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
	}
	service := observeservice.New(local, nil, browserTransport(exchange))
	instance, err := service.AddInstance(ctx, observeservice.InstanceInput{
		Product: domain.ProductSonarr, DisplayName: "Fixture Sonarr", BaseURL: "http://fixture.invalid", APIKey: apiKey, Actor: "owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Collect(ctx, instance.Ref.ID); err != nil {
		t.Fatal(err)
	}
	calls = 0
	var output bytes.Buffer
	if err := checkEmptyPackProfile(ctx, local, "", exchange, &output); err != nil {
		t.Fatal(err)
	}
	if calls != 4 || !strings.Contains(output.String(), "outside the Linux container environment tested in the lab") ||
		strings.Contains(output.String(), apiKey) || strings.Contains(output.String(), "fixture.invalid") {
		t.Fatalf("unexpected profile report: %d requests, %s", calls, output.String())
	}
	state, err := local.State(ctx)
	if err != nil || state.Mode != domain.ModeObserve {
		t.Fatalf("profile check changed operating mode: %s %v", state.Mode, err)
	}
}
