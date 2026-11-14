package arradapter_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

var fixtureTime = time.Date(2026, 1, 21, 10, 11, 12, 0, time.UTC)

func TestFixtureDispatchUsesExactPinnedRequestAndAcknowledgement(t *testing.T) {
	for _, specimen := range []struct {
		product domain.Product
		action  domain.Action
		route   arradapter.Route
		name    string
		payload string
	}{
		{domain.ProductSonarr, domain.ActionSearch, arradapter.RouteEpisodeSearch, "EpisodeSearch", `{"name":"EpisodeSearch","episodeIds":[31]}`},
		{domain.ProductRadarr, domain.ActionSearch, arradapter.RouteMovieSearch, "MoviesSearch", `{"name":"MoviesSearch","movieIds":[31]}`},
		{domain.ProductSonarr, domain.ActionImport, arradapter.RouteCopyImport, "ManualImport", ""},
		{domain.ProductRadarr, domain.ActionImport, arradapter.RouteCopyImport, "ManualImport", ""},
	} {
		t.Run(string(specimen.product)+"/"+string(specimen.action), func(t *testing.T) {
			cfg, op, prepared := fixture(specimen.product, specimen.action)
			sent, qualified := 0, 0
			qualify := func(ctx context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
				qualified++
				assertBinding(t, cfg, specimen.route, binding)
				if _, bounded := ctx.Deadline(); !bounded {
					t.Fatal("qualification lacks a deadline")
				}
				return positive(ctx, binding)
			}
			adapter := newAdapter(t, cfg, func(req *http.Request) (*http.Response, error) {
				sent++
				if qualified != 1 || req.Method != http.MethodPost ||
					req.URL.String() != "https://arr.example.invalid:8443/arr/staging/api/v3/command" ||
					req.Header.Get("X-Api-Key") != cfg.APIKey || req.Header.Get("Accept") != "application/json" ||
					req.Header.Get("Content-Type") != "application/json" || req.GetBody != nil || !req.Close {
					t.Fatal("unexpected fixture request envelope")
				}
				if _, bounded := req.Context().Deadline(); !bounded {
					t.Fatal("exchange lacks a deadline")
				}
				payload, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				if specimen.payload != "" && string(payload) != specimen.payload {
					t.Fatal("targeted search payload differs from fixture")
				}
				if specimen.action == domain.ActionImport && !strings.Contains(string(payload), `"importMode":"copy"`) {
					t.Fatal("copy mode missing")
				}
				reply := response(http.StatusCreated, "application/json; charset=utf-8", commandReply(specimen.name, 412))
				reply.Header.Set("Location", "https://untrusted.example.invalid/api/v3/command/900")
				return reply, nil
			}, qualify, unexpectedReadback(t), fixedTime)
			ack, err := adapter.DispatchPrepared(t.Context(), op, prepared)
			if err != nil || ack != (domain.Acknowledgement{State: domain.DispatchAcknowledged, UpstreamReference: "412"}) {
				t.Fatalf("unexpected acknowledgement: %+v, %v", ack, err)
			}
			if sent != 1 || qualified != 1 {
				t.Fatalf("got %d exchanges and %d qualification checks", sent, qualified)
			}
			if _, err := adapter.Dispatch(t.Context(), op); !errors.Is(err, arradapter.ErrPrepared) || sent != 1 {
				t.Fatal("unprepared interface reached exchange")
			}
		})
	}
}

func TestFixtureQueueRoutesRequireSeparateQualification(t *testing.T) {
	for _, destructive := range []bool{false, true} {
		t.Run(strconv.FormatBool(destructive), func(t *testing.T) {
			cfg, op, prepared := fixture(domain.ProductRadarr, domain.ActionReject)
			expected := arradapter.RouteRejectPreserve
			if destructive {
				expected = arradapter.RouteRejectRemove
				prepared.Reject.RemoveFromClient = true
				op.Effects = append(op.Effects[:2],
					domain.Effect{Kind: domain.EffectClientRemoval},
					domain.Effect{Kind: domain.EffectDataRemoval})
			}
			sent, read := 0, 0
			adapter := newAdapter(t, cfg, func(req *http.Request) (*http.Response, error) {
				sent++
				if req.Method != http.MethodDelete || req.URL.Path != "/arr/staging/api/v3/queue/45" ||
					req.URL.RawQuery != "blocklist=true&changeCategory=false&removeFromClient="+strconv.FormatBool(destructive)+"&skipRedownload=true" ||
					req.GetBody != nil || !req.Close || req.ContentLength != 0 || req.Header.Get("Content-Type") != "" {
					t.Fatal("queue request differs from fixture")
				}
				return response(http.StatusOK, "", ""), nil
			}, func(ctx context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
				assertBinding(t, cfg, expected, binding)
				q, err := positive(ctx, binding)
				if destructive && sent == 0 && read == 0 {
					// A preserve qualification cannot authorise removing client data.
					q.Binding.Route = arradapter.RouteRejectPreserve
					read++
				}
				return q, err
			}, func(_ context.Context, route arradapter.Route, attempt domain.Attempt) (domain.Outcome, error) {
				if attempt.Operation.Key != op.Key || route != expected {
					t.Fatal("readback received the wrong scope")
				}
				return domain.Outcome{AttemptID: attempt.ID, ObservedAt: fixtureTime, Verification: domain.VerificationUnresolved}, nil
			}, fixedTime)
			if destructive {
				if ack, err := adapter.DispatchPrepared(t.Context(), op, prepared); ack != (domain.Acknowledgement{}) ||
					!errors.Is(err, arradapter.ErrQualification) || sent != 0 {
					t.Fatal("different route qualification permitted a destructive request")
				}
			}
			ack, err := adapter.DispatchPrepared(t.Context(), op, prepared)
			if err != nil || ack != (domain.Acknowledgement{State: domain.DispatchAcknowledged}) || sent != 1 {
				t.Fatalf("unexpected queue acknowledgement: %+v, %v", ack, err)
			}
			result, err := adapter.Reconcile(t.Context(), domain.Attempt{
				ID: "fixture-attempt", Operation: op, Prepared: &prepared, SentAt: fixtureTime, Acknowledgement: ack,
			})
			if err != nil || result.Verification != domain.VerificationUnresolved || len(result.Effects) != 0 || sent != 1 {
				t.Fatal("queue acknowledgement changed effect state or triggered another send")
			}
		})
	}
}

func TestFixtureDispatchHoldsAbsentStaleAndMismatchedQualification(t *testing.T) {
	for name, change := range map[string]func(*arradapter.Qualification){
		"absent":       func(q *arradapter.Qualification) { *q = arradapter.Qualification{} },
		"unverified":   func(q *arradapter.Qualification) { q.Verified = false },
		"no evidence":  func(q *arradapter.Qualification) { q.Evidence = "" },
		"expired":      func(q *arradapter.Qualification) { q.ExpiresAt = fixtureTime },
		"future":       func(q *arradapter.Qualification) { q.ObservedAt = fixtureTime.Add(time.Millisecond) },
		"other inst":   func(q *arradapter.Qualification) { q.Binding.Instance.ID = "other" },
		"other epoch":  func(q *arradapter.Qualification) { q.Binding.Instance.Epoch++ },
		"other config": func(q *arradapter.Qualification) { q.Binding.ConnectionRevision++ },
		"other app":    func(q *arradapter.Qualification) { q.Binding.Product = domain.ProductRadarr },
		"other build":  func(q *arradapter.Qualification) { q.Binding.Version = arrwire.RadarrVersion },
		"other wire":   func(q *arradapter.Qualification) { q.Binding.AdapterRevision += "-new" },
		"other scope":  func(q *arradapter.Qualification) { q.Binding.SettingsFingerprint[0]++ },
		"other route":  func(q *arradapter.Qualification) { q.Binding.Route = arradapter.RouteCopyImport },
	} {
		t.Run(name, func(t *testing.T) {
			cfg, op, prepared := fixture(domain.ProductSonarr, domain.ActionSearch)
			adapter := newAdapter(t, cfg, unexpectedExchange(t), func(ctx context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
				q, err := positive(ctx, binding)
				change(&q)
				return q, err
			}, unexpectedReadback(t), fixedTime)
			ack, err := adapter.DispatchPrepared(t.Context(), op, prepared)
			if !errors.Is(err, arradapter.ErrQualification) || ack != (domain.Acknowledgement{}) {
				t.Fatalf("unexpected dispatch result: %+v, %v", ack, err)
			}
		})
	}

	t.Run("expires before the exchange", func(t *testing.T) {
		cfg, op, prepared := fixture(domain.ProductSonarr, domain.ActionSearch)
		clockChecks := 0
		now := func() time.Time {
			clockChecks++
			if clockChecks == 1 {
				return fixtureTime
			}
			return fixtureTime.Add(time.Minute)
		}
		adapter := newAdapter(t, cfg, unexpectedExchange(t), positive, unexpectedReadback(t), now)
		if _, err := adapter.DispatchPrepared(t.Context(), op, prepared); !errors.Is(err, arradapter.ErrQualification) {
			t.Fatalf("expected expired qualification, got %v", err)
		}
	})

	t.Run("stale preparation after connection change", func(t *testing.T) {
		cfg, op, prepared := fixture(domain.ProductSonarr, domain.ActionSearch)
		cfg.Instance.ConnectionRevision++
		adapter := newAdapter(t, cfg, unexpectedExchange(t), func(context.Context, arradapter.Binding) (arradapter.Qualification, error) {
			t.Fatal("changed connection reached qualification")
			return arradapter.Qualification{}, nil
		}, unexpectedReadback(t), fixedTime)
		if _, err := adapter.DispatchPrepared(t.Context(), op, prepared); !errors.Is(err, arrwire.ErrBinding) {
			t.Fatalf("expected changed binding, got %v", err)
		}
	})

	t.Run("qualification backend error", func(t *testing.T) {
		cfg, op, prepared := fixture(domain.ProductSonarr, domain.ActionSearch)
		adapter := newAdapter(t, cfg, unexpectedExchange(t), func(ctx context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
			q, _ := positive(ctx, binding)
			return q, errors.New("private settings-source detail")
		}, unexpectedReadback(t), fixedTime)
		if _, err := adapter.DispatchPrepared(t.Context(), op, prepared); err != arradapter.ErrQualification {
			t.Fatalf("expected qualification error, got %v", err)
		}
	})
}

func TestFixtureAdapterRejectsAmbiguousBaseAndMissingInputs(t *testing.T) {
	for _, raw := range []string{
		"", "/arr", "//arr.example.invalid", "ftp://arr.example.invalid/arr", "https:///arr",
		"https://user:secret@arr.example.invalid/arr", "https://arr.example.invalid/arr?", "https://arr.example.invalid/arr?x=",
		"https://arr.example.invalid/arr#", "https://arr.example.invalid/arr#x", "https://arr.example.invalid//evil",
		"https://arr.example.invalid/arr//", "https://arr.example.invalid/arr/./api", "https://arr.example.invalid/arr/../api",
		"https://arr.example.invalid/%2e%2e/api", "https://arr.example.invalid/arr%2fapi", "https://arr.example.invalid/arr%252fapi",
		"https://arr.example.invalid/arr\\api", "https://arr.example.invalid/arr;api", "https://arr.example.invalid/arr%00",
	} {
		t.Run(raw, func(t *testing.T) {
			cfg, _, _ := fixture(domain.ProductRadarr, domain.ActionSearch)
			cfg.Instance.BaseURL = raw
			if _, err := arradapter.NewFixture(cfg, unexpectedExchange(t), positive, unexpectedReadback(t), fixedTime); err != arradapter.ErrConfiguration {
				t.Fatalf("expected invalid configuration for base, got %v", err)
			}
		})
	}
	cfg, _, _ := fixture(domain.ProductRadarr, domain.ActionSearch)
	transport, readback := unexpectedExchange(t), unexpectedReadback(t)
	for _, err := range []error{
		newAdapterError(cfg, nil, positive, readback, fixedTime),
		newAdapterError(cfg, transport, nil, readback, fixedTime),
		newAdapterError(cfg, transport, positive, nil, fixedTime),
		newAdapterError(cfg, transport, positive, readback, nil),
	} {
		if err != arradapter.ErrConfiguration {
			t.Fatal("absent injected dependency was accepted")
		}
	}
	for _, changed := range []func(*arradapter.Config){
		func(cfg *arradapter.Config) { cfg.APIKey = "bad\r\nkey" },
		func(cfg *arradapter.Config) { cfg.APIKey = "" },
		func(cfg *arradapter.Config) { cfg.AdapterRevision = "" },
		func(cfg *arradapter.Config) { cfg.SettingsFingerprint = [sha256.Size]byte{} },
		func(cfg *arradapter.Config) { cfg.Instance.ConnectionRevision = 0 },
		func(cfg *arradapter.Config) { cfg.Instance.UpstreamVersion = "fixture-unknown-version" },
	} {
		invalid := cfg
		changed(&invalid)
		if err := newAdapterError(invalid, transport, positive, readback, fixedTime); err != arradapter.ErrConfiguration {
			t.Fatal("invalid configuration was accepted")
		}
	}
}

func fixture(product domain.Product, action domain.Action) (arradapter.Config, domain.Operation, domain.PreparedAction) {
	instance := domain.Instance{
		Ref: domain.InstanceRef{ID: "fixture-instance", Epoch: 2}, Product: product, BaseURL: "https://arr.example.invalid:8443/arr/staging/",
		UpstreamVersion: arrwire.SonarrVersion, ConnectionRevision: 3,
	}
	target := domain.TargetRef{Instance: instance.Ref, Kind: domain.TargetEpisode, ID: 31, SeriesID: 17}
	if product == domain.ProductRadarr {
		instance.UpstreamVersion = arrwire.RadarrVersion
		target.Kind = domain.TargetMovie
		target.SeriesID = 0
	}
	cfg := arradapter.Config{Instance: instance, AdapterRevision: "fixture-adapter", APIKey: "synthetic-key",
		SettingsFingerprint: sha256.Sum256([]byte("fixture-settings"))}
	op := domain.Operation{
		Key: "fixture-operation", Job: domain.JobRef{Instance: instance.Ref, ID: "fixture-job", Generation: 4},
		Targets: []domain.TargetRef{target}, Action: action,
	}
	prepared := domain.PreparedAction{Product: product, Version: instance.UpstreamVersion, AdapterRevision: cfg.AdapterRevision,
		ConnectionRevision: instance.ConnectionRevision, OperationKey: op.Key}
	switch action {
	case domain.ActionImport:
		qualitySource, modifier, releaseType := "web", "", "singleEpisode"
		if product == domain.ProductRadarr {
			qualitySource, modifier, releaseType = "webdl", "none", ""
		}
		resolution, flags := int64(1080), uint64(0)
		op.Effects = []domain.Effect{{Kind: domain.EffectLibraryImport, Target: target}, {Kind: domain.EffectSourceRetained}}
		prepared.Import = &domain.PreparedImport{DownloadID: "synthetic-download", Files: []domain.PreparedImportFile{{
			Path: "/synthetic/fixture.mkv", FolderName: "synthetic", Targets: []domain.TargetRef{target}, QualityID: 3,
			QualityName: "WEBDL-1080p", QualitySource: qualitySource, QualityModifier: modifier, QualityResolution: &resolution,
			Quality: domain.ImportQualityRevision{Version: 1, Observed: true}, Languages: []domain.ImportLanguage{{ID: 1, Name: "English"}},
			IndexerFlags: &flags, ReleaseType: releaseType,
		}}}
	case domain.ActionReject:
		op.Effects = []domain.Effect{{Kind: domain.EffectQueueRemoval}, {Kind: domain.EffectBlocklist}, {Kind: domain.EffectSourceRetained}}
		prepared.Reject = &domain.PreparedReject{QueueID: 45, DownloadID: "synthetic-download", GrabHistoryID: 65}
	case domain.ActionSearch:
		op.Effects = []domain.Effect{{Kind: domain.EffectSearchDispatch, Target: target}}
		prepared.Search = &domain.PreparedSearch{ParentReject: "fixture-parent", Evidence: []domain.EvidenceID{"evidence-exact-reject"}}
	}
	return cfg, op, prepared
}

func assertBinding(t *testing.T, cfg arradapter.Config, route arradapter.Route, got arradapter.Binding) {
	t.Helper()
	want := arradapter.Binding{
		Instance: cfg.Instance.Ref, Product: cfg.Instance.Product, Version: cfg.Instance.UpstreamVersion,
		ConnectionRevision: cfg.Instance.ConnectionRevision, AdapterRevision: cfg.AdapterRevision,
		SettingsFingerprint: cfg.SettingsFingerprint, Route: route,
	}
	if got != want {
		t.Fatalf("qualification requested for the wrong binding: %+v", got)
	}
}

func fixedTime() time.Time { return fixtureTime }

func positive(_ context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
	return arradapter.Qualification{
		Binding: binding, Verified: true, Evidence: "fixture-qualification",
		ObservedAt: fixtureTime.Add(-time.Second), ExpiresAt: fixtureTime.Add(time.Second),
	}, nil
}

func newAdapter(t *testing.T, cfg arradapter.Config, exchange arradapter.FixtureTransport, qualify arradapter.Qualify,
	readback arradapter.Readback, now func() time.Time) *arradapter.Adapter {
	t.Helper()
	adapter, err := arradapter.NewFixture(cfg, exchange, qualify, readback, now)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func newAdapterError(cfg arradapter.Config, exchange arradapter.FixtureTransport, qualify arradapter.Qualify,
	readback arradapter.Readback, now func() time.Time) error {
	_, err := arradapter.NewFixture(cfg, exchange, qualify, readback, now)
	return err
}

func unexpectedExchange(t *testing.T) arradapter.FixtureTransport {
	return func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected exchange")
		return nil, nil
	}
}

func unexpectedReadback(t *testing.T) arradapter.Readback {
	return func(context.Context, arradapter.Route, domain.Attempt) (domain.Outcome, error) {
		t.Fatal("unexpected readback")
		return domain.Outcome{}, nil
	}
}

func response(status int, kind, body string) *http.Response {
	r := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body))}
	if kind != "" {
		r.Header.Set("Content-Type", kind)
	}
	return r
}

func commandReply(name string, id int) string {
	return `{"id":` + strconv.Itoa(id) + `,"name":"` + name + `","commandName":"Fixture display name","status":"queued","result":"unknown","body":{"commandData":"synthetic"},"queued":"2026-01-21T10:11:12Z"}`
}
