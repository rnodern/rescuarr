package observeservice

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
)

func TestConnectionRemovalClosesOpenJobsAndRetainsHistory(t *testing.T) {
	db, dir := testStore(t)
	calls := 0
	service := New(db, nil, fixture(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.Header.Get("X-Api-Key") != credential {
			t.Fatal("the pasted key did not authorise a read")
		}
		switch r.URL.Path {
		case "/arr/api/v3/system/status":
			return response(200, `{"appName":"Sonarr","version":"4.0.20.3014"}`)
		case "/arr/api/v3/queue":
			return response(200, queue(sonarrPack, 2))
		default:
			t.Fatalf("unexpected upstream route %s", r.URL.Path)
			return nil, nil
		}
	}))
	input := InstanceInput{Product: domain.ProductSonarr, DisplayName: "Local Sonarr",
		BaseURL: "http://sonarr.invalid/arr", APIKey: " " + credential + "\n", Actor: "owner"}
	instance, err := service.AddInstance(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := db.ReadSecret(instance.SecretRef)
	if err != nil || secret != credential || calls != 0 {
		t.Fatal("pasted credential was not stored privately or produced an upstream request")
	}
	if _, err := service.AddInstance(t.Context(), input); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate pasted-key connection returned %v", err)
	}
	if files, err := os.ReadDir(filepath.Join(dir, "secrets")); err != nil || len(files) != 2 {
		t.Fatal("duplicate connection left an unused credential file")
	}
	if err := service.Collect(t.Context(), instance.Ref.ID); err != nil {
		t.Fatal(err)
	}
	page, err := service.Cases(t.Context(), app.CaseQuery{})
	if err != nil || len(page.Cases) != 1 || page.Cases[0].State != domain.CaseHeld {
		t.Fatalf("expected one open job before removing connection: %+v, %v", page, err)
	}
	open := page.Cases[0]
	release, err := service.HoldRecoveryConnection(t.Context(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveInstance(t.Context(), instance.Ref.ID, input.Actor); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("removal during recovery read returned %v", err)
	}
	release()
	if err := service.RemoveInstance(t.Context(), instance.Ref.ID, input.Actor); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", instance.SecretRef)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removed connection retained its pasted credential")
	}
	if _, err := db.ReadSecret("fixture"); err != nil {
		t.Fatalf("existing local file credential was altered: %v", err)
	}
	if instances, err := service.Instances(t.Context()); err != nil || len(instances) != 0 {
		t.Fatalf("removed connection was still registered: %+v, %v", instances, err)
	}
	if err := service.Collect(t.Context(), instance.Ref.ID); !errors.Is(err, app.ErrNotFound) || calls != 2 {
		t.Fatalf("removed connection made an upstream read or unexpectedly collected: %d calls, %v", calls, err)
	}
	if _, err := service.HoldRecoveryConnection(t.Context(), instance); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("recovery read after removal returned %v", err)
	}
	closed, err := service.Case(t.Context(), open.ID)
	if err != nil || closed.Case.State != domain.CaseClosed || closed.Case.Reason != "connection_removed" ||
		closed.Case.Revision != open.Revision+1 || closed.Case.Job.CompleteSet || !closed.Case.NextCheckAt.IsZero() {
		t.Fatalf("historical job was not closed correctly: %+v, %v", closed, err)
	}
	activity, err := service.RecoveryActivity(t.Context(), 5, 0)
	if err != nil || len(activity) != 1 || activity[0].CaseID != open.ID || activity[0].Kind != "recovery_connection_removed" {
		t.Fatalf("missing removal activity: %+v, %v", activity, err)
	}
	readded, err := service.AddInstance(t.Context(), input)
	if err != nil || readded.Ref.ID == instance.Ref.ID {
		t.Fatalf("failed to re-add the same service as a fresh connection: %v", err)
	}
}

func TestConnectionRemovalKeepsLegacyCredentialsAndRejectsActiveIntent(t *testing.T) {
	db, _ := testStore(t)
	service := New(db, nil, nil)
	instance := add(t, service, domain.ProductSonarr)
	proposal, err := json.Marshal(domain.Proposal{Operation: domain.Operation{Job: domain.JobRef{Instance: instance.Ref}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO action_intents (id, proposal_id, actor, request_key, operation_key,
			proposal, physical_job, client, state, created_at) VALUES ('in-flight', 'plan', 'owner', 'request', 'job', ?, 'job', 'client', 'sending', '')`,
			string(proposal))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveInstance(t.Context(), instance.Ref.ID, "owner"); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("removal with a live import returned %v", err)
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE action_intents SET state = 'completed' WHERE id = 'in-flight'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := json.Marshal(struct{ Attempt domain.Attempt }{Attempt: domain.Attempt{
		Operation: domain.Operation{Job: domain.JobRef{Instance: instance.Ref}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO manual_reviews
			(case_id,token,state,case_revision,updated_at,action) VALUES ('copy-case','copy-token','approved',1,'','import')`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `INSERT INTO manual_import_attempts (case_id,token,stage,payload)
			VALUES ('copy-case','copy-token','uncertain',?)`, string(attempt))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveInstance(t.Context(), instance.Ref.ID, "owner"); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("removal with an unconfirmed Manual Copy returned %v", err)
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `UPDATE manual_reviews SET state = 'completed' WHERE case_id = 'copy-case'`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE manual_import_attempts SET stage = 'unconfirmed' WHERE case_id = 'copy-case'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveInstance(t.Context(), instance.Ref.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if secret, err := db.ReadSecret(instance.SecretRef); err != nil || secret != credential {
		t.Fatal("remove deleted the existing file-based credential")
	}
}
