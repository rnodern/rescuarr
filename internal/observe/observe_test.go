package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Body: io.NopCloser(strings.NewReader(body)),
		Header: http.Header{"Content-Type": {"application/json; charset=utf-8"}},
	}
}

var observedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func configuration(product domain.Product) Config {
	return Config{Instance: domain.Instance{
		Ref: domain.InstanceRef{ID: "fixture-instance", Epoch: 2}, Product: product,
		BaseURL: "https://fixture.invalid/namespace/",
	}, APIKey: "synthetic-api-key"}
}

func arrFor(t *testing.T, product domain.Product, transport roundTrip) *Arr {
	t.Helper()
	a, err := NewArr(configuration(product), transport)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestArrStatusAndQueue(t *testing.T) {
	for _, tc := range []struct {
		product domain.Product
		app     string
		version string
		flag    string
		target  string
		kind    domain.TargetKind
	}{
		{domain.ProductSonarr, "Sonarr", "4.0.20.3014", "includeUnknownSeriesItems", `"seriesId":12,"episodeId":41`, domain.TargetEpisode},
		{domain.ProductRadarr, "Radarr", "6.4.4.10685", "includeUnknownMovieItems", `"movieId":41`, domain.TargetMovie},
	} {
		t.Run(tc.app, func(t *testing.T) {
			a := arrFor(t, tc.product, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Host != "fixture.invalid" || r.Header.Get("X-Api-Key") != "synthetic-api-key" || r.Body != nil {
					t.Fatalf("unexpected Arr request method, origin or key delivery: %s %s", r.Method, r.URL.Host)
				}
				switch r.URL.Path {
				case "/namespace/api/v3/system/status":
					return response(http.StatusOK, fmt.Sprintf(`{"appName":%q,"version":%q,"osName":"alpine"}`, tc.app, tc.version)), nil
				case "/namespace/api/v3/queue":
					want := url.Values{tc.flag: {"true"}, "page": {"1"}, "pageSize": {"100"}, "sortKey": {"added"}, "sortDirection": {"descending"}}
					if tc.product == domain.ProductSonarr {
						want.Set("includeSeries", "true")
						want.Set("includeEpisode", "true")
					} else {
						want.Set("includeMovie", "true")
					}
					if !reflect.DeepEqual(r.URL.Query(), want) {
						t.Fatalf("query mismatch: %v", r.URL.Query())
					}
					return response(http.StatusOK, fmt.Sprintf(`{"page":1,"pageSize":100,"totalRecords":2,"records":[
						{"id":1001,%s,"downloadId":"SABnzbd_nzo_labpack","protocol":"usenet","outputPath":"/synthetic/downloads","trackedDownloadState":"importPending","downloadClient":"LAB SAB","statusMessages":[{"title":"ignored filename","messages":["No files found are eligible for import","raw context"]}]},
						{"id":1002,"downloadId":null,"protocol":"future","status":"newThingFromUpstream","size":null}
					]}`, tc.target)), nil
				default:
					t.Fatalf("unexpected path %q", r.URL.Path)
					return nil, nil
				}
			})
			got, err := a.Status(t.Context())
			if err != nil || got.Product != tc.product || got.Version != tc.version {
				t.Fatalf("status %+v: %v", got, err)
			}
			q, err := a.Queue(t.Context(), 4, observedAt)
			if err != nil || len(q.Records) != 2 || !q.Evidence.Fresh(observedAt) || q.Evidence.SnapshotGeneration != 4 || q.Evidence.ID == "" || q.Evidence.ContentHash == "" || q.Evidence.Scope.Instance != configuration(tc.product).Instance.Ref {
				t.Fatalf("queue %+v: %v", q, err)
			}
			first, unknown := q.Records[0], q.Records[1]
			if first.Target.Kind != tc.kind || !first.Target.Valid() || first.TrackedState != "importPending" || first.Protocol != domain.ProtocolUsenet || !reflect.DeepEqual(first.Findings, []QueueFinding{FindingNoEligibleFiles, FindingOtherMessage}) {
				t.Fatalf("positive row %+v", first)
			}
			if unknown.Target.Valid() || unknown.DownloadID != "" || unknown.Protocol != domain.ProtocolUnknown || unknown.Status != "unknown" || unknown.SizeBytes != nil {
				t.Fatalf("invented identity or facts %+v", unknown)
			}
			encoded, _ := json.Marshal(q)
			for _, omitted := range []string{"synthetic/downloads", "raw context", "ignored filename", "synthetic-api-key"} {
				if strings.Contains(string(encoded), omitted) {
					t.Fatalf("serialised protected field %q", omitted)
				}
			}
		})
	}
}

func TestQueueNoEligibleFilesWithPrivatePath(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		want         QueueFinding
	}{
		{"windows", ` in C:\Synthetic\Private Transfer\Season Pack`, FindingNoEligibleFiles},
		{"unc", ` in \\synthetic-host\private-transfer\season-pack`, FindingNoEligibleFiles},
		{"unix", " in /synthetic/private-transfer/season-pack", FindingNoEligibleFiles},
		{"bare", ".", FindingNoEligibleFiles},
		{"empty-path", " in ", FindingOtherMessage},
		{"similar-warning", " in this particular situation", FindingOtherMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := arrFor(t, domain.ProductSonarr, func(r *http.Request) (*http.Response, error) {
				body := fmt.Sprintf(`{"page":1,"pageSize":100,"totalRecords":1,"records":[
					{"id":1001,"seriesId":12,"episodeId":41,"statusMessages":[{"messages":[%q]}]}]}`,
					"No files found are eligible for import"+tc.suffix)
				return response(http.StatusOK, body), nil
			})
			snapshot, err := a.Queue(t.Context(), 1, observedAt)
			if err != nil || len(snapshot.Records) != 1 {
				t.Fatalf("queue read yielded %d records: %v", len(snapshot.Records), err)
			}
			if got := snapshot.Records[0].Findings; !reflect.DeepEqual(got, []QueueFinding{tc.want}) {
				t.Fatalf("findings %v, want %s", got, tc.want)
			}
			jsonSnapshot, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.ToLower(string(jsonSnapshot)), "private") || strings.Contains(string(jsonSnapshot), "eligible for import") {
				t.Fatal("raw queue warning or its private path appeared in the snapshot")
			}
		})
	}
}

func TestSonarrQueueKeepsSeriesHintForUnmappedRows(t *testing.T) {
	a := arrFor(t, domain.ProductSonarr, func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"page":1,"pageSize":100,"totalRecords":5,"records":[
			{"id":1,"seriesId":18,"episodeId":19,"series":{"id":18}},
			{"id":2,"seriesId":18},
			{"id":3,"episodeId":19,"series":{"id":18}},
			{"id":4,"seriesId":18,"episodeId":19,"series":{"id":20}},
			{"id":5,"seriesId":18,"episodeId":19,"episode":{"id":19,"seriesId":20}}
		]}`), nil
	})
	queue, err := a.Queue(t.Context(), 1, observedAt)
	if err != nil || len(queue.Records) != 5 {
		t.Fatalf("queue read returned %d records: %v", len(queue.Records), err)
	}
	for i, want := range []struct {
		series int64
		exact  bool
	}{{18, true}, {18, false}, {18, false}, {0, false}, {0, false}} {
		row := queue.Records[i]
		if row.SeriesID != want.series || row.Target.Valid() != want.exact {
			t.Fatalf("row %d has unexpected series or target: %+v", i+1, row)
		}
	}
	encoded, err := json.Marshal(queue.Records[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"SeriesID":18`) {
		t.Fatal("unmapped series hint entered serialised queue evidence")
	}
}

func TestQueuePagingHoldsInconsistentScans(t *testing.T) {
	for _, test := range []struct {
		name    string
		second  string
		moving  string
		wantErr error
		wantLen int
	}{
		{"stable", `{"id":101}`, "", nil, 101},
		{"duplicate-window", `{"id":1}`, "", ErrPartial, 100},
		{"empty-window", "", "", ErrPartial, 100},
		{"moving-head", `{"id":101}`, "1", ErrPartial, 101},
		{"moving-tail", `{"id":101}`, "2", ErrPartial, 101},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := make(map[string]int)
			a := arrFor(t, domain.ProductSonarr, func(r *http.Request) (*http.Response, error) {
				page := r.URL.Query().Get("page")
				calls[page]++
				rows := test.second
				if page == "1" {
					ids := make([]string, 100)
					for i := range ids {
						ids[i] = fmt.Sprintf(`{"id":%d}`, i+1)
					}
					if calls[page] > 1 && test.moving == page {
						ids[0] = `{"id":1,"status":"completed"}`
					}
					rows = strings.Join(ids, ",")
				} else if calls[page] > 1 && test.moving == page {
					rows = `{"id":102}`
				}
				return response(http.StatusOK, fmt.Sprintf(`{"page":%s,"pageSize":100,"totalRecords":101,"records":[%s]}`, page, rows)), nil
			})
			q, err := a.Queue(t.Context(), 1, observedAt)
			if !errors.Is(err, test.wantErr) || len(q.Records) != test.wantLen || q.Evidence.Complete != (test.wantErr == nil) || q.Evidence.Fresh(observedAt) != (test.wantErr == nil) {
				t.Fatalf("records %d, evidence %+v, err %v", len(q.Records), q.Evidence, err)
			}
			if test.wantErr == nil && (calls["1"] != 2 || calls["2"] != 2) {
				t.Fatalf("both pages should be confirmed: %v", calls)
			}
		})
	}
}

func TestTransportRejectsUnsafeConfigurationAndResponses(t *testing.T) {
	for _, address := range []string{"ftp://fixture.invalid", "https://user:credential@fixture.invalid", "https://fixture.invalid?", "https://fixture.invalid#", "https://fixture.invalid/a/../b", "https://fixture.invalid/%2fapi"} {
		cfg := configuration(domain.ProductSonarr)
		cfg.Instance.BaseURL = address
		if _, err := NewArr(cfg, nil); !errors.Is(err, ErrConfiguration) || strings.Contains(err.Error(), "credential@") {
			t.Fatalf("unexpected configuration result: %v", err)
		}
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"login", 200, `<html>login</html>`, ErrResponse},
		{"trailing", 200, `{} {}`, ErrResponse},
		{"oversize", 200, strings.Repeat(" ", maxBody+1), ErrResponse},
		{"identity", 200, `{"appName":"Radarr","version":"6.4.4.10685"}`, ErrResponse},
		{"access", 403, `{"error":"key=synthetic-api-key"}`, ErrAccess},
		{"redirect", 302, ``, ErrUnavailable},
		{"rate-limit", 429, ``, ErrRateLimited},
		{"upstream-down", 503, ``, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			a := arrFor(t, domain.ProductSonarr, func(*http.Request) (*http.Response, error) {
				calls++
				r := response(tc.status, tc.body)
				r.Header.Set("Location", "https://unexpected.invalid/key")
				return r, nil
			})
			if _, err := a.Status(t.Context()); !errors.Is(err, tc.want) || calls != 1 || strings.Contains(err.Error(), "synthetic-api-key") {
				t.Fatalf("calls %d, error %v", calls, err)
			}
		})
	}
}

func TestArrTargetScopedReads(t *testing.T) {
	a := arrFor(t, domain.ProductSonarr, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/namespace/api/v3/queue/details":
			if r.URL.RawQuery != "episodeIds=41" {
				t.Fatalf("details query %q", r.URL.RawQuery)
			}
			return response(200, `[{"id":1001,"seriesId":12,"episodeId":41}]`), nil
		case "/namespace/api/v3/history":
			if r.URL.Query().Get("downloadId") != "SABnzbd_nzo_labpack" || r.URL.Query().Get("pageSize") != "50" {
				t.Fatalf("history query %q", r.URL.RawQuery)
			}
			return response(200, `{"page":1,"pageSize":50,"totalRecords":2,"records":[{"id":203,"seriesId":12,"episodeId":41,"downloadId":"SABnzbd_nzo_labpack","eventType":"grabbed","data":{"indexer":"LAB","downloadUrl":"https://hidden.invalid/api?apikey=discard"}},{"id":204,"seriesId":12,"episodeId":41,"downloadId":"different-job","eventType":"grabbed"}]}`), nil
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
			return nil, nil
		}
	})
	ref := domain.TargetRef{Instance: configuration(domain.ProductSonarr).Instance.Ref, Kind: domain.TargetEpisode, ID: 41, SeriesID: 12}
	if rows, err := a.QueueDetails(t.Context(), ref); err != nil || len(rows) != 1 || rows[0].Target != ref {
		t.Fatalf("queue details %+v: %v", rows, err)
	}
	foreign := ref
	foreign.Instance.Epoch++
	if _, err := a.QueueDetails(t.Context(), foreign); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("cross-epoch target: %v", err)
	}
	h, err := a.History(t.Context(), "SABnzbd_nzo_labpack", 1, observedAt)
	if !errors.Is(err, ErrPartial) || h.Evidence.Complete || len(h.Records) != 1 || h.Records[0].Event != "grabbed" || h.Records[0].Target != ref {
		t.Fatalf("history %+v: %v", h, err)
	}
	encoded, _ := json.Marshal(h)
	if strings.Contains(string(encoded), "hidden.invalid") {
		t.Fatal("provider URL was retained")
	}
}

func TestOptionalSABScopeAndMissingHistory(t *testing.T) {
	cfg := configuration(domain.ProductSAB)
	calls := 0
	s, err := NewSAB(cfg, roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/namespace/api" || r.Header.Get("X-Api-Key") != "" {
			t.Fatalf("unexpected SAB request")
		}
		if q.Get("mode") == "version" {
			if q.Has("apikey") {
				t.Fatal("SAB version probe received the API key")
			}
			return response(200, `{"version":"5.1.3"}`), nil
		}
		if len(q) != 6 || q.Get("mode") != "history" || q.Get("nzo_ids") != "SABnzbd_nzo_labmovie,SABnzbd_nzo_missing" || q.Get("start") != "0" || q.Get("limit") != "2" || q.Get("apikey") != cfg.APIKey {
			t.Fatal("incorrect scoped SAB history request")
		}
		return response(200, `{"history":{"slots":[{"nzo_id":"SABnzbd_nzo_labmovie","status":"Failed","category":"radarr-lab","fail_message":"raw failure details","bytes":280040}]}}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.Version(t.Context()); err != nil || v != "5.1.3" {
		t.Fatalf("version %q: %v", v, err)
	}
	jobs, err := s.History(t.Context(), "SABnzbd_nzo_labmovie", "SABnzbd_nzo_missing")
	if err != nil || len(jobs) != 1 || jobs[0].State != SABFailed || jobs[0].Instance != cfg.Instance.Ref || jobs[0].Bytes == nil || *jobs[0].Bytes != 280040 {
		t.Fatalf("history %+v: %v", jobs, err)
	}
	if _, err := s.Queue(t.Context(), "all,SABnzbd_nzo_labmovie"); !errors.Is(err, ErrConfiguration) || calls != 2 {
		t.Fatalf("unscoped selector dispatched: %v, calls %d", err, calls)
	}
}

func TestSentTransportErrorIsRedacted(t *testing.T) {
	s, err := NewSAB(configuration(domain.ProductSAB), roundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("synthetic-api-key via verbose proxy error")
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Queue(context.Background(), "SABnzbd_nzo_labmovie")
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "synthetic-api-key") {
		t.Fatalf("unsafe transport error: %v", err)
	}
}

func TestSABQueueUsesCategoryAndLeavesExactSizeUnknown(t *testing.T) {
	s, err := NewSAB(configuration(domain.ProductSAB), roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("mode") != "queue" {
			t.Fatal("unexpected SAB mode")
		}
		return response(200, `{"queue":{"slots":[{"nzo_id":"SABnzbd_nzo_labmovie","cat":"radarr-lab","status":"Downloading","mb":"104.1","size":"104.1 MB","category":"ignored","bytes":104}]}}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.Queue(t.Context(), "SABnzbd_nzo_labmovie")
	if err != nil || len(jobs) != 1 || jobs[0].Category != "radarr-lab" || jobs[0].Bytes != nil || jobs[0].State != SABActive {
		t.Fatalf("queue %+v: %v", jobs, err)
	}
}
