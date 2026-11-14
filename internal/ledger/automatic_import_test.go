package ledger

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

func TestAutomaticImportSerialisesUntilVerifiedAcrossRestart(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "state")
	local, journal := openJournal(t, dir, fixtureAuthorize)
	first := separateScopeApproval("first")
	second := separateScopeApproval("second")
	for _, input := range []*Approval{&first, &second} {
		input.Actor, input.RequestKey = "", ""
	}
	setFixtureMode(t, local)
	if _, err := journal.AuthorizeAutomaticImport(ctx, first, fixtureTime); !errors.Is(err, ErrHeld) {
		t.Fatalf("assist mode authorised an unattended import: %v", err)
	}
	if err := local.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE app_state SET mode = 'autonomous'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	intent, err := journal.AuthorizeAutomaticImport(ctx, first, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := journal.AuthorizeAutomaticImport(ctx, first, fixtureTime)
	if err != nil || duplicate.ID != intent.ID {
		t.Fatalf("retry did not reuse the approval: %+v %v", duplicate, err)
	}
	if _, err := journal.AuthorizeAutomaticImport(ctx, second, fixtureTime); !errors.Is(err, ErrHeld) {
		t.Fatalf("second automatic operation started while first was queued: %v", err)
	}
	_, send := beginFixture(t, journal, intent)
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	local, journal = openJournal(t, dir, fixtureAuthorize)
	ready, err := journal.CanPlanAutomaticImport(ctx)
	if err != nil || ready {
		t.Fatalf("lost the active slot across restart: %v %v", ready, err)
	}
	if _, err := journal.AuthorizeAutomaticImport(ctx, second, fixtureTime.Add(time.Second)); !errors.Is(err, ErrHeld) {
		t.Fatalf("uncertain send did not preserve serial ordering: %v", err)
	}
	readback, err := journal.BeginReconcile(ctx, send.Attempt.ID, send.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := journal.RecordOutcome(ctx, readback, verifiedReadback(send, send.ExpiresAt))
	if err != nil || outcome.Verification != domain.VerificationEffect {
		t.Fatalf("import did not reconcile: %+v %v", outcome, err)
	}
	next, err := journal.AuthorizeAutomaticImport(ctx, second, send.ExpiresAt)
	if err != nil || next.ID == intent.ID {
		t.Fatalf("verified import did not make room for the next case: %+v %v", next, err)
	}
	assertActionAudit(t, local, "action_authorized", 2)
	assertActionAudit(t, local, "action_sending_recorded", 1)
	assertActionAudit(t, local, "action_readback_recorded", 1)
}
