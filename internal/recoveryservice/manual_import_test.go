package recoveryservice

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/importflow"
	"github.com/rnodern/rescuarr/internal/observe"
)

func TestManualImportReservationIsDurableAndUsesCurrentLanguage(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "state")
	local := openEmptyPackFixture(t, dir)
	t.Cleanup(func() { local.Close() })
	key, _ := emptyPackFixture()
	registerEmptyPackInstance(t, local, key.Job.Instance)
	writeEmptyPackFixture(t, local, "UPDATE app_state SET mode = 'assist'")
	writeEmptyPackFixture(t, local, `INSERT INTO observe_cases (id,instance_id,epoch,correlation_key,observation_only,
		fingerprint,state,reason,revision,payload,last_seen_generation,updated_at)
		VALUES (?, ?, 1, 'import-fixture', 0, 'fixture', 'held', 'series_matched_by_id', 5, '{}', 1, '')`, key.CaseID, key.Job.Instance.ID)
	writeEmptyPackFixture(t, local, `INSERT INTO manual_reviews
		(case_id,token,state,case_revision,scope_hash,updated_at,action)
		VALUES (?, 'fixture-token','approved',5,'fixture-scope','','import')`, key.CaseID)
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	clock := app.FixedClock{At: now}
	reviews := &ManualReviews{local: local, clock: clock}
	instance := domain.Instance{Ref: key.Job.Instance, Product: domain.ProductSonarr,
		ConnectionRevision: 1, UpstreamVersion: arrwire.SonarrIDOnlyImportVersion}
	target := domain.TargetRef{Instance: instance.Ref, Kind: domain.TargetEpisode, SeriesID: 3, ID: 17}
	source := importflow.SonarrV5Source{Instance: instance, Job: key.Job, Target: target,
		DownloadID: "fixture-download", Client: "fixture-client", OutputPath: "/downloads/fixture",
		Path: "/downloads/fixture/fixture.mkv", Size: 1024}
	plan := manualImportPlan{instance: instance, source: source, job: importflow.Job{Queue: observe.QueueSnapshot{
		Evidence: domain.Observation{Complete: true, State: domain.ObservationKnown, ObservedAt: now, ExpiresAt: now.Add(time.Minute)},
	}}}
	approval := manualImportApproval{manualSlot: manualSlot{id: key.CaseID, token: "fixture-token"},
		revision: 5, scope: "fixture-scope", approvedAt: now}
	resolution, flags := int64(720), uint64(0)
	prepared := domain.PreparedImport{DownloadID: source.DownloadID, Files: []domain.PreparedImportFile{{
		Path: source.Path, FolderName: "fixture", Targets: []domain.TargetRef{target},
		QualityID: 1, QualitySource: "web", QualityName: "WEB-DL", QualityResolution: &resolution,
		Quality: domain.ImportQualityRevision{Version: 1, Observed: true}, IndexerFlags: &flags,
		Languages: []domain.ImportLanguage{{ID: 1, Name: "English"}},
	}}}
	record := newManualImportCheckpoint(plan, prepared, "fixture-adapter", now)
	preferences, err := app.NewLanguageSettings(local, clock).SaveLanguageSettings(ctx, app.DefaultLanguagePreferences(), "fixture-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := reviews.reserveManualImport(ctx, approval, plan, record); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("stale audio policy reached reservation: %v", err)
	}
	plan.languageRevision = preferences.Revision
	if err := reviews.reserveManualImport(ctx, approval, plan, record); err != nil {
		t.Fatal(err)
	}
	if err := reviews.allowManualImportSend(ctx, approval, plan, record.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	writeEmptyPackFixture(t, local, "UPDATE action_pause SET paused = 1")
	if err := reviews.allowManualImportSend(ctx, approval, plan, record.Attempt.ID); !errors.Is(err, app.ErrHeld) {
		t.Fatalf("paused approval reached dispatch: %v", err)
	}
	if err := reviews.cancelUnsentManualImport(ctx, approval.manualSlot, record.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	if err := reviews.reserveManualImport(ctx, approval, plan, record); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("cancelled approval could be used again: %v", err)
	}
	if err := local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT token FROM manual_reviews WHERE case_id = ? AND state = 'pending'`, key.CaseID).Scan(&approval.token)
	}); err != nil {
		t.Fatal(err)
	}
	writeEmptyPackFixture(t, local, "UPDATE action_pause SET paused = 0")
	writeEmptyPackFixture(t, local, "UPDATE manual_reviews SET state = 'approved', scope_hash = 'fixture-scope' WHERE token = ?", approval.token)
	record = newManualImportCheckpoint(plan, prepared, "fixture-adapter", now)
	if err := reviews.reserveManualImport(ctx, approval, plan, record); err != nil {
		t.Fatal(err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	local = openEmptyPackFixture(t, dir)
	reviews.local = local
	saved, err := reviews.loadManualImport(ctx, approval.manualSlot)
	if err != nil || saved.stage != "sending" || saved.Attempt.ID != record.Attempt.ID || saved.Prepared.Import.Files[0].Path != source.Path {
		t.Fatalf("private import reservation failed to survive restart: %+v %v", saved, err)
	}
	if err := reviews.reserveManualImport(ctx, approval, plan, record); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("same review could dispatch twice: %v", err)
	}
	if err := reviews.recordManualImport(ctx, approval.manualSlot, saved, "verified", "recovery_manual_import_verified"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT state FROM manual_reviews WHERE case_id = ?`, key.CaseID).Scan(&state)
	}); err != nil || state != "completed" {
		t.Fatalf("verified Copy did not release the review slot: %q %v", state, err)
	}
	assertEmptyPackAudit(t, local, "language_preferences_saved", "recovery_manual_import_sending",
		"recovery_manual_import_cancelled", "recovery_manual_import_sending", "recovery_manual_import_verified")
}
