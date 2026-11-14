package importflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/policy"
)

var (
	ErrSonarrV5Qualification = errors.New("qualified Sonarr 5 ID-only source is unavailable")
	ErrSonarrV5Candidate     = errors.New("Sonarr did not confirm one eligible scoped episode candidate")
)

// SonarrV5Source is the private identity of the one native candidate selected
// from the full tracked download, for independent source and route validation.
type SonarrV5Source struct {
	Instance   domain.Instance `json:"-"`
	Job        domain.JobRef
	Target     domain.TargetRef
	DownloadID string `json:"-"`
	Client     string `json:"-"`
	OutputPath string `json:"-"`
	Path       string `json:"-"`
	Size       int64
}

type QualifySonarrV5Source func(context.Context, SonarrV5Source) error

type sonarrV5ReprocessRequest struct {
	Path         string       `json:"path"`
	DownloadID   string       `json:"downloadId"`
	SeriesID     int64        `json:"seriesId"`
	SeasonNumber int          `json:"seasonNumber"`
	EpisodeIDs   []int64      `json:"episodeIds"`
	Quality      wireQuality  `json:"quality"`
	Languages    []v5Language `json:"languages"`
	ReleaseGroup *string      `json:"releaseGroup"`
	IndexerFlags int32        `json:"indexerFlags"`
	ReleaseType  ReleaseType  `json:"releaseType"`
}

type v5Language struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type sonarrV5ReprocessResult struct {
	wireCandidate
	SeasonNumber *int `json:"seasonNumber"`
	Episodes     []struct {
		wireEpisode
		SeasonNumber  *int `json:"seasonNumber"`
		EpisodeNumber *int `json:"episodeNumber"`
	} `json:"episodes"`
}

// ReprocessSonarrV5 binds a verified ID-only queue item to one file. The caller
// must independently qualify source preservation and the precise route before
// the POST. Its response prepares a Copy selector for the action journal. It
// never queues an import command.
func (c *Client) ReprocessSonarrV5(ctx context.Context, job Job, now time.Time, qualify QualifySonarrV5Source) (domain.PreparedImport, error) {
	var empty domain.PreparedImport
	if c.instance.Product != domain.ProductSonarr || c.instance.UpstreamVersion != arrwire.SonarrIDOnlyImportVersion {
		return empty, ErrUnsupported
	}
	if qualify == nil {
		return empty, ErrSonarrV5Qualification
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	started := time.Now()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return empty, ctx.Err()
	}
	row, err := v5IDOnlyQueue(c.instance, job, now.Add(time.Since(started)))
	if err != nil {
		return empty, err
	}
	rows, err := c.get(ctx, job.DownloadID)
	if err != nil {
		return empty, err
	}
	match := checkEpisodeNames(domain.EpisodeNameReview{}, row, job.PathStyle, job.LibraryRoots, rows)
	if len(rows) != 1 || !match.Matched || !pathWithin(job.PathStyle, job.OutputPath, rows[0].Path, false) {
		return empty, ErrSonarrV5Candidate
	}
	candidate := rows[0]
	if candidate.DownloadID == nil || *candidate.DownloadID != row.DownloadID ||
		!cleanText(candidate.FolderName, 512) || candidate.FolderName != match.Folder ||
		(candidate.EpisodeFileID != nil && *candidate.EpisodeFileID != 0) {
		return empty, ErrSonarrV5Candidate
	}
	episode := singleEpisode(row.Episode)
	req, err := v5ReprocessRequest(row, episode, candidate)
	if err != nil {
		return empty, err
	}
	source := SonarrV5Source{Instance: c.instance, Job: job.Ref, Target: row.Target, DownloadID: row.DownloadID,
		Client: row.ClientLabel, OutputPath: row.OutputPath, Path: candidate.Path, Size: candidate.Size}
	if err := qualify(ctx, source); err != nil {
		return empty, ErrSonarrV5Qualification
	}
	if _, err := v5IDOnlyQueue(c.instance, job, now.Add(time.Since(started))); err != nil {
		return empty, err
	}
	results, err := c.reprocessV5(ctx, req)
	if err != nil {
		return empty, err
	}
	if _, err := v5IDOnlyQueue(c.instance, job, now.Add(time.Since(started))); err != nil {
		return empty, err
	}
	file, ok := v5PreparedFile(results, candidate, row, episode)
	if !ok {
		return empty, ErrSonarrV5Candidate
	}
	return domain.PreparedImport{DownloadID: row.DownloadID, Files: []domain.PreparedImportFile{file}}, nil
}

func v5IDOnlyQueue(instance domain.Instance, job Job, now time.Time) (observe.QueueRecord, error) {
	var empty observe.QueueRecord
	if !job.Ref.Instance.Valid() || job.Ref.Instance != instance.Ref || job.Ref.ID == "" || job.Ref.Generation != 1 ||
		instance.ConnectionRevision == 0 || !cleanText(job.DownloadID, 256) || len(job.Targets) != 1 ||
		len(job.LibraryRoots) == 0 || len(job.LibraryRoots) > maxCandidates {
		return empty, ErrInvalid
	}
	if output, ok := parsePath(job.PathStyle, job.OutputPath); !ok || len(output.parts) == 0 {
		return empty, ErrInvalid
	}
	for _, root := range job.LibraryRoots {
		if _, ok := parsePath(job.PathStyle, root); !ok ||
			pathWithin(job.PathStyle, root, job.OutputPath, true) || pathWithin(job.PathStyle, job.OutputPath, root, true) {
			return empty, ErrInvalid
		}
	}
	queue, target := job.Queue.Evidence, job.Targets[0]
	if !job.TrackingComplete || !queue.Complete || queue.Kind != "queue" ||
		!fromOwner(instance, queue, now) || queue.SnapshotGeneration == 0 || len(job.Queue.Records) > 1000 ||
		!target.Ref.Valid() || target.Ref.Instance != instance.Ref || target.Ref.Kind != domain.TargetEpisode ||
		target.Ref.ID > math.MaxInt32 || target.Ref.SeriesID > math.MaxInt32 ||
		target.Intent != policy.NeedFirstImport || !validIntent(target) || target.ProfileID <= 0 || target.ProfileRevision == 0 ||
		!target.Evidence.Complete || target.Evidence.Kind != TargetProfileEvidence ||
		target.Evidence.SourceRecord != strconv.FormatInt(target.Ref.ID, 10) ||
		target.Evidence.SnapshotGeneration != queue.SnapshotGeneration || !fromOwner(instance, target.Evidence, now) {
		return empty, ErrStale
	}
	result := empty
	for _, row := range job.Queue.Records {
		if row.DownloadID != job.DownloadID {
			if row.Target == target.Ref || pathWithin(job.PathStyle, job.OutputPath, row.OutputPath, true) ||
				pathWithin(job.PathStyle, row.OutputPath, job.OutputPath, true) {
				return empty, ErrStale
			}
			continue
		}
		if result.ID != 0 || row.ID <= 0 || row.ID > math.MaxInt32 || row.Target != target.Ref ||
			row.Protocol != domain.ProtocolUsenet || !cleanText(row.ClientLabel, 256) || !samePath(job.PathStyle, row.OutputPath, job.OutputPath) ||
			row.Status != "completed" || row.TrackedState != "importBlocked" || len(row.Findings) != 1 || row.Findings[0] != observe.FindingSeriesIDOnly ||
			len(seriesWords(row.MediaTitle)) == 0 || !singleEpisode(row.Episode).valid() {
			return empty, ErrStale
		}
		result = row
	}
	if result.ID == 0 {
		return empty, ErrStale
	}
	return result, nil
}

func v5ReprocessRequest(row observe.QueueRecord, episode episodeMarker, in wireCandidate) (sonarrV5ReprocessRequest, error) {
	group, groupOK := nullableReleaseGroup(in.ReleaseGroup)
	flags, releaseType := nativeFlags(in.IndexerFlags), nativeReleaseType(in.ReleaseType)
	quality := in.Quality.Quality
	if !groupOK || flags == nil || *flags < 0 || *flags > 0x1ff ||
		releaseType == nil || (*releaseType != ReleaseUnknown && *releaseType != ReleaseSingle) ||
		!cleanText(quality.Name, 128) || !cleanText(quality.Source, 64) || quality.ID < 0 || quality.ID > math.MaxInt32 ||
		quality.Resolution == nil || *quality.Resolution < 0 || *quality.Resolution > math.MaxInt32 ||
		quality.Modifier != nil || !v5QualityRevision(in.Quality) || len(in.Languages) == 0 || len(in.Languages) > 32 {
		return sonarrV5ReprocessRequest{}, ErrSonarrV5Candidate
	}
	languages := make([]v5Language, 0, len(in.Languages))
	for _, l := range in.Languages {
		if l.ID < 0 || l.ID > math.MaxInt32 || !cleanText(l.Name, 128) || (l.ID == 0 && len(in.Languages) != 1) {
			return sonarrV5ReprocessRequest{}, ErrSonarrV5Candidate
		}
		languages = append(languages, v5Language(l))
	}
	return sonarrV5ReprocessRequest{
		Path: in.Path, DownloadID: row.DownloadID, SeriesID: row.Target.SeriesID,
		SeasonNumber: episode.season, EpisodeIDs: []int64{row.Target.ID},
		Quality: in.Quality, Languages: languages, ReleaseGroup: group, IndexerFlags: *flags, ReleaseType: *releaseType,
	}, nil
}

func v5QualityRevision(quality wireQuality) bool {
	r := quality.Revision
	return r != nil && r.Version != nil && *r.Version >= 0 && *r.Version <= math.MaxInt32 &&
		r.Real != nil && *r.Real >= 0 && *r.Real <= math.MaxInt32 && r.IsRepack != nil
}

func (c *Client) reprocessV5(ctx context.Context, input sonarrV5ReprocessRequest) ([]sonarrV5ReprocessResult, error) {
	endpoint := c.base
	endpoint.Path += "/api/v3/manualimport"
	payload, err := json.Marshal([]sonarrV5ReprocessRequest{input})
	if err != nil {
		return nil, ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, ErrInvalid
	}
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Api-Key", c.key)
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, observe.ErrUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusAccepted {
		return nil, observe.ErrResponse
	}
	kind, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(kind, "application/json") || res.ContentLength > maxBody {
		return nil, observe.ErrResponse
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil || len(data) > maxBody || !utf8.Valid(data) {
		return nil, observe.ErrResponse
	}
	var result []sonarrV5ReprocessResult
	if json.Unmarshal(data, &result) != nil || len(result) != 1 {
		return nil, observe.ErrResponse
	}
	return result, nil
}

func v5PreparedFile(results []sonarrV5ReprocessResult, before wireCandidate, row observe.QueueRecord, episode episodeMarker) (domain.PreparedImportFile, bool) {
	var empty domain.PreparedImportFile
	if len(results) != 1 {
		return empty, false
	}
	result := results[0]
	if result.Path != before.Path || result.DownloadID == nil || *result.DownloadID != row.DownloadID ||
		result.SeriesID == nil || *result.SeriesID != row.Target.SeriesID ||
		result.SeasonNumber == nil || *result.SeasonNumber != episode.season ||
		(result.Series != nil && result.Series.ID != row.Target.SeriesID) ||
		len(result.EpisodeIDs) != 0 || len(result.Episodes) != 1 ||
		result.Rejections == nil || len(result.Rejections) != 0 {
		return empty, false
	}
	selected := result.Episodes[0]
	if selected.ID != row.Target.ID || selected.SeriesID == nil || *selected.SeriesID != row.Target.SeriesID ||
		selected.SeasonNumber == nil || *selected.SeasonNumber != episode.season ||
		selected.EpisodeNumber == nil || *selected.EpisodeNumber != episode.episode ||
		selected.FileID == nil || *selected.FileID != 0 {
		return empty, false
	}
	mapped := result.wireCandidate
	// Reprocess echoes the selected path and metadata but does not return size.
	mapped.Size = before.Size
	metadata := Candidate{Size: before.Size}
	if !candidateMetadata(domain.ProductSonarr, &metadata, mapped) || !v5QualityRevision(mapped.Quality) ||
		*metadata.IndexerFlags < 0 || *metadata.IndexerFlags > 0x1ff || len(metadata.Languages) > 32 ||
		(*metadata.ReleaseType != ReleaseSingle && *metadata.ReleaseType != ReleaseUnknown) {
		return empty, false
	}
	quality := metadata.Quality
	file := domain.PreparedImportFile{
		Path: before.Path, FolderName: before.FolderName, Targets: []domain.TargetRef{row.Target},
		QualityID: quality.ID, QualityName: quality.Name, QualitySource: quality.Source,
		QualityResolution: new(quality.Resolution), Quality: domain.ImportQualityRevision{
			Version: int64(quality.Version), Real: int64(quality.Real), IsRepack: *quality.IsRepack, Observed: true,
		}, IndexerFlags: new(uint64(*metadata.IndexerFlags)), ReleaseType: string(*metadata.ReleaseType),
		ReleaseGroup: metadata.ReleaseGroup,
	}
	for _, language := range metadata.Languages {
		file.Languages = append(file.Languages, domain.ImportLanguage(language))
	}
	return file, true
}
