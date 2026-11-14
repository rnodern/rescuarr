package observe

import (
	"cmp"
	"context"
	"encoding/binary"
	"encoding/hex"
	"math"
	"slices"

	"github.com/rnodern/rescuarr/internal/domain"
)

const maxProfileItems = 256

// QualityProfile preserves Arr's native ordered qualities, groups and scores.
// Revision is the local, stable identifier for the projected profile digest.
type QualityProfile struct {
	ID                    int64
	Digest                string
	Revision              domain.Revision
	UpgradeAllowed        bool
	Cutoff                int32
	MinFormatScore        int32
	CutoffFormatScore     int32
	MinUpgradeFormatScore int32
	LanguageID            *int32
	Items                 []ProfileQuality
	Formats               []ProfileFormat
}

type ProfileQuality struct {
	GroupID   int32
	QualityID *int32
	Allowed   bool
	Items     []ProfileQuality
}

type ProfileFormat struct {
	ID    int32
	Score int32
}

type resourceIDWire struct {
	ID *int32 `json:"id"`
}

type profileItemWire struct {
	ID      int32             `json:"id"`
	Quality *resourceIDWire   `json:"quality"`
	Items   []profileItemWire `json:"items"`
	Allowed *bool             `json:"allowed"`
}

func (a *Arr) qualityProfile(ctx context.Context, id int64) (QualityProfile, error) {
	var wire struct {
		ID                    int64             `json:"id"`
		UpgradeAllowed        *bool             `json:"upgradeAllowed"`
		Cutoff                *int32            `json:"cutoff"`
		MinFormatScore        *int32            `json:"minFormatScore"`
		CutoffFormatScore     *int32            `json:"cutoffFormatScore"`
		MinUpgradeFormatScore *int32            `json:"minUpgradeFormatScore"`
		Language              *resourceIDWire   `json:"language"`
		Items                 []profileItemWire `json:"items"`
		Formats               []struct {
			ID    int32  `json:"format"`
			Score *int32 `json:"score"`
		} `json:"formatItems"`
	}
	if err := a.getTargetResource(ctx, "qualityprofile", id, &wire); err != nil {
		return QualityProfile{}, err
	}
	if wire.ID != id || wire.UpgradeAllowed == nil || wire.Cutoff == nil || wire.MinFormatScore == nil ||
		wire.CutoffFormatScore == nil || wire.MinUpgradeFormatScore == nil || wire.Formats == nil ||
		len(wire.Formats) > maxProfileItems || len(wire.Items) == 0 || len(wire.Items) > maxProfileItems {
		return QualityProfile{}, ErrResponse
	}
	out := QualityProfile{
		ID: id, UpgradeAllowed: *wire.UpgradeAllowed, Cutoff: *wire.Cutoff, MinFormatScore: *wire.MinFormatScore,
		CutoffFormatScore: *wire.CutoffFormatScore, MinUpgradeFormatScore: *wire.MinUpgradeFormatScore,
		Items: make([]ProfileQuality, 0, len(wire.Items)), Formats: make([]ProfileFormat, 0, len(wire.Formats)),
	}
	if a.reader.product == domain.ProductRadarr {
		// Native IDs include Any (-1), Original (-2) and Unknown (0).
		if wire.Language == nil || wire.Language.ID == nil || *wire.Language.ID < -2 {
			return QualityProfile{}, ErrResponse
		}
		out.LanguageID = wire.Language.ID
	}
	seen := make(map[int32]bool)
	for _, item := range wire.Items {
		value, ok := profileQuality(item, seen, true)
		if !ok {
			return QualityProfile{}, ErrResponse
		}
		out.Items = append(out.Items, value)
	}
	if !seen[out.Cutoff] {
		return QualityProfile{}, ErrResponse
	}
	clear(seen)
	for _, format := range wire.Formats {
		if format.ID <= 0 || seen[format.ID] || format.Score == nil {
			return QualityProfile{}, ErrResponse
		}
		seen[format.ID] = true
		out.Formats = append(out.Formats, ProfileFormat{ID: format.ID, Score: *format.Score})
	}
	slices.SortFunc(out.Formats, func(x, y ProfileFormat) int { return cmp.Compare(x.ID, y.ID) })
	digest := digestObservationContent(out)
	out.Digest = hex.EncodeToString(digest[:])
	out.Revision = domain.Revision(max(1, binary.BigEndian.Uint64(digest[:8])&math.MaxInt64))
	return out, nil
}

func profileQuality(item profileItemWire, seen map[int32]bool, groupsAllowed bool) (ProfileQuality, bool) {
	if item.Allowed == nil || len(seen) == maxProfileItems {
		return ProfileQuality{}, false
	}
	out := ProfileQuality{Allowed: *item.Allowed}
	if item.Quality != nil {
		id := item.Quality.ID
		if id == nil || *id < 0 || item.ID != 0 || len(item.Items) != 0 || seen[*id] {
			return ProfileQuality{}, false
		}
		seen[*id] = true
		out.QualityID = id
		return out, true
	}
	if !groupsAllowed || item.ID <= 0 || len(item.Items) == 0 || len(item.Items) >= maxProfileItems || seen[item.ID] {
		return ProfileQuality{}, false
	}
	seen[item.ID] = true
	out.GroupID, out.Items = item.ID, make([]ProfileQuality, 0, len(item.Items))
	for _, child := range item.Items {
		value, ok := profileQuality(child, seen, false)
		if !ok {
			return ProfileQuality{}, false
		}
		out.Items = append(out.Items, value)
	}
	return out, true
}
