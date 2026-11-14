package recoveryservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/importflow"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/policy"
	"github.com/rnodern/rescuarr/internal/store"
)

type manualImportPlan struct {
	instance         domain.Instance
	source           importflow.SonarrV5Source
	job              importflow.Job
	scopeHash        string
	languageRevision domain.Revision
	preview          app.ImportPreview
	release          string
}

// prepareManualImport builds a fresh single-file proposal using only reads.
// The plan keeps full Arr paths out of browser views and Activity.
func prepareManualImport(ctx context.Context, cases *observeservice.Service, local *store.Store, clock app.Clock,
	transport http.RoundTripper, id domain.CaseID, revision domain.Revision) (manualImportPlan, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	proof, err := cases.FreshRecoveryQueue(ctx, id, revision)
	if err != nil {
		return manualImportPlan{}, err
	}
	plan := manualImportPlan{instance: proof.Instance}
	if !manualImportScope(proof) {
		return plan.hold("Approval requires one completed Usenet episode held only for the Sonarr series ID match.")
	}
	first := proof.Members[0]
	plan.release = first.Release
	unlock, err := cases.HoldRecoveryConnection(ctx, proof.Instance)
	if err != nil {
		return plan, err
	}
	defer unlock()

	key, err := local.ReadSecret(proof.Instance.SecretRef)
	if err != nil {
		return plan, observe.ErrAccess
	}
	cfg := observe.Config{Instance: proof.Instance, APIKey: key}
	arr, err := observe.NewArr(cfg, transport)
	if err != nil {
		return plan, err
	}
	status, err := arr.Status(ctx)
	if err != nil {
		return plan, err
	}
	if status.Version != proof.Instance.UpstreamVersion {
		return plan, app.ErrConflict
	}
	style := importflow.PathStyle(status.PathOS)
	if style != importflow.UnixPath && style != importflow.WindowsPath {
		return plan.hold("Sonarr did not report a supported filesystem format.")
	}
	roots, err := arr.RootPaths(ctx)
	if err != nil {
		return plan, err
	}
	slices.Sort(roots)
	target, err := arr.SonarrV5ImportTarget(ctx, first.Target, proof.Queue.Evidence.SnapshotGeneration, clock.Now())
	if err != nil {
		return plan, err
	}
	if !target.Monitored.Value || target.FileID.Observation.State != domain.ObservationAbsent ||
		target.EpisodeCode != first.Episode {
		return plan.hold("Sonarr must confirm the same monitored episode is still waiting for its first library file.")
	}
	if !slices.Contains(roots, target.RootPath) {
		return plan.hold("Sonarr did not match the series to a configured library folder.")
	}

	prefs, err := app.NewLanguageSettings(local, clock).LanguageSettings(ctx)
	if err != nil {
		return plan, err
	}
	original, _ := arr.OriginalAudioLanguage(ctx, first.Target)
	labels := grabLanguageLabels(ctx, arr, proof, clock.Now())

	// Read the whole queue again after fetching the target and language
	// evidence so a competing job or a changed download cannot enter the plan.
	current, err := cases.FreshRecoveryQueue(ctx, id, revision)
	if err != nil {
		return plan, err
	}
	if !manualImportScope(current) || current.Instance != proof.Instance || current.Case.Job.Ref != proof.Case.Job.Ref ||
		current.Queue.Evidence.SnapshotGeneration != target.Evidence.SnapshotGeneration ||
		!sameManualImportMember(current.Members[0], first) {
		return plan, app.ErrConflict
	}
	client, err := importflow.New(cfg, transport)
	if err != nil {
		return plan, err
	}
	plan.job = importflow.Job{
		Ref: current.Case.Job.Ref, DownloadID: first.DownloadID, OutputPath: first.OutputPath,
		LibraryRoots: roots, PathStyle: style, Queue: current.Queue, TrackingComplete: true,
		Targets: []importflow.Target{{
			Ref: target.Ref, ProfileID: target.ProfileID, ProfileRevision: target.ProfileRevision,
			Monitored: true, Intent: policy.NeedFirstImport, Evidence: target.Evidence,
		}},
	}
	selection, err := client.InspectSonarrV5Import(ctx, plan.job, clock.Now())
	if selection.Episode.Series != "" {
		plan.preview.Episode = &selection.Episode
	}
	switch {
	case errors.Is(err, importflow.ErrSonarrV5Candidate):
		return plan.hold("Sonarr did not confirm one import candidate with the expected file and episode.")
	case errors.Is(err, importflow.ErrStale), errors.Is(err, importflow.ErrInvalid):
		return plan.hold("The download’s identity or location could not be confirmed in the complete Sonarr queue.")
	case err != nil:
		return plan, err
	}
	plan.source = selection.Source
	plan.preview.Candidates = 1
	for _, language := range selection.Languages {
		if language.ID > 0 && !slices.Contains(labels, language.Name) {
			labels = append(labels, language.Name)
		}
	}
	slices.Sort(labels)
	audio := reviewAudio(prefs, domain.ModeAssist, original.Name, "Download audio",
		filenameLanguageHints(first.Release, selection.Episode.Folder, selection.Episode.Filename), labels, nil, false)
	plan.preview.Audio = []app.AudioLanguageReview{audio}
	plan.languageRevision = prefs.Revision
	if audio.Decision == domain.AudioReject {
		return plan.hold("The configured audio policy rejects this file.")
	}
	plan.preview.Eligible = 1

	content, err := json.Marshal(struct {
		CaseRevision domain.Revision
		Connection   domain.Revision
		Version      string
		Job          domain.JobRef
		Target       domain.TargetRef
		Selection    [32]byte
		PathStyle    importflow.PathStyle
		Roots        []string
		Language     domain.Revision
		Preview      app.ImportPreview
	}{
		CaseRevision: revision, Connection: plan.instance.ConnectionRevision, Version: status.Version,
		Job: plan.job.Ref, Target: target.Ref, Selection: plan.source.SelectionDigest,
		PathStyle: style, Roots: roots, Language: plan.languageRevision, Preview: plan.preview,
	})
	if err != nil {
		return plan, err
	}
	digest := sha256.Sum256(content)
	plan.scopeHash = hex.EncodeToString(digest[:])
	return plan, nil
}

func (p manualImportPlan) hold(reason string) (manualImportPlan, error) {
	p.preview.Hold = reason
	return p, app.ErrHeld
}

func manualImportScope(proof observeservice.RecoveryQueue) bool {
	return proof.Instance.Product == domain.ProductSonarr && proof.Instance.UpstreamVersion == arrwire.SonarrIDOnlyImportVersion &&
		proof.Case.State == domain.CaseHeld && proof.Case.Reason == domain.ReasonCode(observe.FindingSeriesIDOnly) &&
		proof.Case.Job.Protocol == domain.ProtocolUsenet && !proof.Case.Job.Shared &&
		len(proof.Case.Incidents) == 0 && len(proof.Case.Targets) == 1 && len(proof.Members) == 1 &&
		proof.Case.Targets[0].Ref == proof.Members[0].Target
}

func sameManualImportMember(a, b observe.QueueRecord) bool {
	return a.ID == b.ID && a.Target == b.Target && a.DownloadID == b.DownloadID && a.Release == b.Release &&
		a.OutputPath == b.OutputPath && a.ClientLabel == b.ClientLabel && a.Protocol == b.Protocol &&
		a.Status == b.Status && a.TrackedState == b.TrackedState && slices.Equal(a.Findings, b.Findings) &&
		a.Episode == b.Episode && a.EpisodeTitle == b.EpisodeTitle && a.MediaTitle == b.MediaTitle
}
