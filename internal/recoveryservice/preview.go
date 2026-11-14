package recoveryservice

import (
	"context"
	"errors"
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
	languages app.LanguageSettingsService
}

func NewPreviewService(cases *observeservice.Service, secrets interface{ ReadSecret(string) (string, error) }, transport http.RoundTripper,
	clock app.Clock, languages app.LanguageSettingsService) *PreviewService {
	return &PreviewService{cases: cases, secrets: secrets, transport: transport, clock: clock, languages: languages, slots: make(chan struct{}, 1)}
}

// ImportPreview displays Arr's current assessment for the exact completed job.
// The filename review also shows the two basenames used to identify an episode.
func (s *PreviewService) ImportPreview(ctx context.Context, id domain.CaseID, revision domain.Revision) (app.ImportPreview, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return heldPreview("Another import preview is running. Try again in a moment.")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	proof, err := s.cases.FreshRecoveryQueue(ctx, id, revision)
	if errors.Is(err, app.ErrHeld) || errors.Is(err, observe.ErrPartial) {
		return heldPreview("Rescuarr could not confirm the whole download in a recent queue check. Check the service queue again, then reopen this case.")
	}
	if err != nil {
		return app.ImportPreview{}, err
	}
	if proof.Instance.Product == domain.ProductSonarr && proof.Case.Reason == domain.ReasonCode(observe.FindingNoEligibleFiles) {
		return s.previewMissingFiles(ctx, id, revision, proof)
	}
	if len(proof.Members) == 0 || len(proof.Members) > 8 {
		return heldPreview("Import preview supports jobs with up to eight identified targets.")
	}
	first := proof.Members[0]
	filenameReview := proof.Case.Reason == domain.ReasonCode(observe.FindingSeriesIDOnly)
	if filenameReview {
		if len(proof.Members) != 1 || !importflow.SupportsEpisodeNameReview(proof.Instance.Product, proof.Instance.UpstreamVersion) {
			return heldFilenameCheck("Filename matching requires one episode on a supported Sonarr version.")
		}
	} else {
		for _, row := range proof.Members {
			if row.Status != "completed" || row.TrackedState != "importPending" {
				return heldPreview("Arr has not marked this download as complete and ready for import. Check the download in the Arr queue.")
			}
			if row.OutputPath == "" || row.OutputPath != first.OutputPath {
				return heldPreview("Arr did not report one consistent download location for these targets. Check the download in Arr.")
			}
		}
	}
	release, err := s.cases.HoldRecoveryConnection(ctx, proof.Instance)
	if err != nil {
		return app.ImportPreview{}, err
	}
	defer release()
	key, err := s.secrets.ReadSecret(proof.Instance.SecretRef)
	if err != nil {
		return heldPreview("The connection’s API key is unavailable. Check the service in Connections.")
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
		return heldPreview("Arr did not identify a supported filesystem path format.")
	}
	roots, err := arr.RootPaths(ctx)
	if err != nil {
		return app.ImportPreview{}, err
	}
	client, err := importflow.New(cfg, s.transport)
	if err != nil {
		return app.ImportPreview{}, err
	}
	if filenameReview {
		match, err := client.ReviewEpisodeNames(ctx, first, style, roots)
		if errors.Is(err, observe.ErrPartial) {
			return heldFilenameCheck("Sonarr returned too many files to identify one episode.")
		}
		if errors.Is(err, observe.ErrUnavailable) || errors.Is(err, observe.ErrNotFound) || errors.Is(err, observe.ErrResponse) {
			return heldFilenameCheck("Rescuarr could not read the download’s file list from Sonarr.")
		}
		if err != nil {
			return app.ImportPreview{}, err
		}
		view := app.ImportPreview{Episode: &match}
		if s.languages != nil {
			prefs, mode, err := s.languageContext(ctx)
			if err != nil {
				return app.ImportPreview{}, err
			}
			original, _ := arr.OriginalAudioLanguage(ctx, first.Target)
			view.Audio = []app.AudioLanguageReview{reviewAudio(prefs, mode, original.Name, "Download audio", filenameLanguageHints(first.Release, match.Folder, match.Filename),
				grabLanguageLabels(ctx, arr, proof, s.clock.Now()), nil, false)}
		}
		return view, nil
	}
	prefs, mode, err := s.languageContext(ctx)
	if err != nil {
		return app.ImportPreview{}, err
	}
	originalLanguages := make(map[domain.TargetRef]string)
	job := importflow.Job{Ref: proof.Case.Job.Ref, DownloadID: first.DownloadID, OutputPath: first.OutputPath,
		LibraryRoots: roots, PathStyle: style, Queue: proof.Queue, TrackingComplete: true}
	for _, target := range proof.Case.Targets {
		snapshot, err := arr.Target(ctx, target.Ref, proof.Queue.Evidence.SnapshotGeneration, s.clock.Now())
		if errors.Is(err, observe.ErrTargetVersion) {
			return heldPreview("This Arr version supports queue monitoring. Import preview is unavailable on this version.")
		}
		if err != nil {
			return app.ImportPreview{}, err
		}
		if s.languages != nil {
			original, _ := arr.OriginalAudioLanguage(ctx, target.Ref)
			originalLanguages[target.Ref] = original.Name
			if snapshot.File != nil {
				tracks, complete := snapshot.File.AudioTracks()
				view, err := heldPreview("The target already has a library file.")
				view.Audio = []app.AudioLanguageReview{reviewAudio(prefs, mode, original.Name, "Current library audio", nil, nil, tracks, complete)}
				return view, err
			}
		}
		if !snapshot.Monitored.Value || snapshot.FileID.Observation.State != domain.ObservationAbsent {
			return heldPreview("This preview covers monitored targets waiting for their first file. Check existing files and monitoring in Arr.")
		}
		if !slices.Contains(roots, snapshot.RootPath) {
			return heldPreview("Arr did not match the target to a configured library folder.")
		}
		job.Targets = append(job.Targets, importflow.Target{
			Ref: snapshot.Ref, ProfileID: snapshot.ProfileID, ProfileRevision: snapshot.ProfileRevision,
			Monitored: true, Intent: policy.NeedFirstImport, Evidence: snapshot.Evidence,
		})
	}
	result, err := client.Discover(ctx, job, s.clock.Now())
	if err != nil {
		return app.ImportPreview{}, err
	}
	view := app.ImportPreview{Reason: result.Reason, Candidates: len(result.Candidates)}
	for _, candidate := range result.Candidates {
		accepted := candidate.State == importflow.Eligible
		if s.languages != nil {
			var native []string
			for _, claim := range candidate.Languages {
				native = append(native, claim.Name)
			}
			filename := filenameLanguageHints(first.Release, candidate.FolderName, candidate.Path)
			for _, ref := range candidate.Targets {
				original, inScope := originalLanguages[ref]
				if !inScope {
					continue
				}
				review := reviewAudio(prefs, mode, original, "Download audio", filename, native, nil, false)
				view.Audio = append(view.Audio, review)
				accepted = accepted && review.Decision == domain.AudioAccept
			}
		}
		if accepted {
			view.Eligible++
		}
	}
	return view, nil
}

func (s *PreviewService) languageContext(ctx context.Context) (app.LanguagePreferences, domain.Mode, error) {
	if s.languages == nil {
		return app.DefaultLanguagePreferences(), domain.ModeObserve, nil
	}
	prefs, err := s.languages.LanguageSettings(ctx)
	if err != nil {
		return prefs, "", err
	}
	status, err := s.cases.Overview(ctx)
	return prefs, status.Mode, err
}

func heldPreview(reason string) (app.ImportPreview, error) {
	return app.ImportPreview{Hold: reason}, app.ErrHeld
}

func heldFilenameCheck(reason string) (app.ImportPreview, error) {
	return app.ImportPreview{Episode: &domain.EpisodeNameReview{Reason: reason}}, app.ErrHeld
}
