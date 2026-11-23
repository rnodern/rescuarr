package observe

import (
	"context"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

// EpisodeNeed describes a Sonarr episode's current library and monitoring state.
// Callers must also check the queue for another download covering the episode.
type EpisodeNeed string

const (
	EpisodeMissing     EpisodeNeed = "missing"
	EpisodeInLibrary   EpisodeNeed = "in_library"
	EpisodeUnmonitored EpisodeNeed = "unmonitored"
	EpisodeUnaired     EpisodeNeed = "unaired"

	maxEpisodeNeeds = 128
)

type episodeNeedWire struct {
	ID        int64      `json:"id"`
	SeriesID  int64      `json:"seriesId"`
	Monitored *bool      `json:"monitored"`
	HasFile   *bool      `json:"hasFile"`
	FileID    *int32     `json:"episodeFileId"`
	AirDate   *time.Time `json:"airDateUtc"`
}

// SonarrEpisodeNeeds reads the exact requested episodes and their series. The
// result is complete or an error, so an omitted episode never implies absence.
func (a *Arr) SonarrEpisodeNeeds(ctx context.Context, seriesID int64, ids []int64, now time.Time) (map[int64]EpisodeNeed, error) {
	if a.reader.product != domain.ProductSonarr || seriesID <= 0 || seriesID > math.MaxInt32 ||
		len(ids) == 0 || len(ids) > maxEpisodeNeeds || now.IsZero() {
		return nil, ErrConfiguration
	}
	requested := make(map[int64]struct{}, len(ids))
	query := make(url.Values)
	for _, id := range ids {
		if _, exists := requested[id]; exists || id <= 0 || id > math.MaxInt32 {
			return nil, ErrConfiguration
		}
		requested[id] = struct{}{}
		query.Add("episodeIds", strconv.FormatInt(id, 10))
	}
	status, err := a.Status(ctx)
	if err != nil {
		return nil, err
	}
	if !sonarrContract(status.Version) || (a.upstreamVersion != "" && a.upstreamVersion != status.Version) {
		return nil, ErrTargetVersion
	}
	var series struct {
		ID        int64 `json:"id"`
		Monitored *bool `json:"monitored"`
		Runtime   *int  `json:"runtime"`
	}
	if err := a.getTargetResource(ctx, "series", seriesID, &series); err != nil {
		return nil, err
	}
	if series.ID != seriesID || series.Monitored == nil || series.Runtime == nil ||
		*series.Runtime < 0 || int64(*series.Runtime) > math.MaxInt64/int64(time.Minute) {
		return nil, ErrResponse
	}
	runtime := time.Duration(*series.Runtime) * time.Minute
	var episodes []episodeNeedWire
	if err := a.reader.get(ctx, "/api/v3/episode", query, true, &episodes); err != nil {
		return nil, err
	}
	if len(episodes) != len(requested) {
		return nil, ErrPartial
	}
	needs := make(map[int64]EpisodeNeed, len(requested))
	for _, episode := range episodes {
		if _, found := requested[episode.ID]; !found || episode.SeriesID != seriesID ||
			episode.Monitored == nil || episode.HasFile == nil || !validFileLink(episode.FileID, episode.HasFile, nil) {
			return nil, ErrResponse
		}
		delete(requested, episode.ID)
		switch {
		case *episode.HasFile:
			needs[episode.ID] = EpisodeInLibrary
		case !*series.Monitored || !*episode.Monitored:
			needs[episode.ID] = EpisodeUnmonitored
		// Sonarr's Wanted list compares the end of the episode's first airing.
		case episode.AirDate == nil || episode.AirDate.IsZero() || episode.AirDate.Add(runtime).After(now):
			needs[episode.ID] = EpisodeUnaired
		default:
			needs[episode.ID] = EpisodeMissing
		}
	}
	return needs, nil
}

// sonarrContract reports whether a Sonarr build belongs to a qualified v4 or v5 contract.
func sonarrContract(version string) bool {
	switch arrwire.ContractFor(domain.ProductSonarr, version) {
	case arrwire.ContractSonarrV4, arrwire.ContractSonarrV5:
		return true
	default:
		return false
	}
}

// sonarrV5 reports whether a Sonarr build belongs to the v5 contract.
func sonarrV5(version string) bool {
	return arrwire.ContractFor(domain.ProductSonarr, version) == arrwire.ContractSonarrV5
}
