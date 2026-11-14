package recoveryservice

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/store"
)

type emptyPackEffects interface {
	Preflight(context.Context, arradapter.EmptyPackScope) error
	Reject(context.Context, arradapter.EmptyPackScope) error
	VerifyRejected(context.Context, arradapter.EmptyPackScope, time.Time) (bool, error)
	Search(context.Context, arradapter.EmptyPackScope, []int64) (int64, error)
}

type emptyPackReader interface {
	SonarrEpisodeNeeds(context.Context, int64, []int64, time.Time) (map[int64]observe.EpisodeNeed, error)
	Queue(context.Context, domain.Revision, time.Time) (observe.QueueSnapshot, error)
}

// emptyPackWorker processes one completed no-files download at a time. The
// connection factory supplies both qualified effects and independent reads.
type emptyPackWorker struct {
	cases      *observeservice.Service
	journal    *EmptyPackJournal
	clock      app.Clock
	connection func(domain.Instance) (emptyPackEffects, emptyPackReader, error)
	after      domain.CaseID
	instance   domain.InstanceRef
}

// RunEmptyPackRecovery runs the background worker on an explicitly qualified
// connection provider. It does not start when the caller has no verified route.
func RunEmptyPackRecovery(ctx context.Context, cases *observeservice.Service, local *store.Store, clock app.Clock, instance domain.InstanceRef,
	transport http.RoundTripper, qualified func(domain.Instance) (*arradapter.EmptyPackAdapter, error), onError func(error)) error {
	if cases == nil || local == nil || clock == nil || !instance.Valid() || transport == nil || qualified == nil {
		return arradapter.ErrQualification
	}
	worker := newEmptyPackWorker(ctx, cases, local, clock, transport, func(_ context.Context, instance domain.Instance) (*arradapter.EmptyPackAdapter, error) {
		return qualified(instance)
	})
	worker.instance = instance
	return runEmptyPackPasses(ctx, worker.step, onError)
}

// RunManualEmptyPackRecovery resumes only the pack explicitly approved by the
// owner. Automatic selection remains exclusive to the autonomous worker.
func RunManualEmptyPackRecovery(ctx context.Context, cases *observeservice.Service, local *store.Store, clock app.Clock,
	transport http.RoundTripper, qualified func(context.Context, domain.Instance) (*arradapter.EmptyPackAdapter, error), onError func(error)) error {
	if cases == nil || local == nil || clock == nil || transport == nil || qualified == nil {
		return arradapter.ErrQualification
	}
	return runEmptyPackPasses(ctx, func(pass context.Context) error {
		worker := newEmptyPackWorker(pass, cases, local, clock, transport, qualified)
		return worker.manualStep(pass, transport)
	}, onError)
}

func newEmptyPackWorker(ctx context.Context, cases *observeservice.Service, local *store.Store, clock app.Clock,
	transport http.RoundTripper, qualified func(context.Context, domain.Instance) (*arradapter.EmptyPackAdapter, error)) *emptyPackWorker {
	return &emptyPackWorker{
		cases: cases, journal: NewEmptyPackJournal(local), clock: clock,
		connection: func(instance domain.Instance) (emptyPackEffects, emptyPackReader, error) {
			effects, err := qualified(ctx, instance)
			if err != nil {
				return nil, nil, err
			}
			if effects == nil || !effects.MatchesInstance(instance) {
				return nil, nil, arradapter.ErrQualification
			}
			key, err := local.ReadSecret(instance.SecretRef)
			if err != nil {
				return nil, nil, observe.ErrAccess
			}
			reader, err := observe.NewArr(observe.Config{Instance: instance, APIKey: key}, transport)
			return effects, reader, err
		},
	}
}

func runEmptyPackPasses(ctx context.Context, step func(context.Context) error, onError func(error)) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			pass, cancel := context.WithTimeout(ctx, 50*time.Second)
			err := step(pass)
			cancel()
			if err != nil && ctx.Err() == nil && onError != nil {
				onError(err)
			}
			timer.Reset(5 * time.Second)
		}
	}
}

func (w *emptyPackWorker) enabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := w.journal.local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT mode = 'autonomous' AND NOT reconcile_required AND NOT paused
			FROM app_state JOIN action_pause USING (singleton) WHERE singleton = 1`).Scan(&enabled)
	})
	return enabled, err
}

func (w *emptyPackWorker) step(ctx context.Context) error {
	if allowed, err := w.enabled(ctx); !allowed || err != nil {
		return err
	}
	pending, err := w.journal.Pending(ctx, 128)
	if err != nil {
		return err
	}
	var pendingErrors error
	for _, checkpoint := range pending {
		if checkpoint.manualToken != "" || w.instance.Valid() && w.instance != checkpoint.Key.Job.Instance {
			continue
		}
		if ctx.Err() != nil {
			return errors.Join(pendingErrors, ctx.Err())
		}
		pendingErrors = errors.Join(pendingErrors, w.resume(ctx, checkpoint))
	}
	cases, err := w.cases.AutomaticEmptyPackCasesFor(ctx, w.instance.ID, w.after, 1)
	if err != nil {
		return errors.Join(pendingErrors, err)
	}
	if len(cases) == 0 {
		w.after = ""
		return pendingErrors
	}
	item := cases[0]
	w.after = item.ID
	return errors.Join(pendingErrors, w.start(ctx, item))
}

type manualPackPlan struct {
	checkpoint EmptyPackCheckpoint
	preview    *app.ReplacementPreview
	searchIDs  []int64
	scopeHash  string
	folder     string
}

func prepareManualEmptyPack(ctx context.Context, cases *observeservice.Service, local *store.Store, clock app.Clock,
	transport http.RoundTripper, id domain.CaseID, revision domain.Revision) (manualPackPlan, error) {
	proof, err := cases.FreshRecoveryQueue(ctx, id, revision)
	if err != nil {
		return manualPackPlan{}, err
	}
	seriesID, ids, exact := missingFileTargets(proof)
	key := EmptyPackKey{CaseID: id, Job: proof.Case.Job.Ref}
	if !exact || !key.valid() || len(ids) < 2 || proof.Case.Job.Protocol != domain.ProtocolUsenet ||
		proof.Case.Job.Shared || proof.Case.LastSeenAt.IsZero() {
		return manualPackPlan{}, app.ErrHeld
	}
	release, err := cases.HoldRecoveryConnection(ctx, proof.Instance)
	if err != nil {
		return manualPackPlan{}, err
	}
	defer release()
	apiKey, err := local.ReadSecret(proof.Instance.SecretRef)
	if err != nil {
		return manualPackPlan{}, observe.ErrAccess
	}
	arr, err := observe.NewArr(observe.Config{Instance: proof.Instance, APIKey: apiKey}, transport)
	if err != nil {
		return manualPackPlan{}, err
	}
	needs, err := arr.SonarrEpisodeNeeds(ctx, seriesID, ids, clock.Now())
	if err != nil {
		return manualPackPlan{}, err
	}
	// Read the full queue again after the episode lookup. A second active job
	// may have appeared while the library/monitoring evidence was fetched.
	current, err := cases.FreshRecoveryQueue(ctx, id, revision)
	if err != nil {
		return manualPackPlan{}, err
	}
	currentSeries, currentIDs, ok := missingFileTargets(current)
	if !ok || current.Instance != proof.Instance || current.Case.Job.Ref != key.Job ||
		currentSeries != seriesID || !slices.Equal(currentIDs, ids) {
		return manualPackPlan{}, app.ErrConflict
	}
	first := current.Members[0]
	if first.OutputPath == "" || first.DownloadID == "" || first.ClientLabel == "" {
		return manualPackPlan{}, app.ErrHeld
	}
	original := proof.Members[0]
	if first.OutputPath != original.OutputPath || first.DownloadID != original.DownloadID ||
		first.ClientLabel != original.ClientLabel || first.Release != original.Release {
		return manualPackPlan{}, app.ErrConflict
	}
	checkpoint := EmptyPackCheckpoint{
		Key: key, ConnectionRevision: current.Instance.ConnectionRevision, SeriesID: seriesID,
		Release: first.Release, DownloadID: first.DownloadID, EpisodeIDs: ids,
		outputPath: first.OutputPath, clientLabel: first.ClientLabel,
	}
	for _, row := range current.Members {
		if row.OutputPath != first.OutputPath || row.DownloadID != first.DownloadID || row.ClientLabel != first.ClientLabel {
			return manualPackPlan{}, app.ErrHeld
		}
		checkpoint.QueueIDs = append(checkpoint.QueueIDs, row.ID)
	}
	slices.Sort(checkpoint.QueueIDs)
	slices.Sort(checkpoint.EpisodeIDs)
	if !validEmptyPackIdentity(checkpoint) {
		return manualPackPlan{}, app.ErrHeld
	}
	folder := strings.TrimRight(first.OutputPath, "/\\")
	folder = folder[strings.LastIndexAny(folder, "/\\")+1:]
	if !emptyPackToken(folder, 512) {
		folder = ""
	}
	plan := manualPackPlan{checkpoint: checkpoint, preview: replacementPlan(current, needs), folder: folder}
	prefs, err := app.NewLanguageSettings(local, clock).LanguageSettings(ctx)
	if err != nil {
		return manualPackPlan{}, err
	}
	plan.checkpoint.languageRevision = prefs.Revision
	plan.preview.Language = reviewPackLanguage(ctx, arr, current, plan.preview, clock.Now(), prefs, domain.ModeAssist)
	plan.searchIDs = wantedEmptyPackEpisodes(plan.preview)
	// The token binds to what the owner sees and to the original private job
	// and path identity. Only the digest is persisted with the approval.
	content, err := json.Marshal(struct {
		Checkpoint EmptyPackCheckpoint
		Preview    *app.ReplacementPreview
		Path       string
		Client     string
		Language   domain.Revision
	}{checkpoint, plan.preview, first.OutputPath, first.ClientLabel, prefs.Revision})
	if err != nil {
		return manualPackPlan{}, err
	}
	digest := sha256.Sum256(content)
	plan.scopeHash = hex.EncodeToString(digest[:])
	return plan, nil
}

func (w *emptyPackWorker) manualStep(ctx context.Context, transport http.RoundTripper) error {
	local := w.journal.local
	var id domain.CaseID
	var revision domain.Revision
	var token, scopeHash string
	var searchJSON []byte
	err := local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT m.case_id, m.case_revision, m.token, m.scope_hash, m.search_ids
			FROM manual_reviews AS m
			JOIN app_state AS s ON s.singleton = 1 JOIN action_pause AS p ON p.singleton = 1
			WHERE m.state = 'approved' AND s.mode = 'assist' AND s.reconcile_required = 0 AND p.paused = 0`).
			Scan(&id, &revision, &token, &scopeHash, &searchJSON)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	checkpoint, exists, err := w.journal.loadManual(ctx, id, token)
	if err != nil {
		return err
	}
	if exists {
		if checkpoint.Stage == EmptyPackRejectSending || checkpoint.Stage == EmptyPackRejectVerified {
			return w.resume(ctx, checkpoint)
		}
		// A lost search acknowledgement is held for operator reconciliation.
		return nil
	}
	detail, err := w.cases.Case(ctx, id)
	if errors.Is(err, app.ErrNotFound) || detail.Case.State == domain.CaseClosed ||
		err == nil && (detail.Case.Revision != revision || len(detail.Case.Incidents) != 0) {
		return resetManualReview(ctx, local, id, token, w.clock.Now())
	}
	if err != nil {
		return err
	}
	plan, err := prepareManualEmptyPack(ctx, w.cases, local, w.clock, transport, id, revision)
	switch {
	case errors.Is(err, app.ErrConflict):
		return resetManualReview(ctx, local, id, token, w.clock.Now())
	case err != nil:
		return err
	}
	var approved []int64
	if err := json.Unmarshal(searchJSON, &approved); err != nil {
		return err
	}
	if plan.scopeHash != scopeHash || !slices.Equal(plan.searchIDs, approved) {
		return resetManualReview(ctx, local, id, token, w.clock.Now())
	}
	if plan.preview.UnclearCount != 0 || plan.preview.Language != nil && plan.preview.Language.NeedsReview {
		return app.ErrHeld
	}
	instance, err := w.cases.RecoveryInstance(ctx, plan.checkpoint.Key.Job.Instance, plan.checkpoint.ConnectionRevision)
	if err != nil {
		return err
	}
	release, err := w.cases.HoldRecoveryConnection(ctx, instance)
	if err != nil {
		return err
	}
	defer release()
	effects, _, err := w.connection(instance)
	if errors.Is(err, arradapter.ErrQualification) {
		return resetManualReview(ctx, local, id, token, w.clock.Now())
	}
	if err != nil {
		return err
	}
	plan.checkpoint.manualToken, plan.checkpoint.manualScopeHash = token, scopeHash
	err = w.reject(ctx, effects, plan.checkpoint)
	if errors.Is(err, app.ErrConflict) || errors.Is(err, arradapter.ErrReadback) || errors.Is(err, arradapter.ErrQualification) {
		return resetManualReview(ctx, local, id, token, w.clock.Now())
	}
	return err
}

func (w *emptyPackWorker) start(ctx context.Context, item domain.Case) error {
	key := EmptyPackKey{CaseID: item.ID, Job: item.Job.Ref}
	if _, exists, err := w.journal.Load(ctx, key); exists || err != nil {
		return err
	}
	proof, err := w.cases.FreshRecoveryQueue(ctx, item.ID, item.Revision)
	if errors.Is(err, app.ErrHeld) || errors.Is(err, app.ErrConflict) || errors.Is(err, observeservice.ErrLimit) {
		return nil
	}
	if err != nil {
		return err
	}
	seriesID, ids, exact := missingFileTargets(proof)
	if !exact || len(ids) < 2 || proof.Case.Job.Protocol != domain.ProtocolUsenet || proof.Case.Job.Shared || proof.Case.LastSeenAt.IsZero() {
		return nil
	}
	release, err := w.cases.HoldRecoveryConnection(ctx, proof.Instance)
	if err != nil {
		return err
	}
	defer release()
	first := proof.Members[0]
	checkpoint := EmptyPackCheckpoint{
		Key: key, ConnectionRevision: proof.Instance.ConnectionRevision, SeriesID: seriesID,
		Release: first.Release, DownloadID: first.DownloadID, EpisodeIDs: ids,
	}
	for _, row := range proof.Members {
		checkpoint.QueueIDs = append(checkpoint.QueueIDs, row.ID)
	}
	slices.Sort(checkpoint.QueueIDs)
	slices.Sort(checkpoint.EpisodeIDs)
	effects, reader, err := w.connection(proof.Instance)
	if err != nil {
		return err
	}
	now := w.clock.Now()
	needs, err := reader.SonarrEpisodeNeeds(ctx, seriesID, ids, now)
	if err != nil {
		return err
	}
	if replacementPlan(proof, needs).UnclearCount != 0 {
		return nil
	}
	return w.reject(ctx, effects, checkpoint)
}

func (w *emptyPackWorker) reject(ctx context.Context, effects emptyPackEffects, checkpoint EmptyPackCheckpoint) error {
	scope := emptyPackScope(checkpoint)
	if err := effects.Preflight(ctx, scope); err != nil {
		return err
	}
	key := checkpoint.Key
	ok, err := w.journal.BeginReject(ctx, key, checkpoint, w.clock.Now())
	if !ok || err != nil {
		return err
	}
	// Even an unknown response must be reconciled through independent reads.
	err = effects.Reject(ctx, scope)
	return w.cancelIfUnsent(ctx, key, EmptyPackRejectSending, err)
}

func (w *emptyPackWorker) resume(ctx context.Context, cp EmptyPackCheckpoint) error {
	instance, err := w.cases.RecoveryInstance(ctx, cp.Key.Job.Instance, cp.ConnectionRevision)
	if errors.Is(err, app.ErrHeld) {
		return nil
	}
	if err != nil {
		return err
	}
	release, err := w.cases.HoldRecoveryConnection(ctx, instance)
	if err != nil {
		return err
	}
	defer release()
	effects, reader, err := w.connection(instance)
	if err != nil {
		return err
	}
	scope := emptyPackScope(cp)
	verified, err := effects.VerifyRejected(ctx, scope, cp.StartedAt)
	if err != nil || !verified {
		return err
	}
	if cp.Stage == EmptyPackRejectSending {
		ok, err := w.journal.Advance(ctx, cp.Key, cp.Stage, EmptyPackCheckpoint{Stage: EmptyPackRejectVerified}, w.clock.Now())
		if err != nil || !ok {
			return err
		}
		cp.Stage = EmptyPackRejectVerified
	}
	return w.search(ctx, effects, reader, cp)
}

func (w *emptyPackWorker) search(ctx context.Context, effects emptyPackEffects, reader emptyPackReader, cp EmptyPackCheckpoint) error {
	needs, err := reader.SonarrEpisodeNeeds(ctx, cp.SeriesID, cp.EpisodeIDs, w.clock.Now())
	if err != nil {
		return err
	}
	now := w.clock.Now()
	queue, err := reader.Queue(ctx, 1, now)
	if err != nil || !queue.Evidence.Fresh(w.clock.Now()) {
		return err
	}
	for _, row := range queue.Records {
		if row.DownloadID == cp.DownloadID || slices.Contains(cp.QueueIDs, row.ID) {
			return nil
		}
	}
	item := domain.Case{}
	for _, id := range cp.EpisodeIDs {
		item.Targets = append(item.Targets, domain.Target{Ref: domain.TargetRef{
			Instance: cp.Key.Job.Instance, Kind: domain.TargetEpisode, ID: id, SeriesID: cp.SeriesID,
		}})
	}
	plan := planReplacement(item, queue, cp.DownloadID, cp.SeriesID, cp.Release, needs)
	if plan.UnclearCount != 0 {
		return nil
	}
	wanted := wantedEmptyPackEpisodes(plan)
	if cp.manualToken != "" {
		approved, err := w.journal.manualSearchScope(ctx, cp)
		if err != nil {
			return err
		}
		wanted = slices.DeleteFunc(wanted, func(id int64) bool { return !slices.Contains(approved, id) })
	}
	if len(wanted) == 0 {
		_, err := w.journal.Advance(ctx, cp.Key, cp.Stage, EmptyPackCheckpoint{Stage: EmptyPackDone}, w.clock.Now())
		return err
	}
	ok, err := w.journal.Advance(ctx, cp.Key, cp.Stage, EmptyPackCheckpoint{Stage: EmptyPackSearchSending, SearchIDs: wanted}, w.clock.Now())
	if err != nil || !ok {
		return err
	}
	commandID, err := effects.Search(ctx, emptyPackScope(cp), wanted)
	if err != nil {
		return w.cancelIfUnsent(ctx, cp.Key, EmptyPackSearchSending, err)
	}
	_, err = w.journal.Advance(ctx, cp.Key, EmptyPackSearchSending, EmptyPackCheckpoint{Stage: EmptyPackDone, CommandID: commandID}, w.clock.Now())
	return err
}

func wantedEmptyPackEpisodes(plan *app.ReplacementPreview) []int64 {
	wanted := make([]int64, 0, plan.SearchCount)
	for _, episode := range plan.Episodes {
		if episode.Status == app.ReplacementSearch {
			wanted = append(wanted, episode.Target.ID)
		}
	}
	slices.Sort(wanted)
	return wanted
}

// The adapter reserves ErrUncertain for anything that reached transport.
// Only a known-unsent preflight failure can release a checkpoint.
func (w *emptyPackWorker) cancelIfUnsent(ctx context.Context, key EmptyPackKey, stage EmptyPackStage, sendErr error) error {
	if !errors.Is(sendErr, arradapter.ErrReadback) && !errors.Is(sendErr, arradapter.ErrPrepared) &&
		!errors.Is(sendErr, arradapter.ErrQualification) && !errors.Is(sendErr, arradapter.ErrConfiguration) {
		return sendErr
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_, err := w.journal.CancelUnsent(cleanup, key, stage, w.clock.Now())
	return errors.Join(sendErr, err)
}

func emptyPackScope(cp EmptyPackCheckpoint) arradapter.EmptyPackScope {
	return arradapter.EmptyPackScope{QueueIDs: cp.QueueIDs, DownloadID: cp.DownloadID, Release: cp.Release,
		SeriesID: cp.SeriesID, EpisodeIDs: cp.EpisodeIDs, OutputPath: cp.outputPath, ClientLabel: cp.clientLabel}
}
