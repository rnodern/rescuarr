package recoveryservice

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/importflow"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/policy"
)

type PreviewService struct {
	cases     *observeservice.Service
	secrets   interface{ ReadSecret(string) (string, error) }
	transport http.RoundTripper
	clock     app.Clock
	slots     chan struct{}
}

func NewPreviewService(cases *observeservice.Service, secrets interface{ ReadSecret(string) (string, error) }, transport http.RoundTripper, clock app.Clock) *PreviewService {
	return &PreviewService{cases: cases, secrets: secrets, transport: transport, clock: clock, slots: make(chan struct{}, 1)}
}

// ImportPreview displays Arr's current assessment for the exact completed job.
// The browser receives aggregates only. No candidates become write proposals.
func (s *PreviewService) ImportPreview(ctx context.Context, id domain.CaseID, revision domain.Revision) (app.ImportPreview, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return app.ImportPreview{}, app.ErrHeld
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	proof, err := s.cases.FreshRecoveryQueue(ctx, id, revision)
	if err != nil {
		return app.ImportPreview{}, err
	}
	if len(proof.Members) == 0 || len(proof.Members) > 8 {
		return app.ImportPreview{}, app.ErrHeld
	}
	first := proof.Members[0]
	for _, row := range proof.Members {
		if row.Status != "completed" || row.TrackedState != "importPending" || row.OutputPath == "" || row.OutputPath != first.OutputPath {
			return app.ImportPreview{}, app.ErrHeld
		}
	}
	key, err := s.secrets.ReadSecret(proof.Instance.SecretRef)
	if err != nil {
		return app.ImportPreview{}, app.ErrHeld
	}
	cfg := observe.Config{Instance: proof.Instance, APIKey: key}
	arr, err := observe.NewArr(cfg, s.transport)
	if err != nil {
		return app.ImportPreview{}, err
	}
	status, err := arr.Status(ctx)
	if err != nil {
		return app.ImportPreview{}, err
	}
	if status.Version != proof.Instance.UpstreamVersion {
		return app.ImportPreview{}, app.ErrConflict
	}
	style := importflow.PathStyle(status.PathOS)
	if style != importflow.UnixPath && style != importflow.WindowsPath {
		return app.ImportPreview{}, app.ErrHeld
	}
	roots, err := arr.RootPaths(ctx)
	if err != nil {
		return app.ImportPreview{}, err
	}
	job := importflow.Job{Ref: proof.Case.Job.Ref, DownloadID: first.DownloadID, OutputPath: first.OutputPath,
		LibraryRoots: roots, PathStyle: style, Queue: proof.Queue, TrackingComplete: true}
	for _, target := range proof.Case.Targets {
		snapshot, err := arr.Target(ctx, target.Ref, proof.Queue.Evidence.SnapshotGeneration, s.clock.Now())
		if err != nil {
			return app.ImportPreview{}, err
		}
		if !snapshot.Monitored.Value || snapshot.FileID.Observation.State != domain.ObservationAbsent ||
			!slices.Contains(roots, snapshot.RootPath) {
			return app.ImportPreview{}, app.ErrHeld
		}
		job.Targets = append(job.Targets, importflow.Target{
			Ref: snapshot.Ref, ProfileID: snapshot.ProfileID, ProfileRevision: snapshot.ProfileRevision,
			Monitored: true, Intent: policy.NeedFirstImport, Evidence: snapshot.Evidence,
		})
	}
	client, err := importflow.New(cfg, s.transport)
	if err != nil {
		return app.ImportPreview{}, err
	}
	result, err := client.Discover(ctx, job, s.clock.Now())
	if err != nil {
		return app.ImportPreview{}, err
	}
	view := app.ImportPreview{Reason: result.Reason, Candidates: len(result.Candidates)}
	for _, candidate := range result.Candidates {
		if candidate.State == importflow.Eligible {
			view.Eligible++
		}
	}
	return view, nil
}
