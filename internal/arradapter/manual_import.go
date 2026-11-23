package arradapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/importflow"
	"github.com/rnodern/rescuarr/internal/observe"
)

const ManualSonarrV5CopyRevision = "sonarr-v5-manual-copy-1"

// ManualSonarrV5Copy binds the approved singleton source to the qualified
// command and verifies the command and its effects through independent reads.
type ManualSonarrV5Copy struct {
	adapter *Adapter
	source  importflow.SonarrV5Source
}

type manualImportProbe struct {
	reader EmptyPackAdapter
	arr    *observe.Arr
	source importflow.SonarrV5Source
	now    func() time.Time
}

func NewManualSonarrV5Copy(cfg Config, source importflow.SonarrV5Source, transport http.RoundTripper, qualify Qualify, now func() time.Time) (*ManualSonarrV5Copy, error) {
	if cfg.Instance.Product != domain.ProductSonarr ||
		arrwire.ContractFor(cfg.Instance.Product, cfg.Instance.UpstreamVersion) != arrwire.ContractSonarrV5 ||
		(cfg.AdapterRevision != ManualSonarrV5CopyRevision && cfg.AdapterRevision != arrwire.SonarrV5CleanupCopyRevision) ||
		source.Job.Instance != cfg.Instance.Ref || source.Job.ID == "" ||
		source.Job.Generation != 1 || source.Target.Instance != cfg.Instance.Ref || source.Target.Kind != domain.TargetEpisode ||
		!source.Target.Valid() || source.Size <= 0 || !validPackText(source.Path, 4096) ||
		!validPackText(source.DownloadID, 256) || !validPackText(source.Client, 128) || transport == nil || qualify == nil {
		return nil, ErrConfiguration
	}
	base, valid := parseBase(cfg.Instance.BaseURL)
	if !valid {
		return nil, ErrConfiguration
	}
	arr, err := observe.NewArr(observe.Config{Instance: cfg.Instance, APIKey: cfg.APIKey}, transport)
	if err != nil {
		return nil, err
	}
	probe := manualImportProbe{reader: EmptyPackAdapter{base: base, key: cfg.APIKey, exchange: transport.RoundTrip}, arr: arr, source: source, now: now}
	preflight := func(ctx context.Context, binding Binding) (Qualification, error) {
		qualification, err := qualify(ctx, binding)
		if err != nil {
			return Qualification{}, err
		}
		retained, err := probe.sourceRetained(ctx)
		if err != nil || !retained {
			return Qualification{}, ErrQualification
		}
		if !probe.queueStillMatches(ctx, cfg.AdapterRevision == arrwire.SonarrV5CleanupCopyRevision) {
			return Qualification{}, ErrQualification
		}
		fileID, err := arr.EpisodeFileLink(ctx, source.Target)
		if err != nil || fileID != 0 {
			return Qualification{}, ErrQualification
		}
		return qualification, nil
	}
	adapter, err := newAdapter(cfg, transport.RoundTrip, preflight, probe.readback, now, RouteCopyImport)
	if err != nil {
		return nil, err
	}
	return &ManualSonarrV5Copy{adapter: adapter, source: source}, nil
}

func (a *ManualSonarrV5Copy) AdapterRevision() string { return a.adapter.binding.AdapterRevision }

func (a *ManualSonarrV5Copy) DispatchManualImport(ctx context.Context, op domain.Operation, prepared domain.PreparedAction,
	beforeSend func(context.Context) error) (domain.Acknowledgement, error) {
	if !a.matches(op, prepared) || a.source.QueueID <= 0 || a.source.SelectionDigest == ([32]byte{}) {
		return domain.Acknowledgement{}, ErrPrepared
	}
	if beforeSend == nil {
		return domain.Acknowledgement{}, ErrQualification
	}
	return a.adapter.dispatchPrepared(ctx, op, prepared, beforeSend)
}

func (p *manualImportProbe) queueStillMatches(ctx context.Context, sonarrCleanup bool) bool {
	queue, err := p.arr.Queue(ctx, p.source.Job.Generation, p.now())
	if err != nil {
		return false
	}
	found := false
	for _, row := range queue.Records {
		if sonarrCleanup && row.DownloadID != p.source.DownloadID &&
			importflow.DownloadPathsOverlap(importflow.WindowsPath, p.source.OutputPath, row.OutputPath) {
			return false
		}
		if row.ID != p.source.QueueID && row.DownloadID != p.source.DownloadID && row.Target != p.source.Target &&
			row.OutputPath != p.source.OutputPath && row.OutputPath != p.source.Path {
			continue
		}
		if found || row.ID != p.source.QueueID || row.DownloadID != p.source.DownloadID || row.Target != p.source.Target ||
			row.OutputPath != p.source.OutputPath || row.ClientLabel != p.source.Client || row.Protocol != domain.ProtocolUsenet ||
			row.Status != "completed" || row.TrackedState != "importBlocked" || len(row.Findings) != 1 ||
			row.Findings[0] != observe.FindingSeriesIDOnly {
			return false
		}
		found = true
	}
	return found
}

func (a *ManualSonarrV5Copy) Reconcile(ctx context.Context, attempt domain.Attempt) (domain.Outcome, error) {
	if attempt.Prepared == nil || !a.matches(attempt.Operation, *attempt.Prepared) {
		return domain.Outcome{}, ErrReadback
	}
	return a.adapter.Reconcile(ctx, attempt)
}

func (a *ManualSonarrV5Copy) matches(op domain.Operation, prepared domain.PreparedAction) bool {
	return op.Action == domain.ActionImport && op.Job == a.source.Job && len(op.Targets) == 1 && op.Targets[0] == a.source.Target &&
		prepared.Import != nil && prepared.Import.DownloadID == a.source.DownloadID && len(prepared.Import.Files) == 1 &&
		prepared.Import.Files[0].Path == a.source.Path &&
		prepared.Import.SonarrCleanup == (a.AdapterRevision() == arrwire.SonarrV5CleanupCopyRevision)
}

// A direct Sonarr disk read uses the already-selected file and does not depend
// on the download remaining in the queue. It reports presence and size.
func (p *manualImportProbe) sourceRetained(ctx context.Context) (bool, error) {
	var candidates []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	query := url.Values{"folder": {p.source.Path}, "filterExistingFiles": {"false"}}
	if err := p.reader.readPackJSON(ctx, "/api/v3/manualimport", query, &candidates); err != nil {
		return false, err
	}
	return len(candidates) == 1 && candidates[0].Path == p.source.Path && candidates[0].Size == p.source.Size, nil
}

type importCommandState int

const (
	importCommandPending importCommandState = iota
	importCommandSucceeded
	importCommandFailed
)

func (p *manualImportProbe) commandState(ctx context.Context, reference string) (importCommandState, error) {
	id, err := strconv.ParseInt(reference, 10, 32)
	if err != nil || id <= 0 || reference != strconv.FormatInt(id, 10) {
		return importCommandPending, ErrReadback
	}
	var command struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Result string `json:"result"`
	}
	if err := p.reader.readPackJSON(ctx, "/api/v3/command/"+reference, nil, &command); err != nil {
		return importCommandPending, err
	}
	if command.ID != id || command.Name != commandName(RouteCopyImport) {
		return importCommandPending, ErrReadback
	}
	switch command.Status {
	case "completed":
		switch command.Result {
		case "successful":
			return importCommandSucceeded, nil
		case "unsuccessful":
			return importCommandFailed, nil
		}
	case "failed", "aborted", "cancelled":
		return importCommandFailed, nil
	}
	// An orphaned command or indeterminate result might have already sent work.
	return importCommandPending, nil
}

func (p *manualImportProbe) readback(ctx context.Context, route Route, attempt domain.Attempt) (domain.Outcome, error) {
	if route != RouteCopyImport {
		return domain.Outcome{}, ErrReadback
	}
	out := domain.Outcome{AttemptID: attempt.ID, Verification: domain.VerificationUnresolved}
	acknowledged := attempt.Acknowledgement.State == domain.DispatchAcknowledged
	if acknowledged {
		state, err := p.commandState(ctx, attempt.Acknowledgement.UpstreamReference)
		if err != nil || state == importCommandPending {
			out.ObservedAt = p.now()
			return out, err
		}
		if state == importCommandFailed {
			return p.failedReadback(ctx, attempt, out)
		}
	}
	history, err := p.arr.SonarrV5ImportProof(ctx, p.source.Target, p.source.DownloadID, p.source.Path, p.source.Size, attempt.SentAt, p.now())
	if err != nil || history.ID == 0 {
		out.ObservedAt = p.now()
		return out, err
	}
	libraryVerification := domain.VerificationExternal
	if acknowledged {
		libraryVerification = domain.VerificationEffect
	}
	out.Verification = domain.VerificationPartial
	out.Effects = []domain.EffectResult{{Effect: domain.Effect{Kind: domain.EffectLibraryImport, Target: p.source.Target},
		Verification: libraryVerification, Evidence: manualImportEvidence(attempt.ID, "library_import", history.ID, history.FileID)}}
	if attempt.Prepared.Import.SonarrCleanup {
		// Confirm Sonarr can still see the actual library file after its own
		// cleanup. Library metadata alone does not establish that it survived.
		var file struct {
			Type string `json:"type"`
		}
		query := url.Values{"path": {history.ImportedPath}}
		err := p.reader.readPackJSON(ctx, "/api/v3/filesystem/type", query, &file)
		out.ObservedAt = p.now()
		if err == nil && file.Type == "file" && acknowledged {
			out.Verification = domain.VerificationEffect
		}
		return out, err
	}
	retained, err := p.sourceRetained(ctx)
	out.ObservedAt = p.now()
	if err != nil {
		return out, err
	}
	if retained {
		out.Effects = append(out.Effects, domain.EffectResult{Effect: domain.Effect{Kind: domain.EffectSourceRetained},
			Verification: domain.VerificationEffect, Evidence: manualImportEvidence(attempt.ID, "source_retained", history.ID, p.source.Size)})
		if acknowledged {
			out.Verification = domain.VerificationEffect
		}
	}
	return out, nil
}

func (p *manualImportProbe) failedReadback(ctx context.Context, attempt domain.Attempt, out domain.Outcome) (domain.Outcome, error) {
	fileID, err := p.arr.EpisodeFileLink(ctx, p.source.Target)
	if err != nil || fileID != 0 {
		return out, err
	}
	history, err := p.arr.EpisodeImportHistory(ctx, p.source.Target)
	if err != nil {
		return out, err
	}
	for _, record := range history {
		if record.DownloadID == p.source.DownloadID && !record.At.Before(attempt.SentAt.Truncate(time.Second)) {
			return out, nil
		}
	}
	retained, err := p.sourceRetained(ctx)
	if err != nil || !retained {
		return out, err
	}
	current, err := p.arr.EpisodeFileLink(ctx, p.source.Target)
	out.ObservedAt = p.now()
	if err != nil || current != 0 {
		return out, err
	}
	out.Verification = domain.VerificationNoEffect
	return out, nil
}

func manualImportEvidence(attempt domain.AttemptID, kind domain.EffectKind, a, b int64) []domain.EvidenceID {
	digest := sha256.Sum256(fmt.Appendf(nil, "%s:%s:%d:%d", attempt, kind, a, b))
	return []domain.EvidenceID{domain.EvidenceID(hex.EncodeToString(digest[:]))}
}
