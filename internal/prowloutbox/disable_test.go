package prowloutbox

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/prowl"
)

func TestDisableWithUnavailableKey(t *testing.T) {
	for _, keyState := range []string{"missing", "invalid"} {
		t.Run(keyState, func(t *testing.T) {
			db, dir, clock := outboxFixture(t)
			s := New(db, clock, nil, false)
			route := routeFixture(t, s, Route{Categories: Attention, Timezone: "Australia/Melbourne", QuietStart: 60, QuietEnd: 120, Digest: true})
			appendEvent(t, s, "ready-to-send", "action_required", clock.Now())
			appendEvent(t, s, "will-retry", "action_unresolved", clock.Now())
			keep := routeFixture(t, s, Route{Categories: Incident})
			appendEvent(t, s, "another-route", "incident_opened", clock.Now())
			if err := db.Write(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.Exec(`UPDATE notification_outbox SET state = 'delayed', attempts = 1 WHERE event_id = 'will-retry'`)
				return err
			}); err != nil {
				t.Fatal(err)
			}

			keyPath := filepath.Join(dir, "secrets", "fixture")
			privateKey := "invalid-key-private-marker"
			switch keyState {
			case "missing":
				if err := os.Remove(keyPath); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(keyPath, []byte(privateKey), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if got, err := s.Disable(t.Context(), route.ID, route.Revision+1, "owner"); !errors.Is(err, app.ErrConflict) || got != (Route{}) {
				t.Fatalf("stale disable returned %+v, %v", got, err)
			}
			statuses, err := s.Recent(t.Context(), route.ID, 10)
			if err != nil || len(statuses) != 2 || statuses[0].State != domain.OutboxDelayed || statuses[1].State != domain.OutboxPending {
				t.Fatalf("stale disable changed pending deliveries: %+v, %v", statuses, err)
			}

			disabled, err := s.Disable(t.Context(), route.ID, route.Revision, "owner")
			expected := route
			expected.Enabled = false
			expected.Revision++
			if err != nil || disabled != expected {
				t.Fatalf("disable returned %+v, %v", disabled, err)
			}
			statuses, err = s.Recent(t.Context(), route.ID, 10)
			if err != nil || len(statuses) != 2 {
				t.Fatalf("disable lost delivery records: %+v, %v", statuses, err)
			}
			for _, status := range statuses {
				if status.State != domain.OutboxSuppressed || status.Reason != "notification_route_changed" {
					t.Fatalf("pending delivery was not suppressed: %+v", status)
				}
			}
			other, err := s.Recent(t.Context(), keep.ID, 10)
			if err != nil || len(other) != 1 || other[0].State != domain.OutboxPending {
				t.Fatalf("disable changed another route: %+v, %v", other, err)
			}
			if _, err := s.QueueTest(t.Context(), route.ID, disabled.Revision, "owner"); !errors.Is(err, ErrDisabled) {
				t.Fatalf("disabled route test returned %v", err)
			}
			appendEvent(t, s, "after-disable", "action_required", clock.Now())
			if got, err := s.Recent(t.Context(), route.ID, 10); err != nil || len(got) != 2 {
				t.Fatalf("disabled route queued new work: %+v, %v", got, err)
			}

			expected.Enabled = true
			if _, err := s.Configure(t.Context(), ConfigureRoute{Route: expected, KeyRef: "fixture", Actor: "owner"}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("enable with unavailable key returned %v", err)
			}
			if err := db.Read(t.Context(), func(tx *sql.Tx) error {
				var enabled bool
				if err := tx.QueryRow(`SELECT enabled FROM prowl_routes WHERE id = ?`, route.ID).Scan(&enabled); err != nil {
					return err
				}
				if enabled {
					return errors.New("route was enabled without a valid key")
				}
				var count int
				var audit string
				if err := tx.QueryRow(`SELECT count(*), group_concat(actor || reason || scope_key || evidence)
					FROM audit_events WHERE kind = 'notification_route_disabled'`).Scan(&count, &audit); err != nil {
					return err
				}
				if count != 1 || !strings.HasPrefix(audit, "ownernotification_route_disabled") ||
					strings.Contains(audit, privateKey) || strings.Contains(audit, testKey) || strings.Contains(audit, keyPath) ||
					strings.Contains(audit, "fixture") {
					return errors.New("expected a single private disable event")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDisableRollsBackIfAuditFails(t *testing.T) {
	db, _, clock := outboxFixture(t)
	s := New(db, clock, nil, false)
	route := routeFixture(t, s, Route{})
	appendEvent(t, s, "pending-at-disable", "action_required", clock.Now())
	clock.at = time.Time{} // The audit writer rejects a missing timestamp.
	if got, err := s.Disable(t.Context(), route.ID, route.Revision, "owner"); err == nil || got != (Route{}) {
		t.Fatalf("disable without audit returned %+v, %v", got, err)
	}
	routes, err := s.Routes(t.Context())
	if err != nil || len(routes) != 1 || routes[0] != route {
		t.Fatalf("failed disable changed route: %+v, %v", routes, err)
	}
	statuses, err := s.Recent(t.Context(), route.ID, 1)
	if err != nil || len(statuses) != 1 || statuses[0].State != domain.OutboxPending {
		t.Fatalf("failed disable changed delivery: %+v, %v", statuses, err)
	}
}

func TestDisableLinearisesWithClaim(t *testing.T) {
	for _, tc := range []struct {
		name        string
		claimBefore bool
		result      prowl.Result
		want        domain.OutboxState
	}{
		{"disable first prevents claim", false, prowl.Result{}, domain.OutboxSuppressed},
		{"claim first keeps acceptance", true, prowl.Result{Status: prowl.Accepted}, domain.OutboxAccepted},
		{"claim first cannot retry", true, prowl.Result{Status: prowl.Transient}, domain.OutboxSuppressed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _, clock := outboxFixture(t)
			s := New(db, clock, nil, false)
			route := routeFixture(t, s, Route{})
			queued, err := s.QueueTest(t.Context(), route.ID, route.Revision, "owner")
			if err != nil {
				t.Fatal(err)
			}
			var work claim
			if tc.claimBefore {
				work, err = s.claim(t.Context(), clock.Now())
				if err != nil || work.ID != queued {
					t.Fatal("expected delivery claim")
				}
			}
			if _, err := s.Disable(t.Context(), route.ID, route.Revision, "owner"); err != nil {
				t.Fatal(err)
			}
			if tc.claimBefore {
				statuses, err := s.Recent(t.Context(), route.ID, 1)
				if err != nil || len(statuses) != 1 || statuses[0].State != domain.OutboxSending {
					t.Fatalf("disable discarded in-flight claim: %+v, %v", statuses, err)
				}
				if err := s.complete(t.Context(), work, clock.Now(), tc.result, ""); err != nil {
					t.Fatal(err)
				}
			}
			if next, err := s.claim(t.Context(), clock.Now()); err != nil || next.ID != "" {
				t.Fatalf("disabled route remained claimable: %v", err)
			}
			statuses, err := s.Recent(t.Context(), route.ID, 1)
			if err != nil || len(statuses) != 1 || statuses[0].State != tc.want {
				t.Fatalf("unexpected final delivery status: %+v, %v", statuses, err)
			}
			if tc.result.Status == prowl.Transient {
				if err := db.Read(t.Context(), func(tx *sql.Tx) error {
					var original domain.OutboxState
					if err := tx.QueryRow(`SELECT state FROM prowl_attempts WHERE outbox_id = ?`, queued).Scan(&original); err != nil {
						return err
					}
					if original != domain.OutboxDelayed {
						return errors.New("suppression lost the original attempt outcome")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestClaimHoldsDuringReconciliation(t *testing.T) {
	db, _, clock := outboxFixture(t)
	s := New(db, clock, nil, false)
	route := routeFixture(t, s, Route{})
	queued, err := s.QueueTest(t.Context(), route.ID, route.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE app_state SET reconcile_required = 1 WHERE singleton = 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if work, err := s.claim(t.Context(), clock.Now()); err != nil || work.ID != "" {
		t.Fatalf("reconciliation exposed a delivery claim: %v", err)
	}
	statuses, err := s.Recent(t.Context(), route.ID, 1)
	if err != nil || len(statuses) != 1 || statuses[0].ID != queued || statuses[0].State != domain.OutboxPending || statuses[0].Attempts != 0 {
		t.Fatalf("reconciliation claim guard mutated delivery: %+v, %v", statuses, err)
	}
}
