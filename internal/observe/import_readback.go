package observe

import (
	"context"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

// SonarrV5ImportProof correlates a fresh linked file with one matching native
// import event. The expected source and library paths must agree independently.
func (a *Arr) SonarrV5ImportProof(ctx context.Context, target domain.TargetRef, download, source string,
	size int64, sentAt, now time.Time) (HistoryRecord, error) {
	if a.upstreamVersion != sonarrV5Needs || download == "" || source == "" || size <= 0 ||
		sentAt.IsZero() || now.Before(sentAt) {
		return HistoryRecord{}, ErrConfiguration
	}
	fileID, err := a.EpisodeFileLink(ctx, target)
	if err != nil || fileID == 0 {
		return HistoryRecord{}, err
	}
	history, err := a.EpisodeImportHistory(ctx, target)
	if err != nil {
		return HistoryRecord{}, err
	}
	var matched HistoryRecord
	for _, record := range history {
		if record.FileID != fileID {
			continue
		}
		if matched.ID != 0 || record.DownloadID != download || record.DroppedPath != source ||
			record.ImportedPath == "" || record.ImportedPath == source || record.At.Before(sentAt.Truncate(time.Second)) || record.At.After(now) {
			return HistoryRecord{}, ErrPartial
		}
		matched = record
	}
	if matched.ID == 0 {
		return HistoryRecord{}, nil
	}
	file, err := a.libraryFile(ctx, target, fileID, nil)
	if err != nil {
		return HistoryRecord{}, err
	}
	if file.Path != matched.ImportedPath || file.SizeBytes == nil || *file.SizeBytes != size || file.DateAdded == nil ||
		file.DateAdded.Before(sentAt.Truncate(time.Second)) || file.DateAdded.After(now) {
		return HistoryRecord{}, ErrPartial
	}
	current, err := a.EpisodeFileLink(ctx, target)
	if err == nil && current != fileID {
		err = ErrPartial
	}
	if err != nil {
		return HistoryRecord{}, err
	}
	return matched, nil
}
