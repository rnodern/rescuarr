package observe

import (
	"context"
	"net/url"
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
		ID         int64     `json:"id"`
		SeriesID   int64     `json:"seriesId"`
		EpisodeID  int64     `json:"episodeId"`
		MovieID    int64     `json:"movieId"`
		DownloadID string    `json:"downloadId"`
		Event      string    `json:"eventType"`
		At         time.Time `json:"date"`
		Data       struct {
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
		out.Records = append(out.Records, HistoryRecord{
			ID: row.ID, Target: a.target(row.SeriesID, row.EpisodeID, row.MovieID), DownloadID: downloadID,
			Event: state(row.Event, "grabbed", "downloadFolderImported", "downloadFailed", "downloadIgnored", "downloadImported", "episodeFileDeleted", "movieFileDeleted"), At: row.At,
			IndexerLabel: boundedText(row.Data.Indexer, 128), ClientLabel: boundedText(row.Data.Client, 128),
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
