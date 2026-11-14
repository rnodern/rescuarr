package replacement

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/policy"
)

// These synthetic projections use the pack and movie identities from the lab
// read fixtures. They exercise proof rules without qualifying an Arr route.
func syntheticRejection(product domain.Product, remove bool) RejectInput {
	sentAt := time.Date(2026, 1, 1, 0, 3, 0, 0, time.UTC)
	downloadID, seriesID, targetIDs := "SABnzbd_nzo_labpack", int64(12), []int64{41, 42}
	kind := domain.TargetEpisode
	if product == domain.ProductRadarr {
		downloadID, seriesID, targetIDs = "SABnzbd_nzo_labmovie", 0, []int64{51}
		kind = domain.TargetMovie
	}
	instance := domain.InstanceRef{ID: domain.InstanceID(product), Epoch: 1}
	client := domain.Scope{Kind: domain.ScopeClient, Instance: instance, Key: "lab-sab"}
	volume := domain.Scope{Kind: domain.ScopeVolume, Instance: instance, Key: "lab-downloads"}
	job := domain.Job{
		Ref:    domain.JobRef{Instance: instance, ID: "original", Generation: 4},
		Client: client, DownloadID: downloadID, Protocol: domain.ProtocolUsenet, CompleteSet: true,
		Release: domain.ReleaseIdentity{GrabHistoryID: 203, IndexerID: 9, GUID: "original-release"},
	}
	sig := ReleaseSignature{
		TitleFingerprint: "original-title", QualityFingerprint: "web-720p",
		IndexerID: job.Release.IndexerID, Protocol: job.Protocol,
	}
	var targets []domain.TargetRef
	var grabs, failed []Record
	var rows []int64
	for i, id := range targetIDs {
		target := domain.TargetRef{Instance: instance, Kind: kind, ID: id, SeriesID: seriesID}
		row := int64(1001 + i)
		job.Members = append(job.Members, domain.QueueMember{QueueID: row, SnapshotGeneration: 20, Target: target})
		rows = append(rows, row)
		targets = append(targets, target)
		grabs = append(grabs, Record{
			ID: int64(203 + i), DownloadID: downloadID, Target: target, Signature: sig, At: sentAt.Add(-2 * time.Minute),
		})
		failed = append(failed, Record{
			ID: int64(401 + i), DownloadID: downloadID, Target: target, Signature: sig, At: sentAt.Add(2 * time.Second),
		})
	}
	origin := Original{Product: product, Job: job, Targets: targets}
	before := sentAt.Add(-time.Second)
	after := sentAt.Add(10 * time.Second)
	origin.Grab = domain.Fact[[]Record]{Value: grabs, Observation: arrRead(origin, "grab", GrabHistory, domain.ObservationKnown, downloadID, before)}
	origin.Arr = domain.Fact[bool]{Value: true, Observation: arrRead(origin, "arr-terminal", ArrTerminal, domain.ObservationKnown, downloadID, before)}
	origin.Client = domain.Fact[bool]{Value: true, Observation: read("sab-terminal", client, domain.Source(domain.ProductSAB), ClientTerminal, domain.ObservationKnown, downloadID, before)}
	input := RejectInput{
		Now: after.Add(time.Second), Origin: origin,
		Attempt: domain.Attempt{
			ID: "reject", SentAt: sentAt, Acknowledgement: domain.Acknowledgement{State: domain.DispatchAcknowledged},
			Operation: domain.Operation{Action: domain.ActionReject, Key: "reject-original", Job: job.Ref, Targets: slices.Clone(targets),
				Effects: []domain.Effect{{Kind: domain.EffectQueueRemoval}, {Kind: domain.EffectBlocklist}}},
			Preflight: []domain.EvidenceID{"grab", "arr-terminal", "sab-terminal", "prior-blocklist", "prior-source"},
		},
		Queue:  domain.Fact[[]int64]{Value: rows, Observation: arrRead(origin, "queue", policy.RejectQueueReadback, domain.ObservationAbsent, downloadID, after)},
		Failed: domain.Fact[[]Record]{Value: failed, Observation: arrRead(origin, "failed", policy.RejectHistoryReadback, domain.ObservationKnown, downloadID, after)},
		Before: domain.Fact[[]BlocklistEntry]{Observation: arrRead(origin, "prior-blocklist", policy.RejectBlocklistReadback, domain.ObservationAbsent, downloadID, before)},
		After: domain.Fact[[]BlocklistEntry]{Value: []BlocklistEntry{{ID: 501, Targets: slices.Clone(targets), Signature: sig, At: sentAt.Add(3 * time.Second)}},
			Observation: arrRead(origin, "blocklist", policy.RejectBlocklistReadback, domain.ObservationKnown, downloadID, after)},
		SourceBefore: read("prior-source", volume, policy.StorageProbeSource, policy.RejectSourceReadback, domain.ObservationKnown, downloadID, before),
		Source:       read("source-retained", volume, policy.StorageProbeSource, policy.RejectSourceReadback, domain.ObservationKnown, downloadID, after),
	}
	if remove {
		input.Attempt.Operation.Effects = append(input.Attempt.Operation.Effects, domain.Effect{Kind: domain.EffectClientRemoval}, domain.Effect{Kind: domain.EffectDataRemoval})
		input.Source = read("source-removed", volume, policy.StorageProbeSource, policy.RejectDataReadback, domain.ObservationAbsent, downloadID, after)
		input.Client = domain.Fact[bool]{Value: true, Observation: read("client-retired", client, domain.Source(domain.ProductSAB), policy.RejectClientReadback, domain.ObservationAbsent, downloadID, after)}
	} else {
		input.Attempt.Operation.Effects = append(input.Attempt.Operation.Effects, domain.Effect{Kind: domain.EffectSourceRetained})
	}
	return input
}

func read(id domain.EvidenceID, scope domain.Scope, source domain.Source, kind domain.ObservationKind, state domain.ObservationState, record string, at time.Time) domain.Observation {
	return domain.Observation{
		ID: id, Scope: scope, Source: source, Kind: kind, State: state, SourceRecord: record,
		AdapterRevision: string(source) + "-v1", SchemaRevision: 1, SnapshotGeneration: 20,
		ObservedAt: at, ExpiresAt: at.Add(time.Minute), Complete: true,
	}
}

func arrRead(o Original, id domain.EvidenceID, kind domain.ObservationKind, state domain.ObservationState, record string, at time.Time) domain.Observation {
	return read(id, domain.Scope{Kind: domain.ScopeInstance, Instance: o.Job.Ref.Instance}, domain.Source(o.Product), kind, state, record, at)
}

func verifyEffect(t *testing.T, outcome domain.Outcome, effect domain.EffectKind, want domain.Verification) {
	t.Helper()
	for _, result := range outcome.Effects {
		if result.Effect.Kind == effect {
			if result.Verification != want {
				t.Errorf("%s = %s, want %s", effect, result.Verification, want)
			}
			return
		}
	}
	t.Errorf("%s effect missing", effect)
}

func TestVerifyReject(t *testing.T) {
	for _, product := range []domain.Product{domain.ProductSonarr, domain.ProductRadarr} {
		for _, remove := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/remove=%t", product, remove), func(t *testing.T) {
				input := syntheticRejection(product, remove)
				out := VerifyReject(input)
				if out.Outcome.Verification != domain.VerificationEffect || out.Outcome.AttemptID != input.Attempt.ID {
					t.Fatalf("outcome = %#v", out.Outcome)
				}
				if out.Readback.Job != input.Origin.Job.Ref || out.Readback.Release != input.Origin.Job.Release ||
					out.Readback.FailedHistoryID != 401 || !slices.Equal(out.Readback.QueueIDs, input.Queue.Value) {
					t.Fatalf("readback scope = %#v", out.Readback)
				}
				for _, effect := range input.Attempt.Operation.Effects {
					verifyEffect(t, out.Outcome, effect.Kind, domain.VerificationEffect)
				}
			})
		}
	}
}

func TestRejectRequiresCompleteIndependentTerminalSource(t *testing.T) {
	checks := []struct {
		name string
		edit func(*RejectInput)
	}{
		{"missing grab", func(in *RejectInput) { in.Origin.Grab.Value = nil }},
		{"incomplete grab set", func(in *RejectInput) { in.Origin.Grab.Value = in.Origin.Grab.Value[:1] }},
		{"unmatched grab", func(in *RejectInput) { in.Origin.Grab.Value[1].DownloadID = "different-original" }},
		{"different grab revision", func(in *RejectInput) { in.Origin.Arr.Observation.AdapterRevision = "old-arr" }},
		{"shared job", func(in *RejectInput) { in.Origin.Job.Shared = true }},
		{"partly salvageable job", func(in *RejectInput) { in.Origin.Client.Value = false }},
		{"torrent", func(in *RejectInput) { in.Origin.Job.Protocol = domain.ProtocolTorrent }},
		{"stale terminal evidence", func(in *RejectInput) { in.Origin.Client.Observation.ExpiresAt = in.Attempt.SentAt }},
		{"same terminal source", func(in *RejectInput) { in.Origin.Client.Observation.Source = in.Origin.Arr.Observation.Source }},
		{"same terminal evidence", func(in *RejectInput) { in.Origin.Client.Observation.ID = in.Origin.Arr.Observation.ID }},
		{"other instance", func(in *RejectInput) { in.Origin.Client.Observation.Scope.Instance.Epoch++ }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			input := syntheticRejection(domain.ProductSonarr, false)
			tc.edit(&input)
			if input.Origin.VerifiedSource(input.Attempt.SentAt) {
				t.Fatal("source verified")
			}
			if got := VerifyReject(input); got.Outcome.Verification != domain.VerificationUnresolved || got.Readback.AttemptID != "" {
				t.Fatalf("invalid source yielded rejection evidence: %#v", got)
			}
		})
	}
}

func TestRejectSeparatesPartialEffects(t *testing.T) {
	checks := []struct {
		name    string
		edit    func(*RejectInput)
		missing domain.EffectKind
	}{
		{"queue only has partial pack scope", func(in *RejectInput) { in.Queue.Value = in.Queue.Value[:1] }, domain.EffectQueueRemoval},
		{"blocklist not written", func(in *RejectInput) { in.After.Value = nil }, domain.EffectBlocklist},
		{"blocklist existed before send", func(in *RejectInput) {
			in.Before.Observation.State = domain.ObservationKnown
			in.Before.Value = slices.Clone(in.After.Value)
			in.Before.Value[0].At = in.Before.Observation.ObservedAt
		}, domain.EffectBlocklist},
		{"blocklist is for part of pack", func(in *RejectInput) { in.After.Value[0].Targets = in.After.Value[0].Targets[:1] }, domain.EffectBlocklist},
		{"failed history is for another job", func(in *RejectInput) { in.Failed.Value[1].DownloadID = "other" }, domain.EffectBlocklist},
		{"failed history reused a grab id", func(in *RejectInput) { in.Failed.Value[1].ID = 204 }, domain.EffectBlocklist},
		{"failed history is missing", func(in *RejectInput) { in.Failed.Value = nil }, domain.EffectBlocklist},
		{"client absence lacks proof", func(in *RejectInput) { in.Client.Value = false }, domain.EffectClientRemoval},
		{"no source storage probe", func(in *RejectInput) { in.Source.Complete = false }, domain.EffectDataRemoval},
		{"preflight source scope does not match", func(in *RejectInput) { in.SourceBefore.Scope.Key = "other-volume" }, domain.EffectDataRemoval},
		{"source proof reused the client evidence id", func(in *RejectInput) { in.Source.ID = in.Client.Observation.ID }, domain.EffectDataRemoval},
		{"source is still retained", func(in *RejectInput) {
			in.Source.Kind = policy.RejectSourceReadback
			in.Source.State = domain.ObservationKnown
		}, domain.EffectDataRemoval},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			input := syntheticRejection(domain.ProductSonarr, true)
			tc.edit(&input)
			out := VerifyReject(input)
			if out.Outcome.Verification != domain.VerificationPartial {
				t.Fatalf("outcome = %#v", out.Outcome)
			}
			verifyEffect(t, out.Outcome, tc.missing, domain.VerificationUnresolved)
		})
	}
}

func TestUncertainRejectKeepsAttemptOpenWithExactReadbacks(t *testing.T) {
	for _, state := range []domain.DispatchState{"", domain.DispatchUncertain} {
		input := syntheticRejection(domain.ProductRadarr, true)
		input.Attempt.Acknowledgement.State = state
		out := VerifyReject(input)
		if out.Outcome.Verification != domain.VerificationPartial || out.Readback.Queue.ID == "" ||
			out.Readback.Blocklist.ID == "" || out.Readback.ClientRemoval.ID == "" || out.Readback.DataRemoval.ID == "" {
			t.Fatalf("%q outcome = %#v", state, out)
		}
		for _, effect := range input.Attempt.Operation.Effects {
			verifyEffect(t, out.Outcome, effect.Kind, domain.VerificationPartial)
		}
	}
}

func TestArrOwnedRejectKeepsSearchDispatchUnresolved(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprintf("remove=%t", remove), func(t *testing.T) {
			input := syntheticRejection(domain.ProductRadarr, remove)
			input.Attempt.Operation.Effects = append(input.Attempt.Operation.Effects, domain.Effect{Kind: domain.EffectSearchDispatch})
			out := VerifyReject(input)
			if out.Outcome.Verification != domain.VerificationPartial || out.Readback.Blocklist.ID == "" {
				t.Fatalf("outcome = %#v", out)
			}
			for _, effect := range input.Attempt.Operation.Effects[:len(input.Attempt.Operation.Effects)-1] {
				verifyEffect(t, out.Outcome, effect.Kind, domain.VerificationEffect)
			}
			verifyEffect(t, out.Outcome, domain.EffectSearchDispatch, domain.VerificationUnresolved)
		})
	}
}
