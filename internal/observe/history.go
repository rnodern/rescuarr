package observe

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

type HistoryRecord struct {
	ID           int64
	Target       domain.TargetRef
	DownloadID   string
	Event        string
	At           time.Time
	IndexerLabel string
	ClientLabel  string
	FileID       int64
	SourceTitle  string                  `json:"-"`
	DroppedPath  string                  `json:"-"`
	ImportedPath string                  `json:"-"`
	Languages    []domain.ImportLanguage `json:"-"`
}

type HistorySnapshot struct {
	Evidence domain.Observation
	Records  []HistoryRecord
}

// History requires the exact queue-observed download token. Provider URLs and
// all other flexible response data are discarded by the typed JSON decoder.
func (a *Arr) History(ctx context.Context, downloadID string, generation domain.Revision, observedAt time.Time) (HistorySnapshot, error) {
	if downloadID == "" || exactToken(downloadID, 256) != downloadID || generation == 0 || observedAt.IsZero() {
		return HistorySnapshot{}, ErrConfiguration
	}
	type record struct {
		ID          int64           `json:"id"`
		SeriesID    int64           `json:"seriesId"`
		EpisodeID   int64           `json:"episodeId"`
		MovieID     int64           `json:"movieId"`
		DownloadID  string          `json:"downloadId"`
		Event       string          `json:"eventType"`
		At          time.Time       `json:"date"`
		SourceTitle json.RawMessage `json:"sourceTitle"`
		Languages   json.RawMessage `json:"languages"`
		Data        struct {
			Indexer string `json:"indexer"`
			Client  string `json:"downloadClientName"`
		} `json:"data"`
	}
	query := url.Values{"downloadId": {downloadID}, "sortKey": {"date"}, "sortDirection": {"descending"}}
	rows, err := scanPages[record](ctx, a.reader, "/api/v3/history", query, historyPageSize, func(r record) int64 { return r.ID })
	out := HistorySnapshot{Records: make([]HistoryRecord, 0, len(rows))}
	for _, row := range rows {
		if row.DownloadID != downloadID {
			err = ErrPartial
			continue
		}
		var title string
		_ = json.Unmarshal(row.SourceTitle, &title)
		var languages []fileLanguageWire
		if json.Unmarshal(row.Languages, &languages) != nil {
			languages = nil
		}
		out.Records = append(out.Records, HistoryRecord{
			ID: row.ID, Target: a.target(row.SeriesID, row.EpisodeID, row.MovieID), DownloadID: downloadID,
			Event: state(row.Event, "grabbed", "downloadFolderImported", "downloadFailed", "downloadIgnored", "downloadImported", "episodeFileDeleted", "movieFileDeleted"), At: row.At,
			IndexerLabel: boundedText(row.Data.Indexer, 128), ClientLabel: boundedText(row.Data.Client, 128),
			SourceTitle: exactToken(title, 512), Languages: nativeFileLanguages(languages),
		})
	}
	out.Evidence = a.observation("history", downloadID, generation, observedAt, out.Records, err)
	return out, err
}

type HealthFinding struct {
	Source   string
	Severity string
}

// Health retains source and severity. No raw message, documentation link or path
// leaves the adapter. An empty result is meaningful only with a nil error.
func (a *Arr) Health(ctx context.Context) ([]HealthFinding, error) {
	var rows []struct {
		Source string `json:"source"`
		Type   string `json:"type"`
	}
	if err := a.reader.get(ctx, "/api/v3/health", nil, true, &rows); err != nil {
		return nil, err
	}
	if len(rows) > queuePageSize {
		return nil, ErrPartial
	}
	out := make([]HealthFinding, 0, len(rows))
	for _, row := range rows {
		out = append(out, HealthFinding{Source: boundedText(row.Source, 128), Severity: state(row.Type, "ok", "notice", "warning", "error")})
	}
	return out, nil
}

// EpisodeImportHistory returns only native import events for the exact Sonarr
// episode. The download IDs are private inputs for correlation by the caller.
func (a *Arr) EpisodeImportHistory(ctx context.Context, target domain.TargetRef) ([]HistoryRecord, error) {
	if a.reader.product != domain.ProductSonarr || target.Kind != domain.TargetEpisode ||
		!target.Valid() || target.Instance != a.reader.instance {
		return nil, ErrConfiguration
	}
	type row struct {
		ID         int64     `json:"id"`
		SeriesID   int64     `json:"seriesId"`
		EpisodeID  int64     `json:"episodeId"`
		DownloadID string    `json:"downloadId"`
		Event      string    `json:"eventType"`
		Date       time.Time `json:"date"`
		Data       struct {
			FileID       string          `json:"fileId"`
			Client       string          `json:"downloadClientName"`
			DroppedPath  json.RawMessage `json:"droppedPath"`
			ImportedPath json.RawMessage `json:"importedPath"`
		} `json:"data"`
	}
	query := url.Values{"episodeId": {strconv.FormatInt(target.ID, 10)}, "seriesIds": {strconv.FormatInt(target.SeriesID, 10)},
		"eventType": {"3"}, "sortKey": {"date"}, "sortDirection": {"descending"}}
	rows, err := scanPages[row](ctx, a.reader, "/api/v3/history", query, historyPageSize, func(r row) int64 { return r.ID })
	if err != nil {
		return nil, err
	}
	var history []HistoryRecord
	for _, r := range rows {
		if r.Event != "downloadFolderImported" || r.SeriesID != target.SeriesID || r.EpisodeID != target.ID || r.Date.IsZero() {
			return nil, ErrPartial
		}
		fileID, err := strconv.ParseInt(r.Data.FileID, 10, 32)
		if err != nil || fileID <= 0 || strconv.FormatInt(fileID, 10) != r.Data.FileID {
			continue
		}
		var dropped, imported string
		_ = json.Unmarshal(r.Data.DroppedPath, &dropped)
		_ = json.Unmarshal(r.Data.ImportedPath, &imported)
		history = append(history, HistoryRecord{ID: r.ID, Target: target, Event: r.Event, At: r.Date, FileID: fileID,
			DownloadID: exactToken(r.DownloadID, 256), ClientLabel: boundedText(r.Data.Client, 128),
			DroppedPath: exactToken(dropped, 4096), ImportedPath: exactToken(imported, 4096)})
	}
	return history, nil
}

// EpisodeFileLink reads the currently linked file, separately from import history.
func (a *Arr) EpisodeFileLink(ctx context.Context, target domain.TargetRef) (int64, error) {
	if a.reader.product != domain.ProductSonarr || target.Kind != domain.TargetEpisode ||
		!target.Valid() || target.Instance != a.reader.instance {
		return 0, ErrConfiguration
	}
	var episode struct {
		ID       int64  `json:"id"`
		SeriesID int64  `json:"seriesId"`
		FileID   *int32 `json:"episodeFileId"`
		HasFile  *bool  `json:"hasFile"`
	}
	if err := a.getTargetResource(ctx, "episode", target.ID, &episode); err != nil {
		return 0, err
	}
	if episode.ID != target.ID || episode.SeriesID != target.SeriesID ||
		!validFileLink(episode.FileID, episode.HasFile, nil) {
		return 0, ErrResponse
	}
	return int64(*episode.FileID), nil
}
