package observe

import (
	"context"
	"encoding/json"
	"math"
	"strings"

	"github.com/rnodern/rescuarr/internal/domain"
)

// OriginalAudioLanguage reads Arr's original-language metadata for the exact
// series or movie. An empty result means the field provided no usable evidence.
// Sonarr can fill the field with English when its metadata source is unclear.
func (a *Arr) OriginalAudioLanguage(ctx context.Context, ref domain.TargetRef) (domain.ImportLanguage, error) {
	if !ref.Valid() || ref.Instance != a.reader.instance || ref.ID > math.MaxInt32 || ref.SeriesID > math.MaxInt32 ||
		(a.reader.product == domain.ProductSonarr) != (ref.Kind == domain.TargetEpisode) {
		return domain.ImportLanguage{}, ErrConfiguration
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	status, err := a.Status(ctx)
	if err != nil {
		return domain.ImportLanguage{}, err
	}
	if a.upstreamVersion != "" && status.Version != a.upstreamVersion {
		return domain.ImportLanguage{}, ErrTargetVersion
	}
	resource, id := "movie", ref.ID
	if a.reader.product == domain.ProductSonarr {
		if status.Version != SonarrTargetVersion && status.Version != sonarrV5Needs {
			return domain.ImportLanguage{}, ErrTargetVersion
		}
		resource, id = "series", ref.SeriesID
	} else if status.Version != RadarrTargetVersion {
		return domain.ImportLanguage{}, ErrTargetVersion
	}
	var media struct {
		ID       int64           `json:"id"`
		Language json.RawMessage `json:"originalLanguage"`
	}
	if err := a.getTargetResource(ctx, resource, id, &media); err != nil {
		return domain.ImportLanguage{}, err
	}
	if media.ID != id {
		return domain.ImportLanguage{}, ErrResponse
	}
	var reported fileLanguageWire
	if json.Unmarshal(media.Language, &reported) != nil {
		return domain.ImportLanguage{}, nil
	}
	languages := nativeFileLanguages([]fileLanguageWire{reported})
	if len(languages) == 0 || strings.EqualFold(languages[0].Name, "unknown") {
		return domain.ImportLanguage{}, nil
	}
	return languages[0], nil
}
