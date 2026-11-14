package arradapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

type packFixture struct {
	now            time.Time
	sentAt         time.Time
	qualified      bool
	version        string
	members        []int64
	candidates     string
	rejected       bool
	wrongBlocklist bool
	oldFailure     bool
	recreated      bool
	competing      bool
	covered        bool
	wrongReply     bool
	deletes, posts int
	scope          EmptyPackScope
}

func newPackFixture() *packFixture {
	now := time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC)
	return &packFixture{now: now, sentAt: now.Add(-time.Second), qualified: true, version: arrwire.SonarrIDOnlyImportVersion,
		members: []int64{11, 12}, candidates: "[]", scope: EmptyPackScope{
			QueueIDs: []int64{21, 22}, DownloadID: "fixture-original", Release: "Fixture.Show.S01",
			SeriesID: 5, EpisodeIDs: []int64{11, 12},
		}}
}

func (f *packFixture) adapter(t *testing.T) *EmptyPackAdapter {
	t.Helper()
	config := Config{Instance: domain.Instance{
		Ref:     domain.InstanceRef{ID: "sonarr-fixture", Epoch: 1},
		Product: domain.ProductSonarr, BaseURL: "https://fixture.invalid/sonarr",
		UpstreamVersion: arrwire.SonarrIDOnlyImportVersion, ConnectionRevision: 2,
	}, APIKey: "fixture-secret", AdapterRevision: "fixture-pack-v1", SettingsFingerprint: sha256.Sum256([]byte("isolated settings"))}
	adapter, err := NewEmptyPackAdapter(config, func(req *http.Request) (*http.Response, error) {
		return f.exchange(t, req)
	}, func(_ context.Context, binding Binding) (Qualification, error) {
		if binding.Instance != config.Instance.Ref || binding.Product != config.Instance.Product ||
			binding.Version != config.Instance.UpstreamVersion || binding.ConnectionRevision != config.Instance.ConnectionRevision ||
			binding.AdapterRevision != config.AdapterRevision || binding.SettingsFingerprint != config.SettingsFingerprint ||
			(binding.Route != RouteEmptyPackReject && binding.Route != RouteEmptyPackSearch) {
			t.Fatalf("incomplete effect binding: %+v", binding)
		}
		if !f.qualified {
			binding.ConnectionRevision++
		}
		return Qualification{Binding: binding, Verified: true, Evidence: "fixture-only",
			ObservedAt: f.now.Add(-time.Minute), ExpiresAt: f.now.Add(time.Minute)}, nil
	}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func (f *packFixture) exchange(t *testing.T, req *http.Request) (*http.Response, error) {
	t.Helper()
	if req.URL.Host != "fixture.invalid" || req.Header.Get("X-Api-Key") != "fixture-secret" ||
		!strings.HasPrefix(req.URL.Path, "/sonarr/") {
		t.Fatalf("request escaped the fixture: %+v", req.URL)
	}
	path := strings.TrimPrefix(req.URL.Path, "/sonarr")
	query := req.URL.Query()
	jsonReply := func(body any) *http.Response {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return packResponse(200, string(encoded))
	}
	page := func(rows []map[string]any, size int) *http.Response {
		return jsonReply(map[string]any{"page": 1, "pageSize": size, "totalRecords": len(rows), "records": rows})
	}
	if req.Method == http.MethodDelete {
		f.deletes++
		if req.GetBody != nil || !req.Close || path != "/api/v3/queue/21" ||
			req.URL.RawQuery != "blocklist=true&changeCategory=false&removeFromClient=false&skipRedownload=true" {
			t.Fatal("queue removal did not retain the client item and suppress broad replacement")
		}
		f.rejected = true
		return packResponse(200, ""), nil
	}
	if req.Method == http.MethodPost {
		f.posts++
		if req.GetBody != nil || !req.Close || path != "/api/v3/command" {
			t.Fatal("unexpected search request")
		}
		payload, err := io.ReadAll(req.Body)
		if err != nil || string(payload) != `{"name":"EpisodeSearch","episodeIds":[12]}` {
			t.Fatalf("search was not scoped correctly: %q, %v", payload, err)
		}
		id := 12
		if f.wrongReply {
			id = 11
		}
		return packResponse(201, fmt.Sprintf(`{"id":81,"name":"EpisodeSearch","status":"queued","body":{"episodeIds":[%d]}}`, id)), nil
	}
	if req.Method != http.MethodGet {
		t.Fatalf("unexpected request %s", req.Method)
	}
	switch path {
	case "/api/v3/system/status":
		return jsonReply(map[string]any{"appName": "Sonarr", "version": f.version}), nil
	case "/api/v3/history":
		if query.Get("downloadId") != f.scope.DownloadID || query.Get("page") != "1" {
			t.Fatal("history read escaped download scope")
		}
		var rows []map[string]any
		for _, id := range f.scope.EpisodeIDs {
			rows = append(rows, map[string]any{"id": id, "eventType": "grabbed", "seriesId": 5, "episodeId": id,
				"downloadId": f.scope.DownloadID, "sourceTitle": f.scope.Release, "date": f.now.Add(-time.Hour),
				"data": map[string]any{"Indexer": "fixture-indexer"}})
			if f.rejected {
				date := f.sentAt
				if f.oldFailure {
					date = date.Add(-time.Second)
				}
				rows = append(rows, map[string]any{"id": id + 100, "eventType": "downloadFailed", "seriesId": 5, "episodeId": id,
					"downloadId": f.scope.DownloadID, "sourceTitle": f.scope.Release, "date": date})
			}
		}
		return page(rows, 400), nil
	case "/api/v3/blocklist":
		if query.Get("seriesIds") != "5" || query.Get("protocols") != "usenet" {
			t.Fatal("blocklist escaped series and client protocol scope")
		}
		rows := make([]map[string]any, 0, 1)
		if f.rejected {
			ids := f.scope.EpisodeIDs
			if f.wrongBlocklist {
				ids = ids[:1]
			}
			rows = append(rows, map[string]any{"id": 1, "seriesId": 5, "episodeIds": ids, "date": f.sentAt,
				"indexer": "fixture-indexer", "sourceTitle": f.scope.Release, "protocol": "usenet"})
		}
		return page(rows, 400), nil
	case "/api/v3/manualimport":
		if len(query) != 2 || query.Get("downloadId") != f.scope.DownloadID || query.Get("filterExistingFiles") != "false" {
			t.Fatal("manual import read escaped exact download scope")
		}
		return packResponse(200, f.candidates), nil
	case "/api/v3/queue":
		rows := make([]map[string]any, 0, 2)
		if !f.rejected || f.recreated {
			for index, episode := range f.members {
				id := 21 + index
				if f.recreated {
					id += 800
				}
				rows = append(rows, map[string]any{"id": id, "seriesId": 5, "episodeId": episode, "downloadId": f.scope.DownloadID,
					"downloadClient": "SAB fixture", "title": f.scope.Release, "status": "completed", "trackedDownloadState": "importPending",
					"protocol": "usenet", "outputPath": "/fixture/original", "statusMessages": []map[string][]string{
						{"messages": {"No files found are eligible for import in /fixture/original"}},
					}})
			}
		}
		if f.competing {
			rows = append(rows, map[string]any{"id": 30, "seriesId": 5, "episodeId": 12, "downloadId": "replacement",
				"status": "downloading"})
		}
		return page(rows, 100), nil
	case "/api/v3/series/5":
		return packResponse(200, `{"id":5,"monitored":true,"runtime":11}`), nil
	case "/api/v3/episode":
		if requested := query["episodeIds"]; !slices.Equal(requested, []string{"12"}) {
			t.Fatalf("read more than the exact search subset: %v", requested)
		}
		file := 0
		if f.covered {
			file = 40
		}
		return packResponse(200, fmt.Sprintf(`[{"id":12,"seriesId":5,"monitored":true,"hasFile":%t,"episodeFileId":%d,"airDateUtc":"2026-10-15T00:00:00Z"}]`, file != 0, file)), nil
	default:
		t.Fatalf("unexpected GET %s", path)
		return nil, nil
	}
}

func packResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func TestEmptyPackAdapterRejectsOnceAndSearchesOnlyScopedEpisode(t *testing.T) {
	test := newPackFixture()
	adapter := test.adapter(t)
	ctx := t.Context()
	if err := adapter.Preflight(ctx, test.scope); err != nil || test.deletes != 0 {
		t.Fatalf("read-only preflight failed: %v", err)
	}
	if err := adapter.Reject(ctx, test.scope); err != nil || test.deletes != 1 {
		t.Fatalf("got %d queue removals with error %v", test.deletes, err)
	}
	if err := adapter.Reject(ctx, test.scope); !errors.Is(err, ErrReadback) || test.deletes != 1 {
		t.Fatalf("adapter sent a second queue removal: %v", err)
	}
	if verified, err := adapter.VerifyRejected(ctx, test.scope, test.sentAt); err != nil || !verified {
		t.Fatalf("independent queue, history and blocklist did not verify: %v", err)
	}
	if id, err := adapter.Search(ctx, test.scope, []int64{12}); err != nil || id != 81 || test.posts != 1 {
		t.Fatalf("got command %d and %d search requests with error %v", id, test.posts, err)
	}
}

func TestEmptyPackAdapterHoldsUnqualifiedOrChangedPack(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*packFixture)
		want error
	}{
		{"wrong binding", func(f *packFixture) { f.qualified = false }, ErrQualification},
		{"changed version", func(f *packFixture) { f.version = "5.0.0.2942" }, ErrQualification},
		{"null file list", func(f *packFixture) { f.candidates = "null" }, ErrReadback},
		{"possible media", func(f *packFixture) { f.candidates = "[{}]" }, ErrReadback},
		{"lost member", func(f *packFixture) { f.members = f.members[:1] }, ErrReadback},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPackFixture()
			test.edit(fixture)
			if err := fixture.adapter(t).Reject(t.Context(), fixture.scope); !errors.Is(err, test.want) || fixture.deletes != 0 {
				t.Fatalf("unsafe reject decision: %v, %d deletes", err, fixture.deletes)
			}
		})
	}
}

func TestEmptyPackRejectReadbackRequiresCompleteEffect(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*packFixture)
	}{
		{"sub-pack blocklist", func(f *packFixture) { f.wrongBlocklist = true }},
		{"stale failed history", func(f *packFixture) { f.oldFailure = true }},
		{"item returned with new aliases", func(f *packFixture) { f.recreated = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPackFixture()
			fixture.rejected = true
			test.edit(fixture)
			if verified, err := fixture.adapter(t).VerifyRejected(t.Context(), fixture.scope, fixture.sentAt); err != nil || verified {
				t.Fatalf("incomplete reject had effect: %t, %v", verified, err)
			}
		})
	}
}

func TestEmptyPackSearchRequiresFreshWantAndExactAcknowledgement(t *testing.T) {
	for _, test := range []struct {
		name  string
		edit  func(*packFixture)
		ids   []int64
		want  error
		posts int
	}{
		{"outside pack", func(*packFixture) {}, []int64{13}, ErrPrepared, 0},
		{"recently imported", func(f *packFixture) { f.covered = true }, []int64{12}, ErrReadback, 0},
		{"alternative covering download", func(f *packFixture) { f.competing = true }, []int64{12}, ErrReadback, 0},
		{"mismatched echo", func(f *packFixture) { f.wrongReply = true }, []int64{12}, ErrUncertain, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPackFixture()
			fixture.rejected = true
			test.edit(fixture)
			id, err := fixture.adapter(t).Search(t.Context(), fixture.scope, test.ids)
			if id != 0 || !errors.Is(err, test.want) || fixture.posts != test.posts {
				t.Fatalf("unsafe search decision: id %d, error %v, %d posts", id, err, fixture.posts)
			}
		})
	}
}
