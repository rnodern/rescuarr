package learning

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

var clock = time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)

func testPattern(t *testing.T, family Family) Fingerprint {
	t.Helper()
	epochs := Epochs{Installation: domain.InstanceRef{ID: "test", Epoch: 3}, Adapter: 2, Config: 1, Policy: 1}
	signature := Signature{Version: FingerprintVersion, Epochs: epochs, Product: domain.ProductRadarr,
		Client: ClientSAB, Protocol: domain.ProtocolUsenet, Phase: CompletedImport, Family: family, Reason: "safe_import_candidate",
		Shape: SingleFile, UniqueMapping: true, StableComplete: true}
	switch family {
	case Replacement:
		signature.Phase, signature.Reason = TerminalFailure, "download_failed"
	case IncidentCause:
		signature.Client, signature.Protocol, signature.Phase, signature.Shape = ClientUnused, domain.ProtocolUnknown, DependencyFault, Dependency
		signature.Reason, signature.UniqueMapping, signature.StableComplete = "root_path_unavailable", false, false
		signature.CausalScope = domain.Scope{Instance: epochs.Installation, Kind: domain.ScopeRoot, Key: "local-root-1"}
	}
	pattern, err := BuildFingerprint(signature)
	if err != nil {
		t.Fatal(err)
	}
	return pattern
}

func testCandidate(pattern Fingerprint, id int, at time.Time) Candidate {
	instance := pattern.Signature.Epochs.Installation
	job := domain.JobRef{Instance: instance, ID: domain.JobID(fmt.Sprintf("job-%d", id)), Generation: 1}
	followup := domain.JobRef{Instance: instance, ID: "replacement-" + job.ID, Generation: 1}
	ref := domain.TargetRef{Instance: instance, Kind: domain.TargetMovie, ID: int64(id)}
	scope := pattern.Signature.CausalScope
	proof := func(role string, at time.Time, job domain.JobRef) Proof {
		return Proof{Evidence: domain.EvidenceID(fmt.Sprintf("%s-%d", role, id)), Observed: at, Job: job, Target: ref, Scope: scope}
	}
	started := at.Add(-25 * time.Hour)
	media := proof("media", started.Add(time.Minute), job)
	history := proof("history", started.Add(2*time.Minute), job)
	original := domain.Operation{Key: domain.OperationKey(job.ID), Job: job, Targets: []domain.TargetRef{ref}, Action: domain.ActionImport,
		Effects: []domain.Effect{{Kind: domain.EffectLibraryImport, Target: ref}, {Kind: domain.EffectSourceRetained}}}
	if pattern.Signature.Family == Replacement {
		original.Action = domain.ActionSearch
		original.Effects = []domain.Effect{{Kind: domain.EffectSearchDispatch, Target: ref}}
		media = proof("replacement-import", started.Add(3*time.Hour), followup)
		history = proof("replacement-history", started.Add(3*time.Hour+time.Minute), followup)
	}
	intent := domain.Intent{ID: domain.IntentID(job.ID), Actor: "test-actor", CreatedAt: started.Add(-time.Minute),
		Proposal: domain.Proposal{ID: domain.ProposalID(job.ID), CaseID: domain.CaseID(job.ID), CaseRevision: 1,
			PolicyRevision: 1, CapabilityRevision: 1, DecisionHash: "verified-test-plan", Operation: original, ExpiresAt: started.Add(time.Minute)}}
	attempt := domain.Attempt{ID: domain.AttemptID(job.ID), IntentID: intent.ID, SentAt: started, Operation: original,
		Preflight: []domain.EvidenceID{proof("preflight", started, job).Evidence}}
	outcome := domain.Outcome{AttemptID: attempt.ID, Verification: domain.VerificationEffect, ObservedAt: started.Add(3 * time.Minute)}
	for index, effect := range original.Effects {
		id := proof(fmt.Sprintf("effect-%d", index), outcome.ObservedAt, job).Evidence
		if effect.Kind == domain.EffectLibraryImport {
			id = media.Evidence
		}
		if effect.Kind == domain.EffectSearchDispatch {
			id = proof("replacement-grab", started.Add(30*time.Minute), followup).Evidence
		}
		outcome.Effects = append(outcome.Effects, domain.EffectResult{Effect: effect, Verification: domain.VerificationEffect, Evidence: []domain.EvidenceID{id}})
	}
	if pattern.Signature.Family == IncidentCause {
		outcome, original.Action = domain.Outcome{}, ""
	}
	return Candidate{
		Sample: domain.PatternSample{Pattern: pattern.ID, Scope: scope, Job: job, Target: ref, Treatment: original.Action, Outcome: outcome, RecordedAt: at},
		Plan:   intent, Attempt: attempt, StartedAt: started, FollowupJob: followup, DispatchVerified: true, IdentityVerified: true, QualifiedPlan: true,
		MediaAssociation: media, ImportHistory: history,
		NoReversalThrough: proof("retention", at, job), ReplacementGrab: proof("replacement-grab", started.Add(30*time.Minute), followup),
		HealthyDependency: proof("dependency-health", started.Add(time.Minute), job), IndependentRecovery: proof("independent-recovery", at, followup),
	}
}

func TestFingerprintUsesExactCanonicalStructuralScope(t *testing.T) {
	original := testPattern(t, OriginalImport)
	signature := original.Signature
	signature.Corroborating = []domain.ReasonCode{"file_unreadable", "post_processing", "post_processing"}
	a, err := BuildFingerprint(signature)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(signature.Corroborating)
	b, err := BuildFingerprint(signature)
	if err != nil || a.ID != b.ID || len(a.Signature.Corroborating) != 2 || slices.Equal(a.Signature.Corroborating, signature.Corroborating) {
		t.Fatalf("canonical fingerprint mismatch: %+v %+v %v", a, b, err)
	}
	signature.Epochs.Config++
	c, err := BuildFingerprint(signature)
	if err != nil || c.ID == a.ID {
		t.Fatalf("config epoch should change the fingerprint: %+v %v", c, err)
	}
	signature.CausalScope = domain.Scope{Instance: signature.Epochs.Installation, Kind: domain.ScopeRoot, Key: "/private/folder"}
	if _, err := BuildFingerprint(signature); err == nil {
		t.Fatal("path used as learnt scope identity")
	}
}

func TestAttributionAndObservationWindows(t *testing.T) {
	pattern := testPattern(t, OriginalImport)
	for _, test := range []struct {
		name   string
		mutate func(*Candidate)
		want   SampleState
	}{
		{"verified and stable", func(*Candidate) {}, SampleSuccess},
		{"operator override", func(c *Candidate) { c.Override = true }, SampleNeutral},
		{"preflight missing", func(c *Candidate) { c.Attempt.Preflight = nil }, SampleNeutral},
		{"different attempt", func(c *Candidate) { c.Attempt.ID = "other" }, SampleNeutral},
		{"missing distinct history", func(c *Candidate) { c.ImportHistory.Evidence = c.MediaAssociation.Evidence }, SampleNeutral},
		{"different target", func(c *Candidate) { c.ImportHistory.Target.ID++ }, SampleNeutral},
		{"24 hour observation pending", func(c *Candidate) { c.NoReversalThrough.Observed = c.ImportHistory.Observed.Add(23 * time.Hour) }, SampleNeutral},
		{"another actor", func(c *Candidate) { c.Sample.Outcome.Verification = domain.VerificationExternal }, SampleNeutral},
		{"acknowledged without all effects", func(c *Candidate) { c.Sample.Outcome.Effects = c.Sample.Outcome.Effects[:1] }, SampleNeutral},
		{"causally proven failure", failedCandidate, SampleFailure},
		{"unsafe effect", func(c *Candidate) { c.Safety, c.SafetyEvidence = NeededFileLost, c.NoReversalThrough }, SampleContradiction},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := testCandidate(pattern, 1, clock.Add(-time.Hour))
			test.mutate(&candidate)
			if got := Classify(pattern, candidate, clock); got.State != test.want {
				t.Fatalf("%s: %+v", test.want, got)
			}
		})
	}
}

func TestDelayedPollingCannotMoveSampleTimestampOrEligibility(t *testing.T) {
	pattern := testPattern(t, OriginalImport)
	c := testCandidate(pattern, 1, clock.Add(-time.Hour))
	c.Sample.RecordedAt = clock // simulate a poll many hours after the outcome
	got := Classify(pattern, c, clock)
	if got.State != SampleSuccess || !got.Sample.RecordedAt.Equal(c.Sample.Outcome.ObservedAt) ||
		got.Sample.RecordedAt.UTC().Format("2006-01-02") == clock.UTC().Format("2006-01-02") {
		t.Fatalf("delayed poll changed success age/day: %+v", got)
	}

	c = testCandidate(pattern, 2, clock.Add(-time.Hour))
	failedCandidate(&c)
	c.Sample.RecordedAt = clock
	got = Classify(pattern, c, clock)
	if got.State != SampleFailure || !got.Sample.RecordedAt.Equal(c.Sample.Outcome.ObservedAt) ||
		got.Sample.RecordedAt.UTC().Format("2006-01-02") == clock.UTC().Format("2006-01-02") {
		t.Fatalf("delayed poll changed failure age: %+v", got)
	}

	c = testCandidate(pattern, 3, clock.Add(-time.Hour))
	c.Override = true
	c.Sample.Eligible = true
	got = Classify(pattern, c, clock)
	if got.Sample.Eligible || got.Sample.Exclusion != "operator_override" {
		t.Fatalf("exclusion retained eligible sample: %+v", got.Sample)
	}
}

func failedCandidate(c *Candidate) {
	c.Sample.Outcome.Verification = domain.VerificationNoEffect
	for index := range c.Sample.Outcome.Effects {
		c.Sample.Outcome.Effects[index].Verification = domain.VerificationNoEffect
	}
}

func TestReplacementSamplesWaitForLedgerBinding(t *testing.T) {
	pattern := testPattern(t, Replacement)
	c := testCandidate(pattern, 1, clock.Add(-time.Hour))
	got := Classify(pattern, c, clock)
	if got.State != SampleNeutral || got.Sample.Eligible || got.Sample.Exclusion != "replacement_chain_binding_missing" {
		t.Fatalf("replacement fixture counted without ledger binding: %+v", got)
	}
	failedCandidate(&c)
	got = Classify(pattern, c, clock)
	if got.State != SampleNeutral || got.Sample.Eligible || got.Sample.Exclusion != "replacement_chain_binding_missing" {
		t.Fatalf("replacement failure counted without ledger binding: %+v", got)
	}

	incident := testPattern(t, IncidentCause)
	c = testCandidate(incident, 1, clock.Add(-time.Hour))
	if got := Classify(incident, c, clock); got.State != SampleSuccess {
		t.Fatalf("independently corroborated root recovery did not count: %+v", got)
	}
	c.IndependentRecovery.Evidence = c.HealthyDependency.Evidence
	if got := Classify(incident, c, clock); got.State != SampleNeutral {
		t.Fatalf("single incident health read counted as recovery: %+v", got)
	}
}

func TestPatternPromotionExpiryAndQuarantine(t *testing.T) {
	pattern := testPattern(t, OriginalImport)
	input := Input{Now: clock, Pattern: pattern, Current: pattern.Signature.Epochs}
	for id := 1; id <= 9; id++ {
		input.Candidates = append(input.Candidates, testCandidate(pattern, id, clock.Add(-time.Duration(id%4+1)*24*time.Hour)))
	}
	old := testCandidate(pattern, 99, clock.Add(-8*24*time.Hour))
	failedCandidate(&old)
	input.Candidates = append(input.Candidates, old, input.Candidates[0])
	view, err := Evaluate(input)
	if err != nil || view.Status != Recommended || view.Counts.Successes != 9 || view.Counts.Excluded != 1 || view.Counts.DistinctDays != 4 {
		t.Fatalf("90%% threshold or dedupe failed: %+v %v", view, err)
	}
	for id := 20; id < 22; id++ {
		failure := testCandidate(pattern, id, clock.Add(-time.Hour))
		failedCandidate(&failure)
		input.Candidates = append(input.Candidates, failure)
	}
	view, err = Evaluate(input)
	if err != nil || view.Status != Suspended || view.Counts.RecentFailures != 2 {
		t.Fatalf("recent failures did not suspend: %+v %v", view, err)
	}
	contradiction := input.Candidates[0]
	contradiction.Safety, contradiction.SafetyEvidence = WorsenedQuality, contradiction.NoReversalThrough
	input.Candidates = append(input.Candidates, contradiction, contradiction)
	view, err = Evaluate(input)
	if err != nil || view.Status != Quarantined || view.Counts.Contradictions != 1 {
		t.Fatalf("contradiction did not quarantine: %+v %v", view, err)
	}
	input.Current.Adapter++
	if got, _ := Evaluate(input); got.Status != Invalidated || got.Counts.Contradictions != 1 {
		t.Fatalf("epoch drift did not preserve visible old history: %+v", got)
	}
	input.Current = pattern.Signature.Epochs
	input.Candidates = input.Candidates[:10]
	input.Now = clock.Add(31 * 24 * time.Hour)
	if got, _ := Evaluate(input); got.Status != Stale || got.Counts.Successes != 0 {
		t.Fatalf("old rows did not expire: %+v", got)
	}
}

func TestResetAuditAndRestartReplayFromRetainedBytes(t *testing.T) {
	pattern := testPattern(t, OriginalImport)
	input := Input{Now: clock, Pattern: pattern, Current: pattern.Signature.Epochs}
	for id := 1; id <= 5; id++ {
		input.Candidates = append(input.Candidates, testCandidate(pattern, id, clock.Add(-time.Duration(id%3+1)*24*time.Hour)))
	}
	before, err := Evaluate(input)
	if err != nil || before.Status != Recommended {
		t.Fatalf("promotion needed five qualified rows: %+v %v", before, err)
	}
	if shorter, _ := Evaluate(Input{Now: clock, Pattern: pattern, Current: input.Current, Candidates: input.Candidates[:4]}); shorter.Status != Collecting {
		t.Fatal("retention reduced evidence without dropping recommendation")
	}
	command := Command{Change: Pause, Actor: "owner", Reason: "operator_request", Event: "pause-1", At: clock.Add(-time.Minute)}
	updated, audit, err := Apply(pattern, input.Controls, command)
	if err != nil || !updated.Paused || audit.Pattern != pattern.ID || audit.Event.Actor != "owner" || audit.Event.Kind != "learning_pause" {
		t.Fatalf("unexpected pause audit: %+v %+v %v", updated, audit, err)
	}
	if _, _, err := Apply(pattern, updated, command); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale controls revision did not conflict: %v", err)
	}
	command.ExpectedRevision, command.Change = updated.Revision, Reset
	input.Controls, _, err = Apply(pattern, updated, command)
	if err != nil || input.Controls.ResetAt.IsZero() {
		t.Fatal("reset failed")
	}
	if cleared, _ := Evaluate(input); cleared.Counts.Successes != 0 || cleared.Counts.Excluded != 5 || cleared.Status != Paused {
		t.Fatalf("reset discarded audit history or kept promotion: %+v", cleared)
	}
	state, _ := json.Marshal(input)
	var restored Input
	if err := json.Unmarshal(state, &restored); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(restored.Candidates)
	a, _ := Evaluate(input)
	b, _ := Evaluate(restored)
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if !bytes.Equal(ab, bb) {
		t.Fatalf("restore changed deterministic view: %s != %s", ab, bb)
	}
}

func TestBoundedReplayNeverWidensPolicy(t *testing.T) {
	pattern := testPattern(t, OriginalImport)
	fingerprint := pattern.ID
	inputs := []FrozenCase{{ID: "case-b", Complete: false}, {ID: "case-a", Complete: true, Fingerprint: fingerprint, Evidence: json.RawMessage(`{"snapshot":1}`)}}
	before := PolicySnapshot{Revision: 11, Frozen: json.RawMessage(`{"mode":"observe"}`)}
	after := PolicySnapshot{Revision: 12, Frozen: json.RawMessage(`{"mode":"assist"}`)}
	views := []View{{Pattern: pattern, Status: Recommended}}
	calls := 0
	evaluator := func(caseSnapshot FrozenCase, config PolicySnapshot, now time.Time) (Prediction, error) {
		calls++
		if caseSnapshot.ID != "case-a" || string(caseSnapshot.Evidence) != `{"snapshot":1}` || !now.Equal(clock) {
			t.Fatal("replay did not use exact frozen evidence and clock")
		}
		caseSnapshot.Evidence[0] = '_'
		if config.Revision == 11 {
			return Prediction{Reason: "safe_import_candidate", State: domain.CaseHeld, Holds: []domain.ReasonCode{"observe"}}, nil
		}
		return Prediction{Reason: "safe_import_candidate", State: domain.CaseProposed, AlreadyAllowed: []domain.Action{domain.ActionImport}}, nil
	}
	report, err := Replay(t.Context(), inputs, before, after, clock, evaluator, views)
	if err != nil || report.Evaluated != 1 || calls != 2 || report.Rows[0].Pattern != fingerprint || report.Rows[1].Skipped == "" ||
		Recommends(views[0], report.Rows[0].Before) || Recommends(views[0], Prediction{Reason: "safe_import_candidate"}) {
		t.Fatalf("unexpected replay: %+v %d %v", report, calls, err)
	}
	slices.Reverse(inputs)
	again, err := Replay(t.Context(), inputs, before, after, clock, evaluator, views)
	firstJSON, _ := json.Marshal(report)
	secondJSON, _ := json.Marshal(again)
	if err != nil || !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("replay was nondeterministic: %s %s %v", firstJSON, secondJSON, err)
	}
	if _, err := Replay(t.Context(), make([]FrozenCase, MaxReplayCases+1), before, after, clock, evaluator, views); !errors.Is(err, ErrInvalid) {
		t.Fatal("replay accepted unbounded cases")
	}
}

func TestPromotionRequiresDistinctUTCDateAndLineage(t *testing.T) {
	pattern := testPattern(t, OriginalImport)
	input := Input{Now: clock, Pattern: pattern, Current: pattern.Signature.Epochs}
	for id := 1; id <= 5; id++ {
		input.Candidates = append(input.Candidates, testCandidate(pattern, id, clock.Add(-time.Duration(id)*time.Hour)))
	}
	if view, err := Evaluate(input); err != nil || view.Counts.DistinctDays != 1 || view.Status != Collecting {
		t.Fatalf("one UTC day passed promotion threshold: %+v %v", view, err)
	}
	duplicateTarget := testCandidate(pattern, 10, clock.Add(-2*24*time.Hour))
	ref := input.Candidates[0].Sample.Target
	duplicateTarget.Sample.Target = ref
	duplicateTarget.Plan.Proposal.Operation.Targets[0] = ref
	duplicateTarget.Plan.Proposal.Operation.Effects[0].Target = ref
	duplicateTarget.Sample.Outcome.Effects[0].Effect.Target = ref
	duplicateTarget.MediaAssociation.Target = ref
	duplicateTarget.ImportHistory.Target = ref
	duplicateTarget.NoReversalThrough.Target = ref
	input.Candidates = append(input.Candidates, duplicateTarget)
	if view, err := Evaluate(input); err != nil || view.Counts.DistinctDays != 1 || view.Counts.Successes != 5 || view.Counts.Excluded != 1 {
		t.Fatalf("a new job with an already-counted lineage expanded the threshold: %+v %v", view, err)
	}
}
