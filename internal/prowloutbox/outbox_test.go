package prowloutbox

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/prowl"
	"github.com/rnodern/rescuarr/internal/store"
)

const testKey = "0123456789abcdef0123456789abcdef01234567"

type stampClock struct{ at time.Time }

func (c *stampClock) Now() time.Time { return c.at }

type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func outboxFixture(t *testing.T) (*store.Store, string, *stampClock) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	db, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := os.Mkdir(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets", "fixture"), []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return db, dir, &stampClock{at: time.Date(2026, 11, 14, 12, 30, 0, 0, time.UTC)}
}

func routeFixture(t *testing.T, service *Service, route Route) Route {
	t.Helper()
	route.Enabled = true
	configured, err := service.Configure(t.Context(), ConfigureRoute{Route: route, Actor: "fixture-owner", KeyRef: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	return configured
}

func appendEvent(t *testing.T, service *Service, id string, kind domain.EventKind, now time.Time) {
	t.Helper()
	event := domain.Event{ID: domain.EventID(id), Kind: kind, Actor: "fixture-actor", Reason: "private_not_payload",
		OccurredAt: now, IncidentID: "pseudonym-incident", CaseID: "path/private", IntentID: domain.IntentID(id)}
	if err := service.db.Write(t.Context(), func(tx *sql.Tx) error { return service.Append(t.Context(), tx, event) }); err != nil {
		t.Fatal(err)
	}
}

func fakeProwl(t *testing.T, run func(url.Values) (int, string, http.Header, error)) *prowl.Client {
	t.Helper()
	return prowl.NewClient(&http.Client{Transport: fakeTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://api.prowlapp.com/publicapi/add" || req.Method != http.MethodPost || req.URL.RawQuery != "" {
			t.Fatal("invalid provider request")
		}
		content, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		fields, err := url.ParseQuery(string(content))
		if err != nil || fields.Get("application") != "Rescuarr" || fields.Get("priority") != "0" || fields.Get("apikey") != testKey ||
			strings.Contains(string(content), "private") || fields.Get("url") != "" {
			t.Fatal("private or unexpected provider data")
		}
		status, body, headers, err := run(fields)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
}

func TestOutboxSuppressesDuplicatesAndPreservesAuditAtomically(t *testing.T) {
	db, _, clock := outboxFixture(t)
	s := New(db, clock, nil, false)
	route := routeFixture(t, s, Route{})
	appendEvent(t, s, "event-1", "incident_opened", clock.Now())
	appendEvent(t, s, "event-2", "incident_opened", clock.Now())
	appendEvent(t, s, "event-3", "incident_resolved", clock.Now())
	appendEvent(t, s, "event-4", "recovery_verified", clock.Now())
	statuses, err := s.Recent(t.Context(), route.ID, 10)
	if err != nil || len(statuses) != 2 || statuses[0].Category != "resolution" || statuses[1].Category != "incident" {
		t.Fatalf("unexpected grouping: %+v %v", statuses, err)
	}
	cancelled := errors.New("fixture abort")
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		if err := s.Append(t.Context(), tx, domain.Event{ID: "event-5", Kind: "action_required", CaseID: "case-5", OccurredAt: clock.Now()}); err != nil {
			return err
		}
		return cancelled
	}); !errors.Is(err, cancelled) {
		t.Fatal(err)
	}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var audit, pending int
		err := tx.QueryRow(`SELECT (SELECT count(*) FROM audit_events), (SELECT count(*) FROM notification_outbox)`).Scan(&audit, &pending)
		if audit != 5 || pending != 2 {
			return errors.New("mutation or audit escaped rollback")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RunOne(t.Context()); got || !errors.Is(err, ErrDisabled) {
		t.Fatalf("configured route granted live launch authority: %t %v", got, err)
	}
}

func TestRouteRequiresOneKeyAtConfigurationAndDelivery(t *testing.T) {
	db, dir, clock := outboxFixture(t)
	client := fakeProwl(t, func(url.Values) (int, string, http.Header, error) {
		t.Fatal("rotated multiple-key secret reached the provider")
		return 0, "", nil, nil
	})
	s := New(db, clock, client, true)
	secret := filepath.Join(dir, "secrets", "fixture")
	multiKey := []byte(testKey + "," + strings.Repeat("a", 40) + "\n")
	if err := os.WriteFile(secret, multiKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Configure(t.Context(), ConfigureRoute{Route: Route{Enabled: true}, KeyRef: "fixture", Actor: "owner"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("multiple-key route configuration returned %v", err)
	}
	if err := os.WriteFile(secret, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	route := routeFixture(t, s, Route{})
	if _, err := s.QueueTest(t.Context(), route.ID, route.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, multiKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if handled, err := s.RunOne(t.Context()); !handled || err != nil {
		t.Fatalf("invalid secret was not handled: %t, %v", handled, err)
	}
	statuses, err := s.Recent(t.Context(), route.ID, 10)
	if err != nil || len(statuses) != 1 || statuses[0].State != domain.OutboxFailed || statuses[0].Reason != "notification_invalid_request" {
		t.Fatalf("invalid rotated secret was not recorded: %+v, %v", statuses, err)
	}
}

func TestAttentionQueuesLaterTransitionForSameCase(t *testing.T) {
	db, _, clock := outboxFixture(t)
	client := fakeProwl(t, func(form url.Values) (int, string, http.Header, error) {
		if form.Get("event") != "Recovery needs attention" {
			t.Fatal("unexpected attention notification")
		}
		return 200, `<prowl><success code="200" remaining="25"/></prowl>`, nil, nil
	})
	s := New(db, clock, client, true)
	route := routeFixture(t, s, Route{Categories: Attention})
	appendEvent(t, s, "requires-attention", "action_required", clock.Now())
	if sent, err := s.RunOne(t.Context()); !sent || err != nil {
		t.Fatalf("first attention transition was not sent: %t, %v", sent, err)
	}
	clock.at = clock.at.Add(minSendGap)
	appendEvent(t, s, "unresolved-attempt", "action_unresolved", clock.Now())
	statuses, err := s.Recent(t.Context(), route.ID, 10)
	if err != nil || len(statuses) != 2 || statuses[0].State != domain.OutboxPending || statuses[1].State != domain.OutboxAccepted {
		t.Fatalf("later attention transition was deduplicated: %+v, %v", statuses, err)
	}
	if sent, err := s.RunOne(t.Context()); !sent || err != nil {
		t.Fatalf("later attention transition was not sent: %t, %v", sent, err)
	}
}

func TestPendingCapRecordsCoalescedSuppression(t *testing.T) {
	db, _, clock := outboxFixture(t)
	s := New(db, clock, nil, false)
	route := routeFixture(t, s, Route{Categories: Attention})
	for i := range maxPending + 2 {
		appendEvent(t, s, fmt.Sprintf("attention-%d", i), "action_required", clock.Now())
	}
	if delivery, err := s.QueueTest(t.Context(), route.ID, route.Revision, "owner"); delivery != "" || !errors.Is(err, ErrLimit) {
		t.Fatalf("test reported success at capacity: %q, %v", delivery, err)
	}
	first, err := s.Recent(t.Context(), route.ID, 1)
	if err != nil || len(first) != 1 || first[0].Category != "queue_overflow" || first[0].State != domain.OutboxSuppressed ||
		first[0].Count != 2 || first[0].Attempts != 0 || first[0].Reason != "notification_queue_full" {
		t.Fatalf("missing route overflow diagnostic: %+v, %v", first, err)
	}
	clock.at = clock.at.Add(time.Hour)
	appendEvent(t, s, "next-hour", "action_unresolved", clock.Now())
	latest, err := s.Recent(t.Context(), route.ID, 2)
	if err != nil || len(latest) != 2 || latest[0].Count != 1 || latest[0].Reason != "notification_queue_full" || latest[1] != first[0] {
		t.Fatalf("next hour did not create a new overflow record: %+v, %v", latest, err)
	}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var pending, suppressed, audit int
		err := tx.QueryRow(`SELECT
			(SELECT count(*) FROM notification_outbox WHERE state = 'pending'),
			(SELECT count(*) FROM notification_outbox WHERE state = 'suppressed'),
			(SELECT count(*) FROM audit_events)`).Scan(&pending, &suppressed, &audit)
		if err == nil && (pending != maxPending || suppressed != 2 || audit != maxPending+4) {
			return fmt.Errorf("unexpected pending, suppressed and audit counts: %d, %d, %d", pending, suppressed, audit)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOutboxProviderStatusAndRetries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
		state  domain.OutboxState
		reason domain.ReasonCode
		paused bool
	}{
		{"accepted", 200, `<prowl><success code="200" remaining="20" resetdate="1893456000"/></prowl>`, nil, domain.OutboxAccepted, "notification_provider_accepted", false},
		{"invalid key", 401, `<prowl><error code="401">arbitrary provider text</error></prowl>`, nil, domain.OutboxFailed, "notification_key_invalid", true},
		{"rate limited", 429, ``, nil, domain.OutboxDelayed, "notification_quota", false},
		{"transient", 500, `<prowl><error code="500">arbitrary provider text</error></prowl>`, nil, domain.OutboxDelayed, "notification_transient", false},
		{"conflicting failure", 503, `<prowl><error code="500">arbitrary provider text</error></prowl>`, nil, domain.OutboxUncertain, "notification_acknowledgement_unknown", false},
		{"malformed", 200, `<not-prowl private="data"/>`, nil, domain.OutboxUncertain, "notification_acknowledgement_unknown", false},
		{"lost response", 0, ``, io.ErrUnexpectedEOF, domain.OutboxUncertain, "notification_acknowledgement_unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _, clock := outboxFixture(t)
			var calls int
			client := fakeProwl(t, func(form url.Values) (int, string, http.Header, error) {
				calls++
				if form.Get("event") != "Test notification" {
					t.Fatal("non-generic test delivery")
				}
				return tc.status, tc.body, http.Header{"Retry-After": {"90"}}, tc.err
			})
			s := New(db, clock, client, true)
			route := routeFixture(t, s, Route{})
			if _, err := s.QueueTest(t.Context(), route.ID, route.Revision, "owner"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.QueueTest(t.Context(), route.ID, route.Revision, "owner"); !errors.Is(err, ErrLimit) {
				t.Fatalf("test rate limit: %v", err)
			}
			if sent, err := s.RunOne(t.Context()); !sent || err != nil || calls != 1 {
				t.Fatalf("expected one claim: %t %d %v", sent, calls, err)
			}
			statuses, err := s.Recent(t.Context(), route.ID, 1)
			if err != nil || len(statuses) != 1 || statuses[0].State != tc.state || statuses[0].Reason != tc.reason || statuses[0].Attempts != 1 {
				t.Fatalf("outcome mismatch: %+v %v", statuses, err)
			}
			routes, err := s.Routes(t.Context())
			if err != nil || len(routes) != 1 || routes[0].Paused != tc.paused {
				t.Fatalf("invalid key pause mismatch: %+v %v", routes, err)
			}
			if sent, err := s.RunOne(t.Context()); sent || err != nil {
				t.Fatalf("provider called before due time: %t %v", sent, err)
			}
			clock.at = clock.at.Add(2 * time.Hour)
			if sent, err := s.RunOne(t.Context()); sent != (tc.state == domain.OutboxDelayed) || err != nil {
				t.Fatalf("wrong retry decision: %t %v", sent, err)
			}
		})
	}
}

func TestAbandonedClaimIsUncertainAcrossRestart(t *testing.T) {
	db, dir, clock := outboxFixture(t)
	client := fakeProwl(t, func(url.Values) (int, string, http.Header, error) {
		t.Fatal("abandoned claim was retransmitted")
		return 0, "", nil, nil
	})
	s := New(db, clock, client, true)
	route := routeFixture(t, s, Route{})
	queued, err := s.QueueTest(t.Context(), route.ID, route.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := s.claim(t.Context(), clock.Now())
	if err != nil || intent.ID != queued {
		t.Fatalf("claim didn't persist: %+v %v", intent, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	clock.at = clock.at.Add(claimLease + time.Second)
	s = New(opened, clock, client, true)
	if sent, err := s.RunOne(t.Context()); sent || err != nil {
		t.Fatalf("unexpected restart send: %t %v", sent, err)
	}
	rows, err := s.Recent(t.Context(), route.ID, 10)
	if err != nil || len(rows) != 1 || rows[0].Attempts != 1 || rows[0].State != domain.OutboxUncertain {
		t.Fatalf("unknown provider state not persisted: %+v %v", rows, err)
	}
	if err := s.complete(t.Context(), intent, clock.Now(), prowl.Result{Status: prowl.Accepted}, ""); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("late obsolete sending completion escaped fence: %v", err)
	}
}

func TestQuietDigestAndRouteRevisionPersist(t *testing.T) {
	db, _, clock := outboxFixture(t)
	calls := 0
	client := fakeProwl(t, func(form url.Values) (int, string, http.Header, error) {
		calls++
		if form.Get("event") != "Recovery digest" || form.Get("description") != "Verified recovery events: 3." {
			t.Fatalf("unexpected coalesced digest %q", form.Get("description"))
		}
		return 200, `<prowl><success code="200" remaining="99" resetdate="1893456000"/></prowl>`, nil, nil
	})
	s := New(db, clock, client, true)
	route := routeFixture(t, s, Route{QuietStart: 720, QuietEnd: 840, Digest: true, Categories: Recovery})
	for n := range 3 {
		appendEvent(t, s, fmt.Sprint(n), "recovery_verified", clock.Now())
	}
	rows, err := s.Recent(t.Context(), route.ID, 10)
	if err != nil || len(rows) != 1 || rows[0].Count != 3 || rows[0].Scheduled.Hour() != 14 {
		t.Fatalf("digest missed quiet hours: %+v %v", rows, err)
	}
	if sent, err := s.RunOne(t.Context()); sent || err != nil {
		t.Fatalf("sent during quiet hours: %t %v", sent, err)
	}
	clock.at = rows[0].Scheduled
	if sent, err := s.RunOne(t.Context()); !sent || err != nil || calls != 1 {
		t.Fatalf("digest not sent: %t %v", sent, err)
	}
	if _, err := s.QueueTest(t.Context(), route.ID, route.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	updated := routeFixture(t, s, route)
	if updated.Revision != route.Revision+1 {
		t.Fatal("route revision not incremented")
	}
	rows, err = s.Recent(t.Context(), route.ID, 10)
	if err != nil || len(rows) != 2 || rows[0].State != domain.OutboxSuppressed || rows[1].State != domain.OutboxAccepted {
		t.Fatalf("route change overwrote accepted or kept pending notification: %+v %v", rows, err)
	}
}

func TestQuotaGatesSameRouteAndAcceptedDoesNotProveHandsetDelivery(t *testing.T) {
	db, _, clock := outboxFixture(t)
	var calls int
	client := fakeProwl(t, func(url.Values) (int, string, http.Header, error) {
		calls++
		return 200, fmt.Sprintf(`<prowl><success code="200" remaining="0" resetdate="%d"/></prowl>`, clock.Now().Add(time.Minute).Unix()), nil, nil
	})
	s := New(db, clock, client, true)
	route := routeFixture(t, s, Route{})
	appendEvent(t, s, "first", "incident_opened", clock.Now())
	appendEvent(t, s, "second", "incident_resolved", clock.Now())
	if sent, err := s.RunOne(t.Context()); !sent || err != nil {
		t.Fatalf("first send failure %t %v", sent, err)
	}
	clock.at = clock.at.Add(minSendGap + time.Second)
	if sent, err := s.RunOne(t.Context()); sent || err != nil || calls != 1 {
		t.Fatalf("zero quota bypassed: %t %v %d", sent, err, calls)
	}
	rows, err := s.Recent(t.Context(), route.ID, 10)
	if err != nil || len(rows) != 2 || !((rows[0].Reason == "notification_provider_accepted") != (rows[1].Reason == "notification_provider_accepted")) {
		t.Fatalf("provider acceptance evidence not surfaced: %+v %v", rows, err)
	}
}

func TestAttemptSchemaPersistsNoSecretPayloads(t *testing.T) {
	db, _, clock := outboxFixture(t)
	s := New(db, clock, fakeProwl(t, func(url.Values) (int, string, http.Header, error) {
		return 200, `<prowl><success code="200" remaining="25" resetdate="1893456000"/></prowl>`, nil, nil
	}), true)
	route := routeFixture(t, s, Route{})
	if _, err := s.QueueTest(t.Context(), route.ID, route.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var state, reason, outboxID string
		if err := tx.QueryRow(`SELECT state, reason, outbox_id FROM prowl_attempts`).Scan(&state, &reason, &outboxID); err != nil {
			return err
		}
		if state != "accepted" || reason != "notification_provider_accepted" || strings.Contains(outboxID, testKey) {
			return errors.New("unexpected attempt fields")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
