package arrwire_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

const adapterRevision = "fixture-adapter"

func TestEncodePinnedCommands(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		product domain.Product
		action  domain.Action
	}{
		{"sonarr-copy", domain.ProductSonarr, domain.ActionImport},
		{"radarr-copy", domain.ProductRadarr, domain.ActionImport},
		{"sonarr-search", domain.ProductSonarr, domain.ActionSearch},
		{"radarr-search", domain.ProductRadarr, domain.ActionSearch},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			instance, op, prepared := action(fixture.product, fixture.action)
			req, err := arrwire.Encode(instance, adapterRevision, op, prepared)
			if err != nil {
				t.Fatal(err)
			}
			if req.Method() != http.MethodPost || req.Path() != "/api/v3/command" ||
				req.ContentType() != "application/json" || req.RawQuery() != "" {
				t.Fatal("unexpected command request envelope")
			}
			want, err := os.ReadFile("testdata/" + fixture.name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			if !equalJSON(t, req.Body(), want) {
				t.Fatal("command fields differ from fixture")
			}
			body := req.Body()
			body[0] = 'x'
			if req.Body()[0] != '{' {
				t.Fatal("mutating the returned body changed the encoded request")
			}
			for _, display := range []string{fmt.Sprint(req), fmt.Sprintf("%#v", req), fmt.Sprintf("%+v", req)} {
				if strings.Contains(display, "synthetic") || strings.Contains(display, "episodeIds") {
					t.Fatal("request formatting exposes wire values")
				}
			}
		})
	}
}

func TestEncodeRejectFlags(t *testing.T) {
	for _, product := range []domain.Product{domain.ProductSonarr, domain.ProductRadarr} {
		for _, remove := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/remove=%t", product, remove), func(t *testing.T) {
				instance, op, prepared := action(product, domain.ActionReject)
				clientRemoval := "false"
				if remove {
					prepared.Reject.RemoveFromClient = true
					op.Effects = append(op.Effects[:2],
						domain.Effect{Kind: domain.EffectClientRemoval},
						domain.Effect{Kind: domain.EffectDataRemoval})
					clientRemoval = "true"
				}
				req, err := arrwire.Encode(instance, adapterRevision, op, prepared)
				if err != nil {
					t.Fatal(err)
				}
				if req.Method() != http.MethodDelete || req.Path() != "/api/v3/queue/45" ||
					req.ContentType() != "" || len(req.Body()) != 0 {
					t.Fatal("expected one queue row and an empty body")
				}
				want := "blocklist=true&changeCategory=false&removeFromClient=" + clientRemoval + "&skipRedownload=true"
				if req.RawQuery() != want {
					t.Fatal("queue rejection flags differ from fixture")
				}
			})
		}
	}
}

func TestEncodeHoldsChangedBindings(t *testing.T) {
	tests := []struct {
		name   string
		change func(*domain.Instance, *string, *domain.Operation, *domain.PreparedAction)
		err    error
	}{
		{"unsupported current version", func(i *domain.Instance, _ *string, _ *domain.Operation, _ *domain.PreparedAction) {
			i.UpstreamVersion = "4.0.20.3015"
		}, arrwire.ErrUnsupported},
		{"unsupported current product", func(i *domain.Instance, _ *string, _ *domain.Operation, _ *domain.PreparedAction) {
			i.Product = domain.ProductSAB
		}, arrwire.ErrUnsupported},
		{"changed prepared version", func(_ *domain.Instance, _ *string, _ *domain.Operation, p *domain.PreparedAction) {
			p.Version = "4.0.20.3015"
		}, arrwire.ErrBinding},
		{"changed prepared product", func(_ *domain.Instance, _ *string, _ *domain.Operation, p *domain.PreparedAction) {
			p.Product = domain.ProductRadarr
		}, arrwire.ErrBinding},
		{"changed current instance", func(i *domain.Instance, _ *string, _ *domain.Operation, _ *domain.PreparedAction) {
			i.Ref.ID = "another-instance"
		}, arrwire.ErrBinding},
		{"changed current epoch", func(i *domain.Instance, _ *string, _ *domain.Operation, _ *domain.PreparedAction) {
			i.Ref.Epoch++
		}, arrwire.ErrBinding},
		{"changed current connection revision", func(i *domain.Instance, _ *string, _ *domain.Operation, _ *domain.PreparedAction) {
			i.ConnectionRevision++
		}, arrwire.ErrBinding},
		{"changed current adapter revision", func(_ *domain.Instance, rev *string, _ *domain.Operation, _ *domain.PreparedAction) {
			*rev = "another-adapter"
		}, arrwire.ErrBinding},
		{"changed operation key", func(_ *domain.Instance, _ *string, op *domain.Operation, _ *domain.PreparedAction) {
			op.Key = "another-operation"
		}, arrwire.ErrBinding},
		{"changed operation action", func(_ *domain.Instance, _ *string, op *domain.Operation, _ *domain.PreparedAction) {
			op.Action = domain.ActionSearch
		}, arrwire.ErrBinding},
		{"changed operation effect", func(_ *domain.Instance, _ *string, op *domain.Operation, _ *domain.PreparedAction) {
			op.Effects[0].Kind = domain.EffectDataRemoval
		}, arrwire.ErrBinding},
		{"changed file target", func(_ *domain.Instance, _ *string, _ *domain.Operation, p *domain.PreparedAction) {
			p.Import.Files[0].Targets[0].ID++
		}, arrwire.ErrBinding},
		{"missing observed indexer flags", func(_ *domain.Instance, _ *string, _ *domain.Operation, p *domain.PreparedAction) {
			p.Import.Files[0].IndexerFlags = nil
		}, arrwire.ErrBinding},
		{"missing observed quality resolution", func(_ *domain.Instance, _ *string, _ *domain.Operation, p *domain.PreparedAction) {
			p.Import.Files[0].QualityResolution = nil
		}, arrwire.ErrBinding},
		{"missing observed quality revision", func(_ *domain.Instance, _ *string, _ *domain.Operation, p *domain.PreparedAction) {
			p.Import.Files[0].Quality.Observed = false
		}, arrwire.ErrBinding},
		{"unknown indexer flag bit", func(_ *domain.Instance, _ *string, _ *domain.Operation, p *domain.PreparedAction) {
			*p.Import.Files[0].IndexerFlags = 1 << 8
		}, arrwire.ErrNativeShape},
		{"unknown quality source", func(_ *domain.Instance, _ *string, _ *domain.Operation, p *domain.PreparedAction) {
			p.Import.Files[0].QualitySource = "webdl"
		}, arrwire.ErrNativeShape},
		{"out of range quality ID", func(_ *domain.Instance, _ *string, _ *domain.Operation, p *domain.PreparedAction) {
			p.Import.Files[0].QualityID = math.MaxInt32 + 1
		}, arrwire.ErrNativeShape},
		{"out of range target ID", func(_ *domain.Instance, _ *string, op *domain.Operation, p *domain.PreparedAction) {
			op.Targets[0].ID = math.MaxInt32 + 1
			op.Effects[1].Target = op.Targets[0]
			p.Import.Files[0].Targets[1] = op.Targets[0]
		}, arrwire.ErrNativeShape},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance, op, prepared := action(domain.ProductSonarr, domain.ActionImport)
			revision := adapterRevision
			test.change(&instance, &revision, &op, &prepared)
			assertHeld(t, instance, revision, op, prepared, test.err)
		})
	}
}

func TestEncodeHoldsChangedRejectAndSearch(t *testing.T) {
	t.Run("removal without reviewed effects", func(t *testing.T) {
		instance, op, prepared := action(domain.ProductRadarr, domain.ActionReject)
		prepared.Reject.RemoveFromClient = true
		assertHeld(t, instance, adapterRevision, op, prepared, arrwire.ErrBinding)
	})
	t.Run("queue ID overflows native int", func(t *testing.T) {
		instance, op, prepared := action(domain.ProductRadarr, domain.ActionReject)
		prepared.Reject.QueueID = math.MaxInt32 + 1
		assertHeld(t, instance, adapterRevision, op, prepared, arrwire.ErrNativeShape)
	})
	t.Run("reject expects automatic search", func(t *testing.T) {
		instance, op, prepared := action(domain.ProductRadarr, domain.ActionReject)
		op.Effects = append(op.Effects, domain.Effect{Kind: domain.EffectSearchDispatch})
		assertHeld(t, instance, adapterRevision, op, prepared, arrwire.ErrBinding)
	})
	t.Run("search missing reject proof", func(t *testing.T) {
		instance, op, prepared := action(domain.ProductSonarr, domain.ActionSearch)
		prepared.Search.Evidence = nil
		assertHeld(t, instance, adapterRevision, op, prepared, arrwire.ErrBinding)
	})
	t.Run("search scope spans series", func(t *testing.T) {
		instance, op, prepared := action(domain.ProductSonarr, domain.ActionSearch)
		op.Targets[1].SeriesID++
		op.Effects[1].Target = op.Targets[1]
		assertHeld(t, instance, adapterRevision, op, prepared, arrwire.ErrBinding)
	})
	t.Run("movie search scope widened", func(t *testing.T) {
		instance, op, prepared := action(domain.ProductRadarr, domain.ActionSearch)
		target := op.Targets[0]
		target.ID++
		op.Targets = append(op.Targets, target)
		op.Effects = append(op.Effects, domain.Effect{Kind: domain.EffectSearchDispatch, Target: target})
		assertHeld(t, instance, adapterRevision, op, prepared, arrwire.ErrBinding)
	})
}

func assertHeld(t *testing.T, instance domain.Instance, revision string, op domain.Operation, prepared domain.PreparedAction, want error) {
	t.Helper()
	req, err := arrwire.Encode(instance, revision, op, prepared)
	if !errors.Is(err, want) {
		t.Fatalf("expected %v, got %v", want, err)
	}
	if req.Method() != "" || req.Path() != "" || req.RawQuery() != "" || req.ContentType() != "" || len(req.Body()) != 0 {
		t.Fatal("held operation produced an encoded request")
	}
}

func action(product domain.Product, kind domain.Action) (domain.Instance, domain.Operation, domain.PreparedAction) {
	instance := domain.Instance{
		Ref: domain.InstanceRef{ID: "fixture-instance", Epoch: 2}, Product: product,
		UpstreamVersion: arrwire.SonarrVersion, ConnectionRevision: 3,
	}
	op := domain.Operation{
		Key: "fixture-operation", Job: domain.JobRef{Instance: instance.Ref, ID: "fixture-job", Generation: 4},
		Action: kind,
	}
	if product == domain.ProductSonarr {
		for _, id := range []int64{101, 102, 103} {
			op.Targets = append(op.Targets, domain.TargetRef{Instance: instance.Ref, Kind: domain.TargetEpisode, ID: id, SeriesID: 17})
		}
	} else {
		instance.UpstreamVersion = arrwire.RadarrVersion
		op.Targets = []domain.TargetRef{{Instance: instance.Ref, Kind: domain.TargetMovie, ID: 301}}
	}
	prepared := domain.PreparedAction{
		Product: product, Version: instance.UpstreamVersion, AdapterRevision: adapterRevision,
		ConnectionRevision: instance.ConnectionRevision, OperationKey: op.Key,
	}
	switch kind {
	case domain.ActionImport:
		op.Effects = []domain.Effect{{Kind: domain.EffectSourceRetained}}
		for _, target := range op.Targets {
			op.Effects = append(op.Effects, domain.Effect{Kind: domain.EffectLibraryImport, Target: target})
		}
		prepared.Import = &domain.PreparedImport{DownloadID: "synthetic-download", Files: importFiles(product, op.Targets)}
	case domain.ActionReject:
		op.Effects = []domain.Effect{{Kind: domain.EffectQueueRemoval}, {Kind: domain.EffectBlocklist}, {Kind: domain.EffectSourceRetained}}
		prepared.Reject = &domain.PreparedReject{QueueID: 45, DownloadID: "synthetic-download", GrabHistoryID: 65}
	case domain.ActionSearch:
		for _, target := range op.Targets {
			op.Effects = append(op.Effects, domain.Effect{Kind: domain.EffectSearchDispatch, Target: target})
		}
		prepared.Search = &domain.PreparedSearch{ParentReject: "fixture-attempt", Evidence: []domain.EvidenceID{"fixture-reject-proof"}}
	}
	return instance, op, prepared
}

func importFiles(product domain.Product, targets []domain.TargetRef) []domain.PreparedImportFile {
	english := domain.ImportLanguage{ID: 1, Name: "English"}
	french := domain.ImportLanguage{ID: 2, Name: "French"}
	if product == domain.ProductRadarr {
		resolution, flags, group := int64(2160), uint64(2080), ""
		return []domain.PreparedImportFile{{
			Path: `Q:\synthetic\arr-copy\movie.mkv`, FolderName: "synthetic-folder", Targets: targets,
			QualityID: 31, QualityName: "Remux-2160p", QualitySource: "bluray", QualityResolution: &resolution, QualityModifier: "remux",
			Quality:   domain.ImportQualityRevision{Version: 3, IsRepack: true, Observed: true},
			Languages: []domain.ImportLanguage{french}, ReleaseGroup: &group, IndexerFlags: &flags,
		}}
	}
	resolutionA, flagsA, group := int64(1080), uint64(9), "SYNTHETIC"
	resolutionB, flagsB := int64(720), uint64(0)
	return []domain.PreparedImportFile{
		{
			Path: "/synthetic/arr-copy/a.mkv", FolderName: "synthetic-folder", Targets: []domain.TargetRef{targets[1], targets[0]},
			QualityID: 3, QualityName: "WEBDL-1080p", QualitySource: "web", QualityResolution: &resolutionA,
			Quality:   domain.ImportQualityRevision{Version: 2, Real: 1, IsRepack: true, Observed: true},
			Languages: []domain.ImportLanguage{english, french}, ReleaseGroup: &group, IndexerFlags: &flagsA, ReleaseType: "multiEpisode",
		},
		{
			Path: "/synthetic/arr-copy/b.mkv", FolderName: "synthetic-folder", Targets: []domain.TargetRef{targets[2]},
			QualityID: 4, QualityName: "HDTV-720p", QualitySource: "television", QualityResolution: &resolutionB,
			Quality:   domain.ImportQualityRevision{Version: 1, Observed: true},
			Languages: []domain.ImportLanguage{english}, IndexerFlags: &flagsB, ReleaseType: "singleEpisode",
		},
	}
}

func equalJSON(t *testing.T, actual, expected []byte) bool {
	t.Helper()
	var actualValue, expectedValue any
	if err := json.Unmarshal(actual, &actualValue); err != nil {
		t.Fatal("could not parse encoded command")
	}
	if err := json.Unmarshal(expected, &expectedValue); err != nil {
		t.Fatal("could not parse expected fixture")
	}
	return reflect.DeepEqual(actualValue, expectedValue)
}
