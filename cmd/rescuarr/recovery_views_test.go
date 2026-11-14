package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/app/apptest"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

func TestRecoveryViewsReadOnlyCommittedScopedOutcomes(t *testing.T) {
	ctx := t.Context()
	local, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	const caseID domain.CaseID = "case-a"
	now := time.Unix(9000, 0).UTC()
	views := recoveryViews{local: local, ConsoleReader: &apptest.Console{
		Dashboard: app.Overview{Mode: domain.ModeObserve, Cases: 2},
		Details:   map[domain.CaseID]app.CaseDetail{caseID: {Case: domain.Case{ID: caseID}}},
	}}
	err = local.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE action_pause SET paused = 1"); err != nil {
			return err
		}
		for _, testCase := range []domain.CaseID{caseID, "case-b"} {
			proposal, err := json.Marshal(domain.Proposal{CaseID: testCase})
			if err != nil {
				return err
			}
			id := "intent-" + string(testCase)
			if _, err := tx.ExecContext(ctx, `INSERT INTO action_intents
				(id, proposal_id, actor, request_key, operation_key, proposal, physical_job, client, state, created_at)
				VALUES (?, ?, 'owner', ?, ?, ?, 'fixture-job', 'fixture-client', 'completed', ?)`,
				id, id, "private-request-"+id, id, string(proposal), now.Format(time.RFC3339Nano)); err != nil {
				return err
			}
			outcome, err := json.Marshal(domain.Outcome{AttemptID: domain.AttemptID(id), Verification: domain.VerificationEffect, ObservedAt: now})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO action_attempts (id, intent_id, preflight, sent_at, outcome)
				VALUES (?, ?, '{}', ?, ?)`, id, id, now.Format(time.RFC3339Nano), string(outcome)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := views.Overview(ctx)
	if err != nil || !status.Paused || status.Cases != 2 {
		t.Fatalf("overview did not include the committed pause state: %v", err)
	}
	detail, err := views.Case(ctx, caseID)
	if err != nil || len(detail.Outcomes) != 1 || detail.Outcomes[0].AttemptID != "intent-case-a" {
		t.Fatalf("case did not show only its committed outcome: %v", err)
	}
	intent, err := views.Intent(ctx, "intent-case-a")
	if err != nil || intent.State != domain.IntentCompleted || intent.Proposal.CaseID != caseID || intent.RequestKey != "" {
		t.Fatalf("intent did not show safe committed data: %v", err)
	}
	if _, err := views.Intent(ctx, "missing"); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("missing intent returned %v", err)
	}
}
