package replacement

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

func syntheticSearch(product domain.Product) SearchInput {
	reject := syntheticRejection(product, false)
	o := reject.Origin
	sentAt := reject.Now.Add(2 * time.Second)
	at := sentAt.Add(20 * time.Second)
	input := SearchInput{
		Now: at.Add(time.Second), Origin: o,
		Attempt: domain.Attempt{
			ID: "search", SentAt: sentAt, Acknowledgement: domain.Acknowledgement{State: domain.DispatchAcknowledged, UpstreamReference: "191"},
			Operation: domain.Operation{Action: domain.ActionSearch, Key: "search-original-targets", Job: o.Job.Ref, Targets: slices.Clone(o.Targets)},
		},
	}
	name := "EpisodeSearch"
	if product == domain.ProductRadarr {
		name = "MoviesSearch"
	}
	input.Command = domain.Fact[SearchCommand]{
		Value:       SearchCommand{Reference: "191", Name: name, Targets: slices.Clone(o.Targets), State: CommandCompleted},
		Observation: arrRead(o, "command", CommandReadback, domain.ObservationKnown, "191", at),
	}
	for i, ref := range o.Targets {
		input.Attempt.Operation.Effects = append(input.Attempt.Operation.Effects, domain.Effect{Kind: domain.EffectSearchDispatch, Target: ref})
		id, record := int64(i+600), fmt.Sprintf("%s:%d", ref.Kind, ref.ID)
		grab := Grab{
			Record: Record{ID: id, DownloadID: "SABnzbd_nzo_newrelease", Target: ref, At: sentAt.Add(2 * time.Second),
				Signature: ReleaseSignature{TitleFingerprint: "new-title", QualityFingerprint: "web-1080p", IndexerID: 9, Protocol: domain.ProtocolUsenet}},
			Job:     domain.JobRef{Instance: o.Job.Ref.Instance, ID: "new-job", Generation: 1},
			Release: domain.ReleaseIdentity{GrabHistoryID: id, IndexerID: 9, GUID: "new-release"},
			Profile: 12, Eligible: true,
		}
		imported := grab.Record
		imported.ID += 100
		imported.At = sentAt.Add(6 * time.Second)
		fileID := int64(i + 800)
		input.Targets = append(input.Targets, SearchTarget{
			Target:        ref,
			Profile:       domain.Fact[domain.Revision]{Value: grab.Profile, Observation: arrRead(o, domain.EvidenceID(record+"/profile"), ReplacementProfile, domain.ObservationKnown, record, at)},
			CurrentFileID: domain.Fact[int64]{Value: fileID, Observation: arrRead(o, domain.EvidenceID(record+"/file"), ReplacementFile, domain.ObservationKnown, record, at)},
			Grabs:         domain.Fact[[]Grab]{Value: []Grab{grab}, Observation: arrRead(o, domain.EvidenceID(record+"/grab"), ReplacementGrab, domain.ObservationKnown, record, at)},
			Imports:       domain.Fact[[]Import]{Value: []Import{{Record: imported, FileID: fileID}}, Observation: arrRead(o, domain.EvidenceID(record+"/import"), ReplacementImport, domain.ObservationKnown, grab.DownloadID, at)},
		})
	}
	return input
}

func TestTrackSearchScopedImportForBothArrs(t *testing.T) {
	for _, product := range []domain.Product{domain.ProductSonarr, domain.ProductRadarr} {
		t.Run(string(product), func(t *testing.T) {
			in := syntheticSearch(product)
			out := TrackSearch(in)
			if out.Command != CommandCompleted || len(out.Targets) != len(in.Targets) {
				t.Fatalf("search progress = %#v", out)
			}
			for i, target := range out.Targets {
				if target.Target != in.Targets[i].Target || target.Grab.Value.Job == in.Origin.Job.Ref ||
					target.Grab.Value != in.Targets[i].Grabs.Value[0] || target.Import.Verification != domain.VerificationExternal || len(target.Import.Evidence) != 4 {
					t.Fatalf("target progress = %#v", target)
				}
			}
		})
	}
}

func TestTrackSearchSeparatesCommandGrabAndImport(t *testing.T) {
	checks := []struct {
		name        string
		edit        func(*SearchInput)
		wantCommand CommandState
		wantGrab    domain.ObservationState
		wantImport  domain.Verification
	}{
		{"command complete with no eligible result", func(in *SearchInput) {
			in.Targets[0].Grabs.Value = nil
			in.Targets[0].Grabs.Observation.State = domain.ObservationAbsent
		}, CommandCompleted, domain.ObservationAbsent, domain.VerificationUnresolved},
		{"empty grab list without positive absence", func(in *SearchInput) { in.Targets[0].Grabs.Value = nil }, CommandCompleted, "", domain.VerificationUnresolved},
		{"another eligible grab", func(in *SearchInput) {
			other := in.Targets[0].Grabs.Value[0]
			other.ID++
			in.Targets[0].Grabs.Value = append(in.Targets[0].Grabs.Value, other)
		}, CommandCompleted, "", domain.VerificationUnresolved},
		{"profile drift", func(in *SearchInput) { in.Targets[0].Profile.Value++ }, CommandCompleted, "", domain.VerificationUnresolved},
		{"profile belongs to another target", func(in *SearchInput) { in.Targets[0].Profile.Observation.SourceRecord = "episode:87" }, CommandCompleted, "", domain.VerificationUnresolved},
		{"repeated failed release", func(in *SearchInput) { in.Targets[0].Grabs.Value[0].Signature = in.Origin.Grab.Value[0].Signature }, CommandCompleted, "", domain.VerificationUnresolved},
		{"grab visible without an import", func(in *SearchInput) { in.Targets[0].Imports.Value = nil }, CommandCompleted, domain.ObservationKnown, domain.VerificationUnresolved},
		{"existing file left in place", func(in *SearchInput) { in.Targets[0].PriorFileID = in.Targets[0].CurrentFileID.Value }, CommandCompleted, domain.ObservationKnown, domain.VerificationUnresolved},
		{"wrong download recorded for import", func(in *SearchInput) { in.Targets[0].Imports.Value[0].DownloadID = in.Origin.Job.DownloadID }, CommandCompleted, domain.ObservationKnown, domain.VerificationUnresolved},
		{"file association disagrees", func(in *SearchInput) { in.Targets[0].Imports.Value[0].FileID++ }, CommandCompleted, domain.ObservationKnown, domain.VerificationUnresolved},
		{"command response lost", func(in *SearchInput) {
			in.Attempt.Acknowledgement = domain.Acknowledgement{State: domain.DispatchUncertain}
		}, "", domain.ObservationKnown, domain.VerificationExternal},
		{"broad command has no scoped proof", func(in *SearchInput) { in.Command.Value.Name = "SeasonSearch" }, "", domain.ObservationKnown, domain.VerificationExternal},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			in := syntheticSearch(domain.ProductSonarr)
			tc.edit(&in)
			out := TrackSearch(in)
			if len(out.Targets) != 2 || out.Command != tc.wantCommand ||
				out.Targets[0].Grab.Observation.State != tc.wantGrab || out.Targets[0].Import.Verification != tc.wantImport ||
				out.Targets[1].Import.Verification != domain.VerificationExternal {
				t.Fatalf("progress = %#v", out)
			}
		})
	}
}

func TestTrackSearchBoundsExplicitAndArrOwnedTargets(t *testing.T) {
	t.Run("still unmet subset", func(t *testing.T) {
		in := syntheticSearch(domain.ProductSonarr)
		in.Attempt.Operation.Targets = in.Attempt.Operation.Targets[1:]
		in.Attempt.Operation.Effects = in.Attempt.Operation.Effects[1:]
		in.Command.Value.Targets = in.Command.Value.Targets[1:]
		in.Targets = in.Targets[1:]
		if out := TrackSearch(in); out.Command != CommandCompleted || len(out.Targets) != 1 || out.Targets[0].Target.ID != 42 {
			t.Fatalf("one-target search = %#v", out)
		}
	})
	t.Run("attempt includes another target", func(t *testing.T) {
		in := syntheticSearch(domain.ProductSonarr)
		in.Attempt.Operation.Targets[0].ID++
		if out := TrackSearch(in); out.Command != "" || len(out.Targets) != 0 {
			t.Fatalf("invalid scope tracked: %#v", out)
		}
	})
	t.Run("Arr owns replacement", func(t *testing.T) {
		in := syntheticSearch(domain.ProductRadarr)
		in.Attempt = syntheticRejection(domain.ProductRadarr, false).Attempt
		if out := TrackArrReplacement(in); len(out.Targets) != 0 {
			t.Fatalf("non-Arr-owned rejection tracked: %#v", out)
		}
		in.Attempt.Operation.Effects = append(in.Attempt.Operation.Effects, domain.Effect{Kind: domain.EffectSearchDispatch})
		if out := TrackArrReplacement(in); out.Command != "" || len(out.Targets) != 1 || out.Targets[0].Import.Verification != domain.VerificationExternal {
			t.Fatalf("Arr-owned progress = %#v", out)
		}
		in.Targets[0].Grabs.Value = nil
		in.Targets[0].Grabs.Observation.State = domain.ObservationAbsent
		if out := TrackArrReplacement(in); out.Command != "" || len(out.Targets) != 1 || out.Targets[0].Grab.Observation.ID != "" {
			t.Fatalf("Arr-owned search exhausted without dispatch proof: %#v", out)
		}
	})
}

func TestTrackSearchAbsenceRequiresCompletedCommand(t *testing.T) {
	checks := []struct {
		name string
		edit func(*SearchInput)
	}{
		{"uncertain dispatch", func(in *SearchInput) { in.Attempt.Acknowledgement.State = domain.DispatchUncertain }},
		{"pending command", func(in *SearchInput) { in.Command.Value.State = CommandPending }},
		{"different command", func(in *SearchInput) { in.Command.Value.Reference = "192" }},
		{"absence read before completion", func(in *SearchInput) {
			in.Targets[0].Grabs.Observation.ObservedAt = in.Command.Observation.ObservedAt.Add(-time.Second)
		}},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			in := syntheticSearch(domain.ProductRadarr)
			in.Targets[0].Grabs.Value = nil
			in.Targets[0].Grabs.Observation.State = domain.ObservationAbsent
			tc.edit(&in)
			out := TrackSearch(in)
			if len(out.Targets) != 1 || out.Targets[0].Grab.Observation.ID != "" ||
				out.Targets[0].Import.Verification != domain.VerificationUnresolved {
				t.Fatalf("unconfirmed search yielded an empty result: %#v", out)
			}
		})
	}
}

func TestTrackSearchRequiresExactOriginalGrabObservation(t *testing.T) {
	checks := []struct {
		name string
		edit func(*SearchInput)
	}{
		{"missing evidence identity", func(in *SearchInput) { in.Origin.Grab.Observation.ID = "" }},
		{"wrong download", func(in *SearchInput) { in.Origin.Grab.Observation.SourceRecord = "other" }},
		{"wrong scope", func(in *SearchInput) { in.Origin.Grab.Observation.Scope.Instance.Epoch++ }},
		{"captured after dispatch", func(in *SearchInput) { in.Origin.Grab.Observation.ObservedAt = in.Attempt.SentAt.Add(time.Second) }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			in := syntheticSearch(domain.ProductRadarr)
			tc.edit(&in)
			if out := TrackSearch(in); out.Command != "" || len(out.Targets) != 0 {
				t.Fatalf("invalid original observation yielded progress: %#v", out)
			}
		})
	}
	t.Run("historical capture can expire before tracking", func(t *testing.T) {
		in := syntheticSearch(domain.ProductRadarr)
		in.Origin.Grab.Observation.ExpiresAt = in.Attempt.SentAt.Add(-time.Second)
		if out := TrackSearch(in); out.Command != CommandCompleted || len(out.Targets) != 1 || out.Targets[0].Import.Verification != domain.VerificationExternal {
			t.Fatalf("historical original observation lost provenance: %#v", out)
		}
	})
}
