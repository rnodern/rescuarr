package arradapter_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/app/apptest"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/ledger"
	"github.com/rnodern/rescuarr/internal/recoveryservice"
	"github.com/rnodern/rescuarr/internal/store"
	"github.com/rnodern/rescuarr/internal/webconsole"
)

func TestFencedFixtureLostAcknowledgementReconcilesAfterRestart(t *testing.T) {
	cfg, op, prepared := fixture(domain.ProductRadarr, domain.ActionImport)
	dir := filepath.Join(t.TempDir(), "state")
	local, journal := fixtureJournal(t, dir)
	if err := local.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "UPDATE app_state SET mode = 'assist' WHERE singleton = 1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	approval := ledger.Approval{
		Actor: "fixture-owner", RequestKey: "fixture-request", PhysicalJob: "fixture-physical-job", Client: "fixture-client",
		Proposal: domain.Proposal{
			ID: "fixture-proposal", CaseID: "fixture-case", CaseRevision: 1, PolicyRevision: 2, CapabilityRevision: 3,
			DecisionHash: "fixture-decision", Operation: op, Guards: []domain.Guard{{Reason: "fixture_guard", Passed: true}},
			ExpiresAt: fixtureTime.Add(time.Minute),
		},
	}
	intent := submitFixtureReview(t, journal, approval)
	gate := fixtureGate(func(_ context.Context, current domain.Intent) (ledger.Preflight, error) {
		p := current.Proposal
		return ledger.Preflight{DecisionHash: p.DecisionHash, CaseRevision: p.CaseRevision, PolicyRevision: p.PolicyRevision,
			CapabilityRevision: p.CapabilityRevision, Evidence: []domain.EvidenceID{"fixture-guard"}, Prepared: &prepared,
			ObservedAt: fixtureTime, ExpiresAt: fixtureTime.Add(time.Second)}, nil
	})
	exchanges := 0
	adapter := newAdapter(t, cfg, func(req *http.Request) (*http.Response, error) {
		exchanges++
		err := local.Read(req.Context(), func(tx *sql.Tx) error {
			var state, private string
			if err := tx.QueryRowContext(req.Context(), `SELECT i.state, a.preflight FROM action_intents AS i
				JOIN action_attempts AS a ON a.intent_id = i.id WHERE i.id = ?`, intent.ID).Scan(&state, &private); err != nil {
				return err
			}
			if state != string(domain.IntentSending) || !strings.Contains(private, prepared.Import.Files[0].Path) {
				t.Fatal("exchange preceded the committed send marker and exact selector")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return nil, io.ErrUnexpectedEOF
	}, positive, unexpectedReadback(t), fixedTime)
	executor, err := ledger.NewExecutor(journal, adapter, gate, fixtureClock(fixtureTime))
	if err != nil {
		t.Fatal(err)
	}
	id, err := executor.Dispatch(t.Context(), intent.ID)
	if err != nil || id == "" || exchanges != 1 {
		t.Fatalf("unexpected dispatch after %d exchanges: %q, %v", exchanges, id, err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}

	_, reopened := fixtureJournal(t, dir)
	readTime := fixtureTime.Add(2 * time.Second)
	reads := 0
	readAdapter := newAdapter(t, cfg, unexpectedExchange(t), func(context.Context, arradapter.Binding) (arradapter.Qualification, error) {
		t.Fatal("recovery called dispatch qualification")
		return arradapter.Qualification{}, nil
	}, func(_ context.Context, route arradapter.Route, attempt domain.Attempt) (domain.Outcome, error) {
		reads++
		if route != arradapter.RouteCopyImport || attempt.ID != id || attempt.Prepared == nil ||
			attempt.Prepared.Import.Files[0].Path != prepared.Import.Files[0].Path ||
			attempt.Acknowledgement != (domain.Acknowledgement{State: domain.DispatchUncertain, Reason: "dispatch_uncertain"}) {
			t.Fatal("reopened journal lost the uncertain acknowledgement or exact selectors")
		}
		return domain.Outcome{AttemptID: id, ObservedAt: readTime, Effects: []domain.EffectResult{
			{Effect: op.Effects[0], Verification: domain.VerificationEffect, Evidence: []domain.EvidenceID{"fixture-library", "fixture-history"}},
			{Effect: op.Effects[1], Verification: domain.VerificationEffect, Evidence: []domain.EvidenceID{"fixture-source"}},
		}}, nil
	}, func() time.Time { return readTime })
	restarted, err := ledger.NewExecutor(reopened, readAdapter, gate, fixtureClock(readTime))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Dispatch(t.Context(), intent.ID); !errors.Is(err, ledger.ErrConflict) && !errors.Is(err, ledger.ErrHeld) {
		t.Fatalf("reopened journal permitted another send: %v", err)
	}
	outcome, err := restarted.Reconcile(t.Context(), id)
	if err != nil || outcome.AttemptID != id || outcome.Verification != domain.VerificationEffect ||
		len(outcome.Effects) != 2 || exchanges != 1 || reads != 1 {
		t.Fatalf("unexpected reconciliation after %d exchanges and %d reads: %+v, %v", exchanges, reads, outcome, err)
	}
}

type reviewedProposal ledger.Approval

func (p reviewedProposal) FindApproval(context.Context, domain.CaseID, domain.ProposalID, domain.Revision, time.Time) (ledger.Approval, error) {
	return ledger.Approval(p), nil
}

var approvalField = regexp.MustCompile(`name="(proposal_id|revision|request_key|csrf)" value="([^"]+)"`)

func submitFixtureReview(t *testing.T, journal *ledger.Journal, approval ledger.Approval) domain.Intent {
	t.Helper()
	p := approval.Proposal
	caseProjection := domain.Case{ID: p.CaseID, Revision: p.CaseRevision}
	origin := "https://rescuarr.invalid"
	browser := webconsole.New(webconsole.Options{
		Read: &apptest.Console{
			Dashboard: app.Overview{Mode: domain.ModeAssist},
			Details: map[domain.CaseID]app.CaseDetail{
				p.CaseID: {Case: caseProjection, Proposals: []domain.Proposal{p}},
			},
		},
		Clock: fixtureClock(fixtureTime),
		Commands: recoveryservice.Commands{
			Journal: journal, Lookup: reviewedProposal(approval), Clock: fixtureClock(fixtureTime),
		},
		Authorize: func(*http.Request) webconsole.Session {
			return webconsole.Session{Actor: approval.Actor, CSRF: "fixture-csrf", Origin: origin}
		},
	})
	casePath := origin + "/cases/" + string(p.CaseID)
	view := httptest.NewRecorder()
	browser.ServeHTTP(view, httptest.NewRequest(http.MethodGet, casePath+"/review/"+string(p.ID), nil))
	fields := url.Values{}
	for _, f := range approvalField.FindAllStringSubmatch(view.Body.String(), -1) {
		fields.Set(f[1], f[2])
	}
	if view.Code != http.StatusOK || len(fields) != 4 {
		t.Fatal("owner review form did not render")
	}
	var location string
	for range 2 {
		submission := httptest.NewRequest(http.MethodPost, casePath+"/actions", strings.NewReader(fields.Encode()))
		submission.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		submission.Header.Set("Origin", origin)
		result := httptest.NewRecorder()
		browser.ServeHTTP(result, submission)
		if result.Code != http.StatusSeeOther || !strings.HasPrefix(result.Header().Get("Location"), "/actions/") {
			t.Fatalf("owner approval returned HTTP %d", result.Code)
		}
		if next := result.Header().Get("Location"); location != "" && next != location {
			t.Fatal("repeat submitted approval created a second intent")
		} else {
			location = next
		}
	}
	return domain.Intent{ID: domain.IntentID(strings.TrimPrefix(location, "/actions/"))}
}

type fixtureGate func(context.Context, domain.Intent) (ledger.Preflight, error)

func (g fixtureGate) Check(ctx context.Context, intent domain.Intent) (ledger.Preflight, error) {
	return g(ctx, intent)
}

type fixtureClock time.Time

func (c fixtureClock) Now() time.Time { return time.Time(c) }

func fixtureJournal(t *testing.T, dir string) (*store.Store, *ledger.Journal) {
	t.Helper()
	local, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	journal, err := ledger.NewJournal(local, func(context.Context, *sql.Tx, ledger.Approval, time.Time) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return local, journal
}
