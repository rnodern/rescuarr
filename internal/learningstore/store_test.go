package learningstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/learning"
	"github.com/rnodern/rescuarr/internal/store"
)

var now = time.Date(2026, 11, 14, 10, 30, 0, 0, time.UTC)

func openFixture(t *testing.T, dir string) (*store.Store, *Store) {
	t.Helper()
	local, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	s, err := New(local)
	if err != nil {
		t.Fatal(err)
	}
	return local, s
}

func patternFixture(t *testing.T, family learning.Family) learning.Fingerprint {
	t.Helper()
	inst := domain.InstanceRef{ID: "fixture", Epoch: 1}
	signature := learning.Signature{Version: learning.FingerprintVersion, Epochs: learning.Epochs{Installation: inst, Adapter: 1, Config: 1, Policy: 1},
		Product: domain.ProductRadarr, Client: learning.ClientSAB, Protocol: domain.ProtocolUsenet, Phase: learning.CompletedImport,
		Family: family, Reason: "safe_import_candidate", Shape: learning.SingleFile, UniqueMapping: true, StableComplete: true}
	if family == learning.IncidentCause {
		signature.Client, signature.Protocol, signature.Phase, signature.Shape = learning.ClientUnused, domain.ProtocolUnknown, learning.DependencyFault, learning.Dependency
		signature.Reason, signature.UniqueMapping, signature.StableComplete = "root_path_unavailable", false, false
		signature.CausalScope = domain.Scope{Instance: inst, Kind: domain.ScopeRoot, Key: "local-root"}
	}
	p, err := learning.BuildFingerprint(signature)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func candidateFixture(p learning.Fingerprint, id int, recorded time.Time) learning.Candidate {
	inst, scope := p.Signature.Epochs.Installation, p.Signature.CausalScope
	job := domain.JobRef{Instance: inst, ID: domain.JobID(fmt.Sprintf("job-%d", id)), Generation: 1}
	followup := domain.JobRef{Instance: inst, ID: "followup-" + job.ID, Generation: 1}
	target := domain.TargetRef{Instance: inst, Kind: domain.TargetMovie, ID: int64(id)}
	start := recorded.Add(-25 * time.Hour)
	proof := func(id domain.EvidenceID, at time.Time, job domain.JobRef) learning.Proof {
		return learning.Proof{Evidence: id, Observed: at, Job: job, Target: target, Scope: scope}
	}
	c := learning.Candidate{Sample: domain.PatternSample{Pattern: p.ID, Scope: scope, Job: job, Target: target, RecordedAt: recorded},
		StartedAt: start, IdentityVerified: true, QualifiedPlan: true, DispatchVerified: true}
	if p.Signature.Family == learning.IncidentCause {
		c.FollowupJob = followup
		c.HealthyDependency = proof("health", start.Add(time.Minute), job)
		c.IndependentRecovery = proof("independent", recorded, followup)
		return c
	}
	op := domain.Operation{Key: domain.OperationKey(job.ID), Job: job, Targets: []domain.TargetRef{target}, Action: domain.ActionImport,
		Effects: []domain.Effect{{Kind: domain.EffectLibraryImport, Target: target}, {Kind: domain.EffectSourceRetained}}}
	c.Plan = domain.Intent{ID: domain.IntentID(job.ID), Actor: "owner", RequestKey: "dropped-authorization-key", CreatedAt: start.Add(-time.Minute),
		Proposal: domain.Proposal{ID: domain.ProposalID(job.ID), CaseID: domain.CaseID(job.ID), CaseRevision: 1, PolicyRevision: 1,
			CapabilityRevision: 1, DecisionHash: "fixture-decision", Operation: op, ExpiresAt: start.Add(time.Minute)}}
	c.Attempt = domain.Attempt{ID: domain.AttemptID(job.ID), IntentID: c.Plan.ID, Operation: op, SentAt: start, Preflight: []domain.EvidenceID{"preflight"},
		Acknowledgement: domain.Acknowledgement{UpstreamReference: "/private/fixture"}}
	c.Sample.Treatment = op.Action
	c.Sample.Outcome = domain.Outcome{AttemptID: c.Attempt.ID, Verification: domain.VerificationEffect, ObservedAt: start.Add(3 * time.Minute),
		Effects: []domain.EffectResult{{Effect: op.Effects[0], Verification: domain.VerificationEffect, Evidence: []domain.EvidenceID{"media"}},
			{Effect: op.Effects[1], Verification: domain.VerificationEffect, Evidence: []domain.EvidenceID{"source"}}}}
	c.MediaAssociation = proof("media", start.Add(time.Minute), job)
	c.ImportHistory = proof("history", start.Add(2*time.Minute), job)
	c.NoReversalThrough = proof("stable", recorded, job)
	return c
}

func writeJournalFixture(t *testing.T, local *store.Store, c learning.Candidate, conflict bool) {
	t.Helper()
	encode := func(v any) string {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	if err := local.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO action_intents (id, proposal_id, actor, request_key, operation_key, proposal, physical_job, client, state, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 'fixture-physical-job', 'fixture-client', 'completed', ?)`, c.Plan.ID, c.Plan.Proposal.ID, c.Plan.Actor,
			c.Plan.ID, c.Plan.Proposal.Operation.Key, encode(c.Plan.Proposal), stamp(c.Plan.CreatedAt))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), `INSERT INTO action_attempts (id, intent_id, preflight, sent_at, outcome, conflicting_readback)
			VALUES (?, ?, ?, ?, ?, ?)`, c.Attempt.ID, c.Plan.ID, encode(struct{ Evidence []domain.EvidenceID }{c.Attempt.Preflight}),
			stamp(c.Attempt.SentAt), encode(c.Sample.Outcome), conflict)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func record(t *testing.T, s *Store, p learning.Fingerprint, c learning.Candidate, expected learning.SampleState) {
	t.Helper()
	result, err := s.Record(t.Context(), p, c, now)
	if err != nil || result.State != expected {
		t.Fatalf("record = %+v, %v; want %s", result, err, expected)
	}
}

func view(t *testing.T, s *Store, p learning.Fingerprint, at time.Time, want learning.Status) learning.View {
	t.Helper()
	result, err := s.View(t.Context(), p.ID, p.Signature.Epochs, at)
	if err != nil || result.Status != want {
		t.Fatalf("view status = %s, %v; want %s (%+v)", result.Status, err, want, result.Counts)
	}
	return result
}

func promote(t *testing.T, s *Store, p learning.Fingerprint) {
	t.Helper()
	for i := 1; i <= 5; i++ {
		candidate := candidateFixture(p, i, now.AddDate(0, 0, -(i%3)))
		record(t, s, p, candidate, learning.SampleSuccess)
		record(t, s, p, candidate, learning.SampleSuccess)
	}
}

func TestJournalProvenanceAndRedactedCandidate(t *testing.T) {
	local, s := openFixture(t, filepath.Join(t.TempDir(), "state"))
	p := patternFixture(t, learning.OriginalImport)
	absent := candidateFixture(p, 1, now)
	record(t, s, p, absent, learning.SampleNeutral)
	writeJournalFixture(t, local, absent, false)
	untied := absent
	untied.Plan.Actor = "another-owner"
	record(t, s, p, untied, learning.SampleNeutral)
	record(t, s, p, absent, learning.SampleSuccess)
	record(t, s, p, absent, learning.SampleSuccess)
	conflicting := candidateFixture(p, 2, now)
	writeJournalFixture(t, local, conflicting, true)
	record(t, s, p, conflicting, learning.SampleNeutral)
	rejected := candidateFixture(p, 3, now)
	rejected.MediaAssociation.Evidence = "https://secret.test/fixture"
	if _, err := s.Record(t.Context(), p, rejected, now); !errors.Is(err, learning.ErrInvalid) {
		t.Fatalf("unsafe evidence unexpectedly persisted: %v", err)
	}
	v := view(t, s, p, now, learning.Collecting)
	if v.Counts.Successes != 1 || v.Counts.Neutral != 2 || len(v.Samples) != 3 {
		t.Fatalf("unexpected journal eligibility: %+v", v.Counts)
	}
	if err := local.Read(t.Context(), func(tx *sql.Tx) error {
		var count int
		err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM learning_samples WHERE candidate LIKE '%private/fixture%' OR candidate LIKE '%dropped-authorization-key%'`).Scan(&count)
		if count != 0 {
			t.Fatal("private metadata appeared in a learning row")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRestartRestoreControlsAndPinnedReplay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	local, s := openFixture(t, dir)
	p := patternFixture(t, learning.IncidentCause)
	promote(t, s, p)
	if err := s.NoteMatch(t.Context(), p.ID, now); err != nil {
		t.Fatal(err)
	}
	v := view(t, s, p, now, learning.Recommended)
	if v.Counts.Successes != 5 || v.Counts.DistinctDays != 3 || len(v.Samples) != 5 || !v.LastMatchAt.Equal(now) || v.FirstSampleAt.IsZero() {
		t.Fatalf("unexpected promoted view: %+v", v)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	local, s = openFixture(t, dir)
	if reopened := view(t, s, p, now, learning.Recommended); !reflect.DeepEqual(v, reopened) {
		t.Fatalf("pattern changed on reopen: %+v", reopened)
	}
	epochs := p.Signature.Epochs
	drift := epochs
	drift.Adapter++
	if changed, err := s.View(t.Context(), p.ID, drift, now); err != nil || changed.Status != learning.Invalidated {
		t.Fatalf("adapter drift = %s, %v", changed.Status, err)
	}
	listed, err := s.List(t.Context(), epochs, now, "", 1)
	if err != nil || len(listed) != 1 || listed[0].Samples != nil {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	pinned, err := s.Snapshot(t.Context(), []domain.PatternID{p.ID}, epochs, now)
	if err != nil {
		t.Fatal(err)
	}
	cmd := learning.Command{Change: learning.Pause, Actor: "owner", Reason: "operator_request", Event: "fixture-pause", At: now}
	if updated, err := s.Apply(t.Context(), p.ID, cmd); err != nil || !updated.Paused || updated.Revision != 1 {
		t.Fatalf("apply pause = %+v, %v", updated, err)
	}
	if _, err := s.Apply(t.Context(), p.ID, cmd); !errors.Is(err, learning.ErrConflict) {
		t.Fatalf("stale control = %v", err)
	}
	cmd.ExpectedRevision, cmd.Change = 1, learning.Resume
	if _, err := s.Apply(t.Context(), p.ID, cmd); !errors.Is(err, learning.ErrConflict) {
		t.Fatalf("duplicate audit event = %v", err)
	}
	view(t, s, p, now, learning.Paused)
	cmd.Event = "fixture-resume"
	if _, err := s.Apply(t.Context(), p.ID, cmd); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "backup.sqlite3")
	if _, err := local.Backup(t.Context(), backupPath); err != nil {
		t.Fatal(err)
	}
	cmd.Change, cmd.ExpectedRevision, cmd.Event = learning.Reset, 2, "fixture-reset"
	if _, err := s.Apply(t.Context(), p.ID, cmd); err != nil {
		t.Fatal(err)
	}
	if reset := view(t, s, p, now, learning.Collecting); reset.Counts.Excluded != 5 || len(reset.Samples) != 5 {
		t.Fatalf("reset lost audit history: %+v", reset.Counts)
	}
	audit, err := s.Audit(t.Context(), p.ID, 1, 3)
	if err != nil || len(audit) != 2 || audit[1].Audit.Change != learning.Reset || audit[1].Audit.Event.ID != cmd.Event {
		t.Fatalf("control audit = %+v, %v", audit, err)
	}
	cases := []learning.FrozenCase{{ID: "case-a", Complete: true, Fingerprint: p.ID, Evidence: json.RawMessage(`{}`)}, {ID: "case-b"}}
	policyBefore, policyAfter := learning.PolicySnapshot{Revision: 1, Frozen: json.RawMessage(`{}`)}, learning.PolicySnapshot{Revision: 2, Frozen: json.RawMessage(`{}`)}
	eval := func(_ learning.FrozenCase, _ learning.PolicySnapshot, at time.Time) (learning.Prediction, error) {
		if !at.Equal(now) {
			return learning.Prediction{}, errors.New("unexpected evaluation time")
		}
		return learning.Prediction{State: domain.CaseHeld, Reason: p.Signature.Reason}, nil
	}
	replay := func(ctx context.Context) learning.ReplayReport {
		t.Helper()
		report, err := Replay(ctx, pinned, cases, policyBefore, policyAfter, eval)
		if err != nil || report.Total != 2 || report.Evaluated != 1 || report.Rows[0].Pattern != p.ID || report.Rows[1].Skipped != "replay_evidence_incomplete" {
			t.Fatalf("replay = %+v, %v", report, err)
		}
		return report
	}
	first := replay(t.Context())
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Restore(t.Context(), dir, backupPath, now); err != nil {
		t.Fatal(err)
	}
	_, s = openFixture(t, dir)
	restored := view(t, s, p, now, learning.Suspended)
	if restored.Explanation != "learning_restore_reconciliation_required" || restored.Controls.Revision != 2 || restored.Counts.Successes != 5 || !reflect.DeepEqual(first, replay(t.Context())) {
		t.Fatalf("restore/replay mismatch: %+v", restored)
	}
}

func TestRetentionProtectsQuarantineAndBoundsCapacity(t *testing.T) {
	local, s := openFixture(t, filepath.Join(t.TempDir(), "state"))
	p := patternFixture(t, learning.IncidentCause)
	for i := 1; i <= 5; i++ {
		at := now.AddDate(0, 0, -(i % 3))
		if _, err := s.Record(t.Context(), p, candidateFixture(p, i, at), at); err != nil {
			t.Fatal(err)
		}
	}
	contradiction := candidateFixture(p, 6, now)
	contradiction.Safety = learning.WrongTarget
	contradiction.SafetyEvidence = learning.Proof{Evidence: "unsafe-fixture", Observed: now, Job: contradiction.Sample.Job, Target: contradiction.Sample.Target}
	fixtureRows := func(at time.Time, count int) {
		t.Helper()
		if err := local.Write(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `WITH RECURSIVE fixture(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM fixture WHERE n < ?)
				INSERT INTO learning_samples (pattern_id, sample_key, state, recorded_at, available_at, candidate)
				SELECT ?, printf('%064x', n), 'neutral', ?, ?, '{}' FROM fixture`, count, p.ID, stamp(at), stamp(at))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	fixtureRows(now, learning.MaxCandidates-5)
	if _, err := s.Record(t.Context(), p, contradiction, now); !errors.Is(err, ErrCapacity) {
		t.Fatalf("full active window = %v", err)
	}
	view(t, s, p, now, learning.Quarantined)
	future := now.Add(2 * learning.SampleWindow)
	newCandidate := candidateFixture(p, 7, future)
	if _, err := s.Record(t.Context(), p, newCandidate, future); err != nil {
		t.Fatal(err)
	}
	if surviving := view(t, s, p, future, learning.Quarantined); len(surviving.Samples) != 2 {
		t.Fatalf("retention did not retain only current and latest verified evidence: %+v", surviving.Samples)
	}
	cmd := learning.Command{Change: learning.Reset, Actor: "owner", Reason: "operator_request", Event: "fixture-reset", At: future}
	if _, err := s.Apply(t.Context(), p.ID, cmd); err != nil {
		t.Fatal(err)
	}
	view(t, s, p, future, learning.Collecting)
}

func TestControlAuditFailureRollsBackChange(t *testing.T) {
	local, s := openFixture(t, filepath.Join(t.TempDir(), "state"))
	p := patternFixture(t, learning.IncidentCause)
	record(t, s, p, candidateFixture(p, 1, now), learning.SampleSuccess)
	if err := local.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER fail_fixture_audit BEFORE INSERT ON learning_control_audit
			BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cmd := learning.Command{Change: learning.Pause, Actor: "owner", Reason: "operator_request", Event: "fixture-failure", At: now}
	if _, err := s.Apply(t.Context(), p.ID, cmd); err == nil {
		t.Fatal("control succeeded without audit")
	}
	if v := view(t, s, p, now, learning.Collecting); v.Controls.Revision != 0 || v.Controls.Paused {
		t.Fatalf("failed audit advanced controls: %+v", v.Controls)
	}
	if err := local.Read(t.Context(), func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_events WHERE id = ?`, cmd.Event).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatal("failed audit left an orphan event")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
