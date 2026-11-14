package recoveryservice

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/ledger"
	"github.com/rnodern/rescuarr/internal/policy"
	"github.com/rnodern/rescuarr/internal/store"
)

type automaticCases []domain.Case

func (q automaticCases) AutomaticImportCases(_ context.Context, after domain.CaseID, limit int) ([]domain.Case, error) {
	var items []domain.Case
	for _, item := range q {
		if item.ID > after && len(items) < limit {
			items = append(items, item)
		}
	}
	return items, nil
}

func TestAutomaticImportAdmitsQualifiedCasesOnly(t *testing.T) {
	ctx := t.Context()
	local, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	clock := app.FixedClock{At: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
	journal, err := ledger.NewJournal(local, func(context.Context, *sql.Tx, ledger.Approval, time.Time) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.InstanceRef{ID: "sonarr", Epoch: 1}
	target := domain.TargetRef{Instance: ref, Kind: domain.TargetEpisode, SeriesID: 1, ID: 2}
	cases := automaticCases{
		{ID: "first", Revision: 1, Job: domain.Job{Ref: domain.JobRef{Instance: ref, ID: "job-first", Generation: 1}}},
		{ID: "next", Revision: 1, Job: domain.Job{Ref: domain.JobRef{Instance: ref, ID: "job-next", Generation: 1}}},
		{ID: "third", Revision: 1, Job: domain.Job{Ref: domain.JobRef{Instance: ref, ID: "job-third", Generation: 1}}},
	}
	var seen []string
	plan := func(_ context.Context, item domain.Case) (policy.Result, string, string, error) {
		seen = append(seen, string(item.ID))
		proposal := &domain.Proposal{
			ID: "proposal-" + domain.ProposalID(item.ID), CaseID: item.ID, CaseRevision: 1,
			PolicyRevision: 1, CapabilityRevision: 1, DecisionHash: "decision-" + string(item.ID),
			Guards: []domain.Guard{{Reason: "fixture_qualified", Passed: true}}, ExpiresAt: clock.Now().Add(time.Minute),
			Operation: domain.Operation{Key: "key-" + domain.OperationKey(item.ID), Action: domain.ActionImport,
				Job: item.Job.Ref, Targets: []domain.TargetRef{target},
				Effects: []domain.Effect{{Kind: domain.EffectLibraryImport, Target: target}, {Kind: domain.EffectSourceRetained}}},
		}
		authority := policy.AuthorityAutonomous
		if item.ID == "first" {
			authority = policy.AuthorityAssist
		}
		return policy.Result{Authority: authority, Path: policy.ImportOriginal, Proposal: proposal}, "fixture-job", "fixture-client", nil
	}
	var after domain.CaseID
	pass := func() {
		t.Helper()
		if err := admitAutomaticImport(ctx, cases, plan, journal, clock, &after); err != nil {
			t.Fatal(err)
		}
	}
	pass()
	if len(seen) != 0 {
		t.Fatal("Observe performed automatic planning")
	}
	if err := local.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE app_state SET mode = 'autonomous'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pass()
	pass()
	pass()
	if strings.Join(seen, ",") != "first,next" {
		t.Fatalf("expected one-by-one admission past a review-only case: %v", seen)
	}
	if err := local.Read(ctx, func(tx *sql.Tx) error {
		var events int
		var caseID domain.CaseID
		err := tx.QueryRowContext(ctx, "SELECT count(*), max(case_id) FROM audit_events WHERE kind = 'action_authorized'").Scan(&events, &caseID)
		if err == nil && (events != 1 || caseID != "next") {
			t.Errorf("expected one durable admission for next: %d %s", events, caseID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
