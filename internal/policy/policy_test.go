package policy

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
)

func evidence[T any](now time.Time, scope domain.Scope, id string, value T) domain.Fact[T] {
	return domain.Fact[T]{Observation: domain.Observation{
		ID: domain.EvidenceID(id), Scope: scope, State: domain.ObservationKnown,
		ObservedAt: now.Add(-3 * time.Second), ExpiresAt: now.Add(time.Minute), Complete: true,
		Source: "arr", SourceRecord: id, ContentHash: id, AdapterRevision: "test-adapter",
	}, Value: value}
}

func example(product domain.Product) Input {
	clock := app.FixedClock{At: time.Date(2026, 11, 14, 1, 2, 3, 0, time.UTC)}
	now := clock.Now()
	instance := domain.InstanceRef{ID: "lab", Epoch: 1}
	scope := domain.Scope{Kind: domain.ScopeInstance, Instance: instance}
	root := domain.Scope{Kind: domain.ScopeRoot, Instance: instance, Key: "library"}
	client := domain.Scope{Kind: domain.ScopeClient, Instance: instance, Key: "download"}
	target := domain.TargetRef{Instance: instance, Kind: domain.TargetEpisode, ID: 6, SeriesID: 1}
	if product == domain.ProductRadarr {
		target.Kind, target.SeriesID = domain.TargetMovie, 0
	}
	movie := domain.Target{Ref: target, Root: root, Monitored: evidence(now, scope, "monitor", true),
		ProfileRevision: 7, FileID: evidence(now, scope, "file", int64(0))}
	movie.FileID.Observation.State = domain.ObservationAbsent
	rootCapacity := Capacity{Scope: root, FreeBytes: evidence(now, root, "root-capacity", uint64(1_000)),
		RequiredBytes: 500, FloorBytes: 100, ReservedBytes: 40, AllocationKnown: true}
	clientCapacity := Capacity{Scope: client, FreeBytes: evidence(now, client, "client-capacity", uint64(1_000)),
		RequiredBytes: 600, FloorBytes: 100, ReservedBytes: 40, AllocationKnown: true}
	input := Input{
		Now: now, Instance: domain.Instance{Ref: instance, Product: product, ConnectionRevision: 1},
		Case: domain.Case{ID: "case", Revision: 1, Targets: []domain.Target{movie}, Job: domain.Job{
			Ref: domain.JobRef{Instance: instance, ID: "job", Generation: 1}, DownloadID: "synthetic-job",
			CompleteSet: true, Client: client, Protocol: domain.ProtocolUsenet,
			Release: domain.ReleaseIdentity{GrabHistoryID: 5, IndexerID: 10, GUID: "release"},
			Members: []domain.QueueMember{{QueueID: 6, SnapshotGeneration: 1, Target: target}},
		}},
		Policy: Policy{Revision: 1, Global: domain.ModeAutonomous, Instance: domain.ModeAutonomous,
			GlobalActions: make(map[Path]Permission), InstanceActions: make(map[Path]Permission)},
		JobPhase: evidence(now, scope, "phase", PhaseComplete), ServiceHealth: evidence(now, scope, "health", HealthAvailable),
		IdentityValid: evidence(now, scope, "id", true), OwnershipVerified: evidence(now, scope, "owned", true),
		Overlap: evidence(now, scope, "overlap", false), Seeding: evidence(now, scope, "seeding", false),
		DedicatedUsenet: evidence(now, client, "dedicated", true), FirstCompletedAt: now.Add(-20 * time.Minute),
		PreviousCoherentAt: now.Add(-40 * time.Second),
		Targets: []TargetEvidence{{Target: movie, Intent: evidence(now, scope, "intent", NeedFirstImport), ProfileAtGrab: 7,
			CompetingGrab: evidence(now, scope, "competing", false), SearchAvailable: evidence(now, scope, "available", true)}},
		Import: ImportEvidence{Assessment: evidence(now, scope, "assessment", ImportAll), Candidates: []Candidate{{
			Key: "candidate", Targets: []domain.TargetRef{target}, Profiles: map[domain.TargetRef]domain.Revision{target: 7},
			Approved: evidence(now, scope, "approved", true), PathContained: evidence(now, scope, "contained", true),
			StableAndUsable: evidence(now, scope, "stable", true),
		}}},
		Failure: Failure{Reason: DownloadFailed, Primary: evidence(now, scope, "failure-arr", true),
			Corroborated: evidence(now, client, "failure-sab", true)},
		Capabilities: make(map[Path]Capability),
		Capacities: map[Path][]Capacity{
			ImportOriginal: {rootCapacity}, RejectRetain: {rootCapacity, clientCapacity},
			RejectRemove: {clientCapacity, rootCapacity}, SearchScoped: {clientCapacity, rootCapacity},
		},
		Gates: make(map[Path]Gate),
	}
	input.Failure.Corroborated.Observation.Source = "sab"
	for path, effects := range map[Path][]domain.EffectKind{
		ImportOriginal: {domain.EffectLibraryImport, domain.EffectSourceRetained},
		RejectRetain:   {domain.EffectQueueRemoval, domain.EffectBlocklist, domain.EffectSourceRetained},
		RejectRemove:   {domain.EffectQueueRemoval, domain.EffectBlocklist, domain.EffectClientRemoval, domain.EffectDataRemoval},
		SearchScoped:   {domain.EffectSearchDispatch},
	} {
		input.Policy.GlobalActions[path], input.Policy.InstanceActions[path] = PermitAutomatic, PermitAutomatic
		input.Gates[path] = Gate{BudgetKnown: true, Remaining: 1}
		input.Capabilities[path] = Capability{Path: path, Instance: instance, Protocol: domain.ProtocolUsenet,
			Revision: 1, Certified: true, Effects: effects, CoversFullJobPreservation: true,
			SearchAuthority: SearchByRescuarr, DedicatedUsenetRemoval: true}
	}
	return input
}

func failed(product domain.Product) Input {
	i := example(product)
	i.JobPhase.Value = PhaseFailed
	i.Import.Assessment.Value = ImportNone
	i.Import.Candidates = nil
	return i
}

func hasHold(result Result, code domain.ReasonCode) bool {
	return slices.ContainsFunc(result.Holds, func(f Finding) bool { return f.Code == code })
}

func TestEvaluateRecoveryOrder(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*Input)
		reason domain.ReasonCode
		state  domain.CaseState
		path   Path
	}{
		{"import before failure", nil, SafeImport, domain.CaseReady, ImportOriginal},
		{"activity before candidate", func(i *Input) { i.JobPhase.Value = PhaseProcess }, PostProcessing, domain.CaseWaiting, ""},
		{"grace before import", func(i *Input) { i.FirstCompletedAt = i.Now.Add(-time.Minute) }, CompletionGrace, domain.CaseWaiting, ""},
		{"satisfaction before candidate", func(i *Input) {
			i.Targets[0].Intent.Value = NeedSatisfied
			i.Targets[0].Target.FileID.Value = 17
			i.Targets[0].Target.FileID.Observation.State = domain.ObservationKnown
		}, AlreadySatisfied, domain.CaseClosed, ""},
		{"authoritative withdrawal", func(i *Input) { i.Targets[0].Intent.Value = NeedWithdrawn; i.Targets[0].Target.Monitored.Value = false }, TargetWithdrawn, domain.CaseClosed, ""},
		{"authentication before lower problems", func(i *Input) {
			i.ServiceHealth.Value = HealthAuthentication
			i.Case.Job.Shared = true
			i.JobPhase.Value = PhaseProcess
		}, AuthenticationFailed, domain.CaseHeld, ""},
		{"root before candidate", func(i *Input) {
			root := i.Case.Targets[0].Root
			i.Incidents = []domain.Incident{{Scope: root, Reason: RootUnavailable, Hold: true}}
		}, RootUnavailable, domain.CaseHeld, ImportOriginal},
		{"competing download", func(i *Input) { i.Targets[0].CompetingGrab.Value = true }, CompetingDownload, domain.CaseHeld, ImportOriginal},
		{"changed profile", func(i *Input) { i.Targets[0].Target.ProfileRevision++ }, RequestChanged, domain.CaseHeld, ""},
		{"bad target mapping", func(i *Input) { i.Case.Job.Members[0].Target.ID++ }, TargetAmbiguous, domain.CaseHeld, ""},
	}
	for _, product := range []domain.Product{domain.ProductSonarr, domain.ProductRadarr} {
		for _, test := range tests {
			t.Run(string(product)+"/"+test.name, func(t *testing.T) {
				i := example(product)
				if test.setup != nil {
					test.setup(&i)
				}
				got := Evaluate(i)
				if got.Reason != test.reason || got.State != test.state || got.Path != test.path ||
					(got.Proposal != nil) != (test.state == domain.CaseReady) || !got.NextCheck.After(i.Now) {
					t.Fatalf("unexpected result %+v", got)
				}
			})
		}
	}
}

func TestPermissionIntersectionAndApproval(t *testing.T) {
	i := example(domain.ProductSonarr)
	i.Policy.Instance = domain.ModeObserve
	if got := Evaluate(i); got.Authority != AuthorityPreview || got.Proposal == nil || MatchApproval(*got.Proposal, got, i.Now) {
		t.Fatalf("observe could approve or failed to preview: %+v", got)
	}
	i.Policy.Instance = domain.ModeAutonomous
	i.Policy.InstanceActions[ImportOriginal] = PermitReview
	original := Evaluate(i)
	if original.Authority != AuthorityAssist || !MatchApproval(*original.Proposal, original, i.Now) {
		t.Fatalf("assist could not approve: %+v", original)
	}
	advanced := i
	advanced.Now = i.Now.Add(time.Second)
	advanced.Overlap.Observation.ID = "clear-on-second-poll"
	if !MatchApproval(*original.Proposal, Evaluate(advanced), advanced.Now) {
		t.Fatal("unchanged material evidence from another poll invalidated approval")
	}
	advanced.Overlap.Observation.ContentHash = "changed"
	if MatchApproval(*original.Proposal, Evaluate(advanced), advanced.Now) || MatchApproval(*original.Proposal, original, original.Proposal.ExpiresAt) {
		t.Fatal("approved stale evidence or expired plan")
	}
	modified := *original.Proposal
	modified.ExpiresAt = modified.ExpiresAt.Add(time.Hour)
	if MatchApproval(modified, original, i.Now) {
		t.Fatal("extension of the signed proposal validity was accepted")
	}
	i.Policy.GlobalActions[ImportOriginal] = Deny
	got := Evaluate(i)
	if got.Proposal != nil || !hasHold(got, ActionDenied) {
		t.Fatalf("global action denial failed: %+v", got)
	}
}

func TestReplacementRequiresAllIndependentEffects(t *testing.T) {
	i := failed(domain.ProductRadarr)
	got := Evaluate(i)
	if got.Path != RejectRetain || got.Authority != AuthorityAutonomous || got.Proposal == nil {
		t.Fatalf("failed job was not considered for source retention: %+v", got)
	}
	outcome := domain.Outcome{AttemptID: "reject-attempt", Verification: domain.VerificationEffect, ObservedAt: i.Now.Add(-time.Second)}
	for _, effect := range got.Proposal.Operation.Effects {
		outcome.Effects = append(outcome.Effects, domain.EffectResult{Effect: effect, Verification: domain.VerificationEffect, Evidence: []domain.EvidenceID{"readback"}})
	}
	i.PriorReject = &PriorReject{Job: i.Case.Job.Ref, Path: RejectRetain, CapabilityRevision: 1, OwnedSearch: SearchByRescuarr, Targets: got.Proposal.Operation.Targets, Outcome: outcome}
	got = Evaluate(i)
	if got.Path != SearchScoped || got.Proposal == nil || len(got.Proposal.Operation.Targets) != 1 ||
		got.Proposal.Operation.Effects[0].Target != i.Targets[0].Target.Ref {
		t.Fatalf("search is not movie-scoped: %+v", got)
	}
	i.PriorReject.Outcome.Effects = outcome.Effects[:1]
	got = Evaluate(i)
	if got.Proposal != nil || !hasHold(got, AwaitingRejectVerification) {
		t.Fatalf("queue disappearance substituted for blocklist verification: %+v", got)
	}
}

func TestSubsetAndDeterministicReplay(t *testing.T) {
	i := example(domain.ProductSonarr)
	target := i.Targets[0]
	target.Target.Ref.ID++
	i.Targets = append(i.Targets, target)
	i.Case.Targets = append(i.Case.Targets, target.Target)
	i.Case.Job.Members = append(i.Case.Job.Members, domain.QueueMember{QueueID: 8, SnapshotGeneration: 1, Target: target.Target.Ref})
	i.Import.Assessment.Value = ImportSubset
	a := Evaluate(i)
	if a.Reason != PartialPack || a.Proposal == nil || len(a.Proposal.Operation.Targets) != 1 {
		t.Fatalf("qualified subset could not be proposed: %+v", a)
	}
	slices.Reverse(i.Targets)
	slices.Reverse(i.Case.Targets)
	slices.Reverse(i.Case.Job.Members)
	b := Evaluate(i)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Fatalf("permuting snapshots changed decision: %s != %s", aj, bj)
	}
	capability := i.Capabilities[ImportOriginal]
	capability.CoversFullJobPreservation = false
	i.Capabilities[ImportOriginal] = capability
	if got := Evaluate(i); got.Proposal != nil || !hasHold(got, SourcePreservationUnknown) {
		t.Fatalf("subset lacking source-preservation proof was accepted: %+v", got)
	}
}

func TestDestructiveRecoveryFailsClosedAcrossCombinations(t *testing.T) {
	for mask := 0; mask < 1<<10; mask++ {
		i := failed(domain.ProductRadarr)
		delete(i.Capabilities, RejectRetain)
		if mask&1 != 0 {
			i.Case.Job.Shared = true
		}
		if mask&2 != 0 {
			i.Case.Job.CompleteSet = false
		}
		if mask&4 != 0 {
			i.Case.Job.Protocol = domain.ProtocolTorrent
		}
		if mask&8 != 0 {
			i.Seeding.Value = true
		}
		if mask&16 != 0 {
			i.DedicatedUsenet.Value = false
		}
		if mask&32 != 0 {
			i.Overlap.Value = true
		}
		if mask&64 != 0 {
			i.Failure.Corroborated.Observation.Source = i.Failure.Primary.Observation.Source
		}
		if mask&128 != 0 {
			i.Capacities[RejectRemove][0].FreeBytes.Observation.State = domain.ObservationUnknown
		}
		if mask&256 != 0 {
			i.Case.Job.Release.GUID = ""
		}
		if mask&512 != 0 {
			i.Policy.Paused = true
		}
		got := Evaluate(i)
		if mask == 0 && (got.Proposal == nil || got.Path != RejectRemove || got.Authority != AuthorityAutonomous) {
			t.Fatalf("positive removal control did not qualify: %+v", got)
		}
		if mask != 0 && got.Proposal != nil {
			t.Fatalf("unsafe operation proposed for mask %d: %+v", mask, got)
		}
	}
}

func TestCapacityEvidenceIsCurrentScopedAndOverflowSafe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Input, *Capacity)
		reason domain.ReasonCode
	}{
		{"unknown", func(_ *Input, c *Capacity) { c.FreeBytes.Observation.State = domain.ObservationUnknown }, CapacityUnknown},
		{"stale", func(i *Input, c *Capacity) { c.FreeBytes.Observation.ObservedAt = i.Now.Add(-6 * time.Minute) }, CapacityStale},
		{"scope mismatch", func(_ *Input, c *Capacity) { c.FreeBytes.Observation.Scope.Key = "unrelated-root" }, CapacityUnknown},
		{"insufficient with reserves", func(_ *Input, c *Capacity) { c.FreeBytes.Value = 639 }, LowCapacity},
		{"sum overflow", func(_ *Input, c *Capacity) { c.FreeBytes.Value, c.ReservedBytes = 1_000, ^uint64(0) }, LowCapacity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := example(domain.ProductSonarr)
			tc.mutate(&i, &i.Capacities[ImportOriginal][0])
			got := Evaluate(i)
			if got.Proposal != nil || !hasHold(got, tc.reason) {
				t.Fatalf("unsafe capacity input did not hold: %+v", got)
			}
		})
	}
}

func TestRejectedJobSearchFreshnessAndBoundaries(t *testing.T) {
	i := failed(domain.ProductRadarr)
	retained := Evaluate(i)
	i.PriorReject = &PriorReject{Job: i.Case.Job.Ref, Path: retained.Path, CapabilityRevision: retained.Proposal.CapabilityRevision,
		Targets: retained.Proposal.Operation.Targets, OwnedSearch: SearchByRescuarr, Outcome: domain.Outcome{
			AttemptID: "test-verified-attempt", ObservedAt: i.Now.Add(-time.Minute), Verification: domain.VerificationEffect,
		}}
	for _, effect := range retained.Proposal.Operation.Effects {
		i.PriorReject.Outcome.Effects = append(i.PriorReject.Outcome.Effects, domain.EffectResult{
			Effect: effect, Verification: domain.VerificationEffect, Evidence: []domain.EvidenceID{domain.EvidenceID(effect.Kind)},
		})
	}
	i.JobPhase.Observation.State = domain.ObservationAbsent
	if got := Evaluate(i); got.Path != SearchScoped || got.Proposal == nil {
		t.Fatalf("verified queue absence should allow scope to proceed: %+v", got)
	}
	i.JobPhase.Observation.State, i.JobPhase.Value = domain.ObservationKnown, PhaseDownload
	if got := Evaluate(i); got.Proposal != nil || got.Reason != DownloadActive {
		t.Fatalf("active new download failed to stop search: %+v", got)
	}
	i.JobPhase.Observation.State = domain.ObservationAbsent
	i.PriorReject.Targets = nil
	if got := Evaluate(i); got.Proposal != nil || !hasHold(got, TargetAmbiguous) {
		t.Fatalf("prior rejection failed to bind original target: %+v", got)
	}
}

func TestRejectPolicyNeverSubstitutesDestructionForADeniedRetainPlan(t *testing.T) {
	i := failed(domain.ProductRadarr)
	i.Policy.GlobalActions[RejectRetain] = Deny
	if got := Evaluate(i); got.Proposal != nil || got.Path != RejectRetain || !hasHold(got, ActionDenied) {
		t.Fatalf("denied retain plan was replaced with an unsafe alternative: %+v", got)
	}
	i.Policy.GlobalActions[RejectRetain] = PermitAutomatic
	i.Policy.GlobalActions[SearchScoped] = Deny
	if got := Evaluate(i); got.Proposal != nil || !hasHold(got, ActionDenied) {
		t.Fatalf("reject without its permitted follow-on search was authorised: %+v", got)
	}
}

func TestEffectCertificateRejectsUnplannedDestructiveSideEffect(t *testing.T) {
	i := example(domain.ProductSonarr)
	certificate := i.Capabilities[ImportOriginal]
	certificate.Effects = append(certificate.Effects, domain.EffectDataRemoval)
	i.Capabilities[ImportOriginal] = certificate
	if got := Evaluate(i); got.Proposal != nil || !hasHold(got, UnsupportedEffect) {
		t.Fatalf("import with unplanned data removal was accepted: %+v", got)
	}
}
