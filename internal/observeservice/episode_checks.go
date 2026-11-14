package observeservice

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/importflow"
	"github.com/rnodern/rescuarr/internal/observe"
)

const (
	episodeCheckBudget = 6 * time.Second
	episodeCheckBatch  = 6
	episodeCheckMaxAge = 10 * time.Minute
)

func (s *Service) reviewEpisodeNames(ctx context.Context, cfg observe.Config, arr *observe.Arr, snapshot observe.QueueSnapshot, status observe.SystemStatus, generation domain.Revision) map[int64]domain.EpisodeNameReview {
	style := importflow.PathStyle(status.PathOS)
	if !importflow.SupportsEpisodeNameReview(cfg.Instance.Product, status.Version) ||
		(style != importflow.UnixPath && style != importflow.WindowsPath) {
		return nil
	}
	// A download must identify one queue row across this complete scan.
	counts := make(map[string]int, len(snapshot.Records))
	for _, row := range snapshot.Records {
		counts[row.DownloadID]++
	}
	var rows []observe.QueueRecord
	for _, row := range snapshot.Records {
		if row.DownloadID != "" && counts[row.DownloadID] == 1 && row.Target.Valid() &&
			row.ClientLabel != "" && row.Protocol != domain.ProtocolUnknown &&
			slices.Contains(row.Findings, observe.FindingSeriesIDOnly) &&
			!slices.Contains(row.Findings, observe.FindingNoEligibleFiles) {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	s.episodeScans.Add(1)
	defer s.episodeScans.Add(-1)
	ctx, cancel := context.WithTimeout(ctx, episodeCheckBudget)
	defer cancel()
	roots, err := arr.RootPaths(ctx)
	if err != nil {
		return nil
	}
	client, err := importflow.New(cfg, s.transport)
	if err != nil {
		return nil
	}
	slices.SortFunc(rows, func(a, b observe.QueueRecord) int { return cmp.Compare(a.ID, b.ID) })
	first := int(((uint64(generation) - 1) * episodeCheckBatch) % uint64(len(rows)))
	checks := make(map[int64]domain.EpisodeNameReview)
	for index := range min(len(rows), episodeCheckBatch) {
		if ctx.Err() != nil {
			break
		}
		row := rows[(first+index)%len(rows)]
		result, err := client.ReviewEpisodeNames(ctx, row, style, roots)
		switch {
		case err == nil:
			checks[row.ID] = result
		case errors.Is(err, observe.ErrPartial):
			checks[row.ID] = domain.EpisodeNameReview{Reason: "Sonarr returned too many files to identify one episode."}
		case errors.Is(err, observe.ErrResponse), errors.Is(err, observe.ErrNotFound):
			checks[row.ID] = domain.EpisodeNameReview{Reason: "Rescuarr could not read the download’s file list from Sonarr."}
		}
	}
	return checks
}

func episodeCheckInput(revision domain.Revision, row observe.QueueRecord) string {
	// Include private inputs in the hash only. The saved review contains basenames.
	fingerprint, _ := hashJSON(struct {
		Revision                              domain.Revision
		Record                                observe.QueueRecord
		Path, Series, Episode, Title, Release string
	}{revision, row, row.OutputPath, row.MediaTitle, row.Episode, row.EpisodeTitle, row.Release})
	return fingerprint
}
