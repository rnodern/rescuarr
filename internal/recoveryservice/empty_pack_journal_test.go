package recoveryservice

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/ledger"
	"github.com/rnodern/rescuarr/internal/store"
)

func TestEmptyPackJournalRestartsWithoutRepeatingSends(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "state")
	local := openEmptyPackFixture(t, dir)
	key, plan := emptyPackFixture()
	registerEmptyPackInstance(t, local, key.Job.Instance)
	journal := NewEmptyPackJournal(local)
	now := time.Date(2026, 11, 14, 11, 0, 0, 0, time.UTC)

	var workers sync.WaitGroup
	results := make(chan bool, 4)
	errs := make(chan error, 4)
	for range cap(results) {
		workers.Go(func() {
			sent, err := journal.BeginReject(ctx, key, plan, now)
			results <- sent
			errs <- err
		})
	}
	workers.Wait()
	close(results)
	close(errs)
	sends := 0
	for send := range results {
		if send {
			sends++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if sends != 1 {
		t.Fatalf("%d workers were allowed to send the same rejection", sends)
	}
	assertEmptyPackAudit(t, local, "recovery_empty_pack_reject_sending")
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}

	local = openEmptyPackFixture(t, dir)
	journal = NewEmptyPackJournal(local)
	pending, err := journal.Pending(ctx, 2)
	if err != nil || len(pending) != 1 {
		t.Fatalf("cannot reconcile after restart: %v %v", pending, err)
	}
	first := pending[0]
	if first.Stage != EmptyPackRejectSending || first.Key != key || first.StartedAt != now ||
		first.DownloadID != plan.DownloadID || !reflect.DeepEqual(first.QueueIDs, plan.QueueIDs) ||
		!reflect.DeepEqual(first.EpisodeIDs, plan.EpisodeIDs) {
		t.Fatalf("restart lost the original download identity: %+v", first)
	}
	if sent, err := journal.BeginReject(ctx, key, plan, now.Add(time.Minute)); err != nil || sent {
		t.Fatalf("restart admitted the same rejection again: %v, %v", sent, err)
	}
	now = now.Add(time.Minute)
	expectEmptyPackAdvance(t, journal, key, EmptyPackRejectSending, EmptyPackCheckpoint{Stage: EmptyPackRejectVerified}, now)

	for _, ids := range [][]int64{nil, {1, 1}, {2}} {
		next := EmptyPackCheckpoint{Stage: EmptyPackSearchSending, SearchIDs: ids}
		if wrote, err := journal.Advance(ctx, key, EmptyPackRejectVerified, next, now); wrote || !errors.Is(err, ledger.ErrInvalid) {
			t.Fatalf("invalid search scope accepted: %v %v %v", ids, wrote, err)
		}
	}
	search := EmptyPackCheckpoint{Stage: EmptyPackSearchSending, SearchIDs: []int64{26}}
	expectEmptyPackAdvance(t, journal, key, EmptyPackRejectVerified, search, now)
	if pending, err := journal.Pending(ctx, 2); err != nil || len(pending) != 0 {
		t.Fatalf("an unacknowledged search must not be retried: %v %v", pending, err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}

	local = openEmptyPackFixture(t, dir)
	defer local.Close()
	journal = NewEmptyPackJournal(local)
	if sent, err := journal.Advance(ctx, key, EmptyPackRejectVerified, search, now); err != nil || sent {
		t.Fatalf("restart admitted the same search again: %v %v", sent, err)
	}
	uncertain, found, err := journal.Load(ctx, key)
	if err != nil || !found || uncertain.Stage != EmptyPackSearchSending || !reflect.DeepEqual(uncertain.SearchIDs, []int64{26}) {
		t.Fatalf("restart lost the uncertain search scope: %+v %v %v", uncertain, found, err)
	}
	if wrote, err := journal.Advance(ctx, key, EmptyPackSearchSending, EmptyPackCheckpoint{Stage: EmptyPackDone}, now); wrote || !errors.Is(err, ledger.ErrInvalid) {
		t.Fatalf("an unacknowledged search was completed: %v %v", wrote, err)
	}
	expectEmptyPackAdvance(t, journal, key, EmptyPackSearchSending, EmptyPackCheckpoint{Stage: EmptyPackDone, CommandID: 88}, now)
	done, found, err := journal.Load(ctx, key)
	if err != nil || !found || done.Stage != EmptyPackDone || done.CommandID != 88 ||
		!reflect.DeepEqual(done.SearchIDs, []int64{26}) {
		t.Fatalf("terminal checkpoint changed the original search scope: %+v %v %v", done, found, err)
	}
	assertEmptyPackAudit(t, local, "recovery_empty_pack_reject_sending", "recovery_empty_pack_reject_verified",
		"recovery_empty_pack_search_sending", "recovery_empty_pack_done")
}

func TestEmptyPackJournalHonoursPauseAndConnectionFence(t *testing.T) {
	ctx := t.Context()
	local := openEmptyPackFixture(t, filepath.Join(t.TempDir(), "state"))
	defer local.Close()
	key, plan := emptyPackFixture()
	registerEmptyPackInstance(t, local, key.Job.Instance)
	journal := NewEmptyPackJournal(local)
	now := time.Now()

	for _, gate := range []struct{ hold, clear string }{
		{"UPDATE app_state SET mode = 'observe'", "UPDATE app_state SET mode = 'autonomous'"},
		{"UPDATE app_state SET reconcile_required = 1", "UPDATE app_state SET reconcile_required = 0"},
		{"UPDATE action_pause SET paused = 1", "UPDATE action_pause SET paused = 0"},
		{"UPDATE observe_instances SET connection_revision = 2", "UPDATE observe_instances SET connection_revision = 1"},
		{"UPDATE observe_instances SET health = 'removed'", "UPDATE observe_instances SET health = 'available'"},
	} {
		writeEmptyPackFixture(t, local, gate.hold)
		if sent, err := journal.BeginReject(ctx, key, plan, now); sent || !errors.Is(err, app.ErrHeld) {
			t.Fatalf("closed gate admitted a rejection: %v %v", sent, err)
		}
		writeEmptyPackFixture(t, local, gate.clear)
	}
	if sent, err := journal.BeginReject(ctx, key, plan, now); !sent || err != nil {
		t.Fatalf("ready installation did not authorise the send: %v %v", sent, err)
	}
	other := key
	other.CaseID, other.Job.ID = "other-case", "other-job"
	if sent, err := journal.BeginReject(ctx, other, plan, now); sent || !errors.Is(err, app.ErrHeld) {
		t.Fatalf("second pack bypassed the unresolved first pack: %v %v", sent, err)
	}
	collidingCase := key
	collidingCase.CaseID = other.CaseID
	if sent, err := journal.BeginReject(ctx, collidingCase, plan, now); sent || !errors.Is(err, app.ErrConflict) {
		t.Fatalf("a different case bypassed the durable job fence: %v %v", sent, err)
	}
	expectEmptyPackAdvance(t, journal, key, EmptyPackRejectSending, EmptyPackCheckpoint{Stage: EmptyPackRejectVerified}, now)
	writeEmptyPackFixture(t, local, "UPDATE observe_instances SET health = 'removed'")
	search := EmptyPackCheckpoint{Stage: EmptyPackSearchSending, SearchIDs: []int64{26}}
	if sent, err := journal.Advance(ctx, key, EmptyPackRejectVerified, search, now); sent || !errors.Is(err, app.ErrHeld) {
		t.Fatalf("removed connection admitted a search: %v %v", sent, err)
	}
	if pending, err := journal.Pending(ctx, 1); err != nil || len(pending) != 0 {
		t.Fatalf("retired connection kept occupying pending reads: %v %v", pending, err)
	}
	writeEmptyPackFixture(t, local, "UPDATE observe_instances SET health = 'available'")
	expectEmptyPackAdvance(t, journal, key, EmptyPackRejectVerified, EmptyPackCheckpoint{Stage: EmptyPackDone}, now)
	done, found, err := journal.Load(ctx, key)
	if err != nil || !found || done.Stage != EmptyPackDone || done.CommandID != 0 || len(done.SearchIDs) != 0 {
		t.Fatalf("no wanted targets should close without a search reservation: %+v %v %v", done, found, err)
	}
	if sent, err := journal.BeginReject(ctx, other, plan, now); !sent || err != nil {
		t.Fatalf("finished pack still blocking unrelated jobs: %v %v", sent, err)
	}
}

func TestEmptyPackJournalCancelsOnlyProvenUnsentAttempts(t *testing.T) {
	ctx := t.Context()
	local := openEmptyPackFixture(t, filepath.Join(t.TempDir(), "state"))
	defer local.Close()
	key, plan := emptyPackFixture()
	registerEmptyPackInstance(t, local, key.Job.Instance)
	journal := NewEmptyPackJournal(local)
	now := time.Now()
	if sent, err := journal.BeginReject(ctx, key, plan, now); !sent || err != nil {
		t.Fatalf("could not reserve reject: %v %v", sent, err)
	}
	if cleared, err := journal.CancelUnsent(ctx, key, EmptyPackSearchSending, now); cleared || err != nil {
		t.Fatalf("wrong reservation was released: %v %v", cleared, err)
	}
	if cleared, err := journal.CancelUnsent(ctx, key, EmptyPackRejectSending, now.Add(-time.Second)); cleared || err != nil {
		t.Fatalf("stale release changed a newer reservation: %v %v", cleared, err)
	}
	if cleared, err := journal.CancelUnsent(ctx, key, EmptyPackRejectSending, now); !cleared || err != nil {
		t.Fatalf("known-unsent reject remains fenced: %v %v", cleared, err)
	}
	if _, found, err := journal.Load(ctx, key); found || err != nil {
		t.Fatalf("released reject remains stored: %v %v", found, err)
	}
	if sent, err := journal.BeginReject(ctx, key, plan, now); !sent || err != nil {
		t.Fatalf("could not re-evaluate an unsent rejection: %v %v", sent, err)
	}
	expectEmptyPackAdvance(t, journal, key, EmptyPackRejectSending, EmptyPackCheckpoint{Stage: EmptyPackRejectVerified}, now)
	expectEmptyPackAdvance(t, journal, key, EmptyPackRejectVerified,
		EmptyPackCheckpoint{Stage: EmptyPackSearchSending, SearchIDs: []int64{26}}, now)
	if cleared, err := journal.CancelUnsent(ctx, key, EmptyPackSearchSending, now); !cleared || err != nil {
		t.Fatalf("known-unsent search remains fenced: %v %v", cleared, err)
	}
	verified, found, err := journal.Load(ctx, key)
	if !found || err != nil || verified.Stage != EmptyPackRejectVerified || len(verified.SearchIDs) != 0 ||
		verified.DownloadID != plan.DownloadID || !reflect.DeepEqual(verified.EpisodeIDs, plan.EpisodeIDs) {
		t.Fatalf("search cancellation erased verified rejection evidence: %+v %v %v", verified, found, err)
	}
	assertEmptyPackAudit(t, local, "recovery_empty_pack_reject_sending", "recovery_empty_pack_send_cancelled",
		"recovery_empty_pack_reject_sending", "recovery_empty_pack_reject_verified",
		"recovery_empty_pack_search_sending", "recovery_empty_pack_send_cancelled")
}

func emptyPackFixture() (EmptyPackKey, EmptyPackCheckpoint) {
	instance := domain.InstanceRef{ID: "fixture-sonarr", Epoch: 1}
	key := EmptyPackKey{CaseID: "fixture-case", Job: domain.JobRef{Instance: instance, ID: "fixture-job", Generation: 1}}
	return key, EmptyPackCheckpoint{
		ConnectionRevision: 1, Release: "Fixture.Series.S01.720p", DownloadID: "fixture-download",
		QueueIDs: []int64{101, 126}, SeriesID: 8, EpisodeIDs: []int64{1, 26},
	}
}

func openEmptyPackFixture(t *testing.T, dir string) *store.Store {
	t.Helper()
	local, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return local
}

func registerEmptyPackInstance(t *testing.T, local *store.Store, instance domain.InstanceRef) {
	t.Helper()
	writeEmptyPackFixture(t, local, `INSERT INTO observe_instances
		(id, epoch, product, display_name, base_url, secret_ref, connection_revision, health)
		VALUES (?, ?, 'sonarr', 'Fixture', 'http://fixture.invalid', 'fixture.key', 1, 'available')`, instance.ID, instance.Epoch)
	writeEmptyPackFixture(t, local, "UPDATE app_state SET mode = 'autonomous'")
}

func writeEmptyPackFixture(t *testing.T, local *store.Store, query string, args ...any) {
	t.Helper()
	ctx := t.Context()
	err := local.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func expectEmptyPackAdvance(t *testing.T, journal *EmptyPackJournal, key EmptyPackKey, expected EmptyPackStage, next EmptyPackCheckpoint, now time.Time) {
	t.Helper()
	wrote, err := journal.Advance(t.Context(), key, expected, next, now)
	if !wrote || err != nil {
		t.Fatalf("expected %s to advance to %s: %v %v", expected, next.Stage, wrote, err)
	}
}

func assertEmptyPackAudit(t *testing.T, local *store.Store, expected ...string) {
	t.Helper()
	var found []string
	ctx := t.Context()
	err := local.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT kind FROM audit_events ORDER BY sequence")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind string
			if err := rows.Scan(&kind); err != nil {
				return err
			}
			found = append(found, kind)
		}
		return rows.Err()
	})
	if err != nil || !reflect.DeepEqual(found, expected) {
		t.Fatalf("unexpected audit events: %v %v", found, err)
	}
}
