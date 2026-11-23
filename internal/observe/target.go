package observe

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	// Baseline builds for the v4 and Radarr target contracts.
	SonarrTargetVersion = arrwire.SonarrVersion
	RadarrTargetVersion = arrwire.RadarrVersion

	TargetProfileEvidence domain.ObservationKind = "import_target_profile"
)

var ErrTargetVersion = errors.New("target evidence requires a supported Arr version")

// TargetSnapshot contains the current exact Arr target and its profile and file
// link. The paths are in Arr's filesystem namespace.
type TargetSnapshot struct {
	Ref                domain.TargetRef
	Evidence           domain.Observation
	Version            string
	ConnectionRevision domain.Revision
	Monitored          domain.Fact[bool]
	Available          domain.Fact[bool]
	AirDateUTC         *time.Time
	EpisodeCode        string `json:",omitempty"`
	MediaPath          string `json:"-"`
	RootPath           string `json:"-"`
	OriginalLanguageID *int32
	ProfileID          int64
	ProfileRevision    domain.Revision
	Profile            *QualityProfile
	FileID             domain.Fact[int64]
	File               *LibraryFile
}

type mediaWire struct {
	ID        int64           `json:"id"`
	ProfileID int64           `json:"qualityProfileId"`
	Monitored *bool           `json:"monitored"`
	Path      string          `json:"path"`
	RootPath  string          `json:"rootFolderPath"`
	Language  *resourceIDWire `json:"originalLanguage"`
}

type targetFileWire struct {
	ID int64 `json:"id"`
}

type currentTarget struct {
	mediaWire
	EpisodeMonitored *bool
	SeasonNumber     *int32
	EpisodeNumber    *int32
	HasFile          *bool
	FileID           *int32
	EmbeddedFile     *targetFileWire
	Available        *bool
	AirDateUTC       *time.Time
}

// Target reads one exact target, its selected quality profile and, when the
// target links it, one exact file. It rechecks the target and profile for drift.
func (a *Arr) Target(ctx context.Context, ref domain.TargetRef, generation domain.Revision, observedAt time.Time) (TargetSnapshot, error) {
	want := arrwire.ContractSonarrV4
	if a.reader.product == domain.ProductRadarr {
		want = arrwire.ContractRadarr
	}
	return a.targetSnapshot(ctx, ref, generation, observedAt, want)
}

// SonarrV5ImportTarget reads an exact episode and profile in the Sonarr v5
// contract's manual import workflow.
func (a *Arr) SonarrV5ImportTarget(ctx context.Context, ref domain.TargetRef, generation domain.Revision, observedAt time.Time) (TargetSnapshot, error) {
	if a.reader.product != domain.ProductSonarr {
		return TargetSnapshot{}, ErrConfiguration
	}
	return a.targetSnapshot(ctx, ref, generation, observedAt, arrwire.ContractSonarrV5)
}

func (a *Arr) targetSnapshot(ctx context.Context, ref domain.TargetRef, generation domain.Revision, observedAt time.Time,
	want arrwire.Contract) (TargetSnapshot, error) {
	if !ref.Valid() || ref.Instance != a.reader.instance || ref.ID > math.MaxInt32 || ref.SeriesID > math.MaxInt32 ||
		(a.reader.product == domain.ProductSonarr) != (ref.Kind == domain.TargetEpisode) ||
		generation == 0 || observedAt.IsZero() {
		return TargetSnapshot{}, ErrConfiguration
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	out := TargetSnapshot{Ref: ref, ConnectionRevision: a.connectionRevision}
	availability, err := a.readTarget(ctx, &out, want)
	state, fileState := domain.ObservationKnown, domain.ObservationKnown
	var monitored bool
	var fileID int64
	var path, relativePath, edition string
	switch {
	case errors.Is(err, ErrTargetVersion):
		state, fileState = domain.ObservationUnsupported, domain.ObservationUnsupported
	case err != nil:
		state, fileState = domain.ObservationUnknown, domain.ObservationUnknown
	default:
		monitored, fileID = out.Monitored.Value, out.FileID.Value
		if fileID == 0 {
			fileState = domain.ObservationAbsent
		} else {
			path, relativePath, edition = out.File.Path, out.File.RelativePath, out.File.Edition
		}
	}
	record := strconv.FormatInt(ref.ID, 10)
	content := struct {
		Ref                domain.TargetRef
		Version            string
		ConnectionRevision domain.Revision
		Monitored          bool
		Available          *bool
		AirDateUTC         *time.Time
		EpisodeCode        string `json:",omitempty"`
		MediaPath          string
		RootPath           string
		OriginalLanguageID *int32
		Profile            *QualityProfile
		FileID             int64
		File               *LibraryFile
		FilePath           string
		RelativeFilePath   string
		FileEdition        string
	}{
		Ref: ref, Version: out.Version, ConnectionRevision: out.ConnectionRevision, Monitored: monitored,
		Available: availability, AirDateUTC: out.AirDateUTC, EpisodeCode: out.EpisodeCode, MediaPath: out.MediaPath, RootPath: out.RootPath,
		OriginalLanguageID: out.OriginalLanguageID, Profile: out.Profile, FileID: fileID, File: out.File,
		FilePath: path, RelativeFilePath: relativePath, FileEdition: edition,
	}
	out.Evidence = a.observationState(TargetProfileEvidence, record, generation, observedAt, content, state)
	out.Evidence.SourceRecord = record
	out.Monitored = domain.Fact[bool]{Value: monitored, Observation: a.observationState(
		"target_monitored", record, generation, observedAt, monitored, state,
	)}
	availabilityState := state
	if state == domain.ObservationKnown && availability == nil {
		availabilityState = domain.ObservationUnknown
	}
	if availability != nil {
		out.Available.Value = *availability
	}
	out.Available.Observation = a.observationState(
		"target_available", record, generation, observedAt, availability, availabilityState,
	)
	out.FileID = domain.Fact[int64]{Value: fileID, Observation: a.observationState(
		"target_file_link", record, generation, observedAt, fileID, fileState,
	)}
	out.Monitored.Observation.SourceRecord = record
	out.Available.Observation.SourceRecord = record
	out.FileID.Observation.SourceRecord = record
	return out, err
}

func (a *Arr) readTarget(ctx context.Context, out *TargetSnapshot, want arrwire.Contract) (*bool, error) {
	status, err := a.Status(ctx)
	if err != nil {
		return nil, err
	}
	out.Version = status.Version
	if arrwire.ContractFor(a.reader.product, status.Version) != want ||
		(a.upstreamVersion != "" && status.Version != a.upstreamVersion) {
		return nil, ErrTargetVersion
	}
	first, err := a.currentTarget(ctx, out.Ref)
	if err != nil {
		return nil, err
	}
	profile, err := a.qualityProfile(ctx, first.ProfileID)
	if err != nil {
		return nil, err
	}
	fileID := int64(*first.FileID)
	var file *LibraryFile
	if fileID != 0 {
		linked, err := a.libraryFile(ctx, out.Ref, fileID, first.SeasonNumber)
		if err != nil {
			return nil, err
		}
		file = &linked
	}
	current, err := a.currentTarget(ctx, out.Ref)
	if err != nil {
		return nil, err
	}
	if digestObservationContent(first) != digestObservationContent(current) {
		return nil, ErrPartial
	}
	// The profile can change in place without changing its ID on the target.
	currentProfile, err := a.qualityProfile(ctx, current.ProfileID)
	if err != nil {
		return nil, err
	}
	if currentProfile.Digest != profile.Digest {
		return nil, ErrPartial
	}
	if want == arrwire.ContractSonarrV5 {
		if first.SeasonNumber == nil || first.EpisodeNumber == nil || *first.EpisodeNumber <= 0 {
			return nil, ErrResponse
		}
		out.EpisodeCode = fmt.Sprintf("S%02dE%02d", *first.SeasonNumber, *first.EpisodeNumber)
	}
	out.MediaPath, out.RootPath = exactToken(first.Path, 4096), exactToken(first.RootPath, 4096)
	if first.Language != nil && first.Language.ID != nil && *first.Language.ID > 0 {
		out.OriginalLanguageID = first.Language.ID
	}
	out.Monitored.Value = *first.Monitored && (first.EpisodeMonitored == nil || *first.EpisodeMonitored)
	if first.AirDateUTC != nil && !first.AirDateUTC.IsZero() {
		out.AirDateUTC = new(first.AirDateUTC.UTC())
	}
	out.ProfileID, out.ProfileRevision, out.Profile = profile.ID, profile.Revision, &profile
	out.FileID.Value, out.File = fileID, file
	return first.Available, nil
}

func (a *Arr) currentTarget(ctx context.Context, ref domain.TargetRef) (currentTarget, error) {
	var current currentTarget
	if ref.Kind == domain.TargetEpisode {
		var episode struct {
			ID            int64           `json:"id"`
			SeriesID      int64           `json:"seriesId"`
			Monitored     *bool           `json:"monitored"`
			SeasonNumber  *int32          `json:"seasonNumber"`
			EpisodeNumber *int32          `json:"episodeNumber"`
			AirDateUTC    *time.Time      `json:"airDateUtc"`
			HasFile       *bool           `json:"hasFile"`
			FileID        *int32          `json:"episodeFileId"`
			File          *targetFileWire `json:"episodeFile"`
		}
		if err := a.getTargetResource(ctx, "episode", ref.ID, &episode); err != nil {
			return current, err
		}
		if episode.ID != ref.ID || episode.SeriesID != ref.SeriesID || episode.Monitored == nil ||
			(episode.SeasonNumber != nil && *episode.SeasonNumber < 0) || !validFileLink(episode.FileID, episode.HasFile, episode.File) {
			return current, ErrResponse
		}
		if err := a.getTargetResource(ctx, "series", ref.SeriesID, &current.mediaWire); err != nil {
			return current, err
		}
		if !current.mediaWire.matches(ref.SeriesID) {
			return current, ErrResponse
		}
		current.EpisodeMonitored, current.SeasonNumber, current.EpisodeNumber = episode.Monitored, episode.SeasonNumber, episode.EpisodeNumber
		current.AirDateUTC = episode.AirDateUTC
		current.HasFile, current.FileID, current.EmbeddedFile = episode.HasFile, episode.FileID, episode.File
		return current, nil
	}
	var movie struct {
		mediaWire
		Available *bool           `json:"isAvailable"`
		HasFile   *bool           `json:"hasFile"`
		FileID    *int32          `json:"movieFileId"`
		File      *targetFileWire `json:"movieFile"`
	}
	if err := a.getTargetResource(ctx, "movie", ref.ID, &movie); err != nil {
		return current, err
	}
	if !movie.mediaWire.matches(ref.ID) || !validFileLink(movie.FileID, movie.HasFile, movie.File) {
		return current, ErrResponse
	}
	current.mediaWire, current.Available = movie.mediaWire, movie.Available
	current.HasFile, current.FileID, current.EmbeddedFile = movie.HasFile, movie.FileID, movie.File
	return current, nil
}

func (w mediaWire) matches(id int64) bool {
	return w.ID == id && w.ProfileID > 0 && w.ProfileID <= math.MaxInt32 && w.Monitored != nil
}

func validFileLink(id *int32, hasFile *bool, file *targetFileWire) bool {
	return id != nil && *id >= 0 && (hasFile == nil || *hasFile == (*id > 0)) &&
		(file == nil || (*id > 0 && file.ID == int64(*id)))
}

func (a *Arr) getTargetResource(ctx context.Context, resource string, id int64, dest any) error {
	return a.reader.get(ctx, "/api/v3/"+resource+"/"+strconv.FormatInt(id, 10), nil, true, dest)
}
