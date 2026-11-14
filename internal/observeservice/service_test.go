package observeservice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/store"
)

const credential = "synthetic-key-for-observe-test"

type fixture func(*http.Request) (*http.Response, error)

func (f fixture) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func response(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
}

func testStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	db, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := os.Mkdir(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets", "fixture"), []byte(credential+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return db, dir
}

func add(t *testing.T, service *Service, product domain.Product) domain.Instance {
	t.Helper()
	instance, err := service.AddInstance(t.Context(), InstanceInput{Product: product, DisplayName: "Local " + string(product),
		BaseURL: "http://" + string(product) + ".invalid/arr/", SecretRef: "fixture", Actor: "synthetic-owner"})
	if err != nil {
		t.Fatal(err)
	}
	return instance
}

func queue(body string, total int) string {
	return fmt.Sprintf(`{"page":1,"pageSize":100,"totalRecords":%d,"records":[%s]}`, total, body)
}

const sonarrPack = `{"id":4,"seriesId":8,"episodeId":11,"downloadId":"123","downloadClient":"lab","protocol":"usenet","status":"completed","trackedDownloadState":"importPending","statusMessages":[{"messages":["No files found are eligible for import"]}]},
{"id":7,"seriesId":8,"episodeId":12,"downloadId":"123","downloadClient":"lab","protocol":"usenet","status":"completed"}`

const radarrMovie = `{"id":4,"movieId":11,"downloadId":"123","downloadClient":"lab","protocol":"usenet","status":"failed","outputPath":"/private/not-for-console"}`

func TestObserveProjectsAndRetainsCaseState(t *testing.T) {
	ctx := t.Context()
	db, dir := testStore(t)
	var sonarrMode string
	var calls int
	var version = "4.0.20.3014"
	transport := fixture(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.Header.Get("X-Api-Key") != credential || strings.Contains(request.URL.RawQuery, credential) {
			t.Fatal("unexpected method or credential handling")
		}
		product := strings.Split(request.URL.Hostname(), ".")[0]
		switch request.URL.Path {
		case "/arr/api/v3/system/status":
			return response(200, fmt.Sprintf(`{"appName":%q,"version":%q}`, product, version))
		case "/arr/api/v3/queue":
			if product == "radarr" {
				return response(200, queue(radarrMovie, 1))
			}
			switch sonarrMode {
			case "partial":
				return response(200, queue(strings.SplitN(sonarrPack, ",\n", 2)[0], 2))
			case "absent":
				return response(200, queue("", 0))
			case "outage":
				return response(503, `{"bad":"redacted"}`)
			default:
				return response(200, queue(sonarrPack, 2))
			}
		default:
			t.Fatalf("unexpected route %s", request.URL.Path)
			return nil, nil
		}
	})
	clock := app.FixedClock{At: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
	service := New(db, clock, transport)
	sonarr, radarr := add(t, service, domain.ProductSonarr), add(t, service, domain.ProductRadarr)
	if calls != 0 {
		t.Fatal("adding an instance performed an upstream request")
	}
	if _, err := service.AddInstance(ctx, InstanceInput{Product: domain.ProductSonarr, DisplayName: "duplicate", BaseURL: "HTTP://SONARR.invalid:80/arr", SecretRef: "fixture", Actor: "owner"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("normalised duplicate accepted: %v", err)
	}
	if err := service.Collect(ctx, sonarr.Ref.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Collect(ctx, radarr.Ref.ID); err != nil {
		t.Fatal(err)
	}
	cases, err := service.Cases(ctx, app.CaseQuery{Limit: 1})
	if err != nil || len(cases.Cases) != 1 || cases.After == "" {
		t.Fatalf("bounded page unavailable: %+v %v", cases, err)
	}
	next, err := service.Cases(ctx, app.CaseQuery{Limit: 1, After: cases.After})
	if err != nil || len(next.Cases) != 1 || next.After != "" || cases.Cases[0].ID == next.Cases[0].ID {
		t.Fatalf("next page overlapped or skipped: %+v %v", next, err)
	}
	overview, err := service.Overview(ctx)
	if err != nil || overview.Mode != domain.ModeObserve || overview.Cases != 2 || overview.HeldCases != 2 {
		t.Fatalf("unexpected overview: %+v %v", overview, err)
	}
	getCase := func() domain.Case {
		t.Helper()
		page, err := service.Cases(ctx, app.CaseQuery{Instance: sonarr.Ref.ID, Search: "local sonarr"})
		if err != nil || len(page.Cases) != 1 {
			t.Fatalf("unexpected Sonarr projection: %+v %v", page, err)
		}
		detail, err := service.Case(ctx, page.Cases[0].ID)
		if err != nil || detail.Case.ID != page.Cases[0].ID || len(detail.Proposals) > 0 {
			t.Fatalf("unexpected detail %+v %v", detail, err)
		}
		return detail.Case
	}
	first := getCase()
	if len(first.Targets) != 2 || first.Reason != "no_eligible_files" || !first.Job.CompleteSet {
		t.Fatalf("exact Sonarr pack not grouped: %+v", first)
	}
	if err := service.Collect(ctx, sonarr.Ref.ID); err != nil || getCase().Revision != first.Revision {
		t.Fatalf("unchanged poll created a new material revision: %v", err)
	}
	sonarrMode = "partial"
	if err := service.Collect(ctx, sonarr.Ref.ID); !errors.Is(err, observe.ErrPartial) || len(getCase().Targets) != 2 {
		t.Fatal("partial page lost a positively observed pack member")
	}
	sonarrMode = "outage"
	if err := service.Collect(ctx, sonarr.Ref.ID); !errors.Is(err, observe.ErrUnavailable) || getCase().Revision != first.Revision {
		t.Fatal("outage altered a reliable projection")
	}
	sonarrMode = "absent"
	if err := service.Collect(ctx, sonarr.Ref.ID); err != nil || getCase().State != domain.CaseHeld {
		t.Fatal("first complete absence should hold")
	}
	if err := service.Collect(ctx, sonarr.Ref.ID); err != nil || getCase().State != domain.CaseClosed || getCase().Reason != "queue_missing_unverified" {
		t.Fatal("second complete absence should mark unknown resolution")
	}
	sonarrMode = ""
	if err := service.Collect(ctx, sonarr.Ref.ID); err != nil || getCase().ID != first.ID || getCase().State != domain.CaseHeld {
		t.Fatal("same case did not reopen")
	}
	version = "4.0.21.1000"
	if err := service.Collect(ctx, sonarr.Ref.ID); !errors.Is(err, observe.ErrResponse) {
		t.Fatal("upstream version drift did not gate that scan")
	}
	instances, err := service.Instances(ctx)
	if err != nil || len(instances) != 2 || instances[1].Status != "contract_changed" || instances[1].Coverage != domain.ObservationUnknown {
		t.Fatalf("instance coverage unavailable: %+v %v", instances, err)
	}
	if _, err := service.Intent(ctx, "unwired"); !errors.Is(err, app.ErrNotFound) {
		t.Fatal("observe should not expose executable intents")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	service = New(opened, clock, transport)
	if got := getCase(); got.ID != first.ID {
		t.Fatal("persisted case lost across restart")
	}
	var leaked int
	if err := opened.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM observe_cases WHERE payload LIKE ? OR payload LIKE ?`, "%private/not-for-console%", "%"+credential+"%").Scan(&leaked)
	}); err != nil || leaked != 0 {
		t.Fatalf("private upstream data in projection: %d, %v", leaked, err)
	}
}

func TestObserveOneScanAtATime(t *testing.T) {
	db, _ := testStore(t)
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	s := New(db, nil, fixture(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/arr/api/v3/system/status" {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
			return response(200, `{"appName":"Sonarr","version":"4.0.20.3014"}`)
		}
		return response(200, queue("", 0))
	}))
	instance := add(t, s, domain.ProductSonarr)
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := s.Collect(t.Context(), instance.Ref.ID); err != nil {
			t.Error(err)
		}
	})
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Collect(ctx, instance.Ref.ID); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("coalesced update launched another operation: %v, calls=%d", err, calls.Load())
	}
	close(release)
	wg.Wait()
}

func TestUnidentifiedQueueChurnCannotExhaustExactCases(t *testing.T) {
	db, _ := testStore(t)
	s := New(db, nil, nil)
	instance := add(t, s, domain.ProductSonarr)
	ctx := t.Context()
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	unknown := make([]observe.QueueRecord, 400)
	for i := range unknown {
		unknown[i] = observe.QueueRecord{ID: int64(i + 1), Protocol: domain.ProtocolUsenet, ClientLabel: "test", Status: "completed"}
	}
	collect := func(generation uint64, coverage domain.ObservationState, records []observe.QueueRecord) {
		t.Helper()
		if err := db.Write(ctx, func(tx *sql.Tx) error {
			return persistCases(ctx, tx, instance, observe.QueueSnapshot{Records: records}, generation, now, coverage, nil, nil)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The original implementation would persist 2,400 different identities by
	// here, preventing the identified job from getting an available case slot.
	for generation := uint64(1); generation <= 6; generation++ {
		collect(generation, domain.ObservationKnown, unknown)
	}
	exact := observe.QueueRecord{ID: 500, Target: domain.TargetRef{Instance: instance.Ref, Kind: domain.TargetEpisode, ID: 8, SeriesID: 9},
		DownloadID: "stable-test-id", Protocol: domain.ProtocolUsenet, ClientLabel: "test", Status: "completed"}
	records := append(unknown, exact)
	collect(7, domain.ObservationKnown, records)
	page, err := s.Cases(ctx, app.CaseQuery{Limit: 100})
	if err != nil || len(page.Cases) != maxObservationCases+1 {
		t.Fatalf("projection exceeded separate observation-only quota: %d, %v", len(page.Cases), err)
	}
	var exactID domain.CaseID
	for _, item := range page.Cases {
		if item.Job.CompleteSet {
			if len(item.Targets) != 1 || item.Targets[0].Ref != exact.Target {
				t.Fatal("unidentified rows gained complete status")
			}
			exactID = item.ID
		}
	}
	if exactID == "" {
		t.Fatal("unidentified rows starved the exact group")
	}
	collect(8, "partial", nil)
	if unchanged, _ := s.Cases(ctx, app.CaseQuery{Limit: 100}); len(unchanged.Cases) != len(page.Cases) {
		t.Fatal("partial scan removed retained observations")
	}
	collect(9, domain.ObservationKnown, []observe.QueueRecord{exact})
	if current, _ := s.Cases(ctx, app.CaseQuery{Limit: 100}); len(current.Cases) != 1 || current.Cases[0].ID != exactID {
		t.Fatalf("complete scan failed to retire observations without disturbing exact case: %+v", current)
	}
}
