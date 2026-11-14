package domain

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PreparedAction binds private selectors to one locally reviewed operation and
// to the upstream contract revalidated immediately before dispatch.
type PreparedAction struct {
	Product            Product
	Version            string
	AdapterRevision    string
	ConnectionRevision Revision
	OperationKey       OperationKey
	Import             *PreparedImport
	Reject             *PreparedReject
	Search             *PreparedSearch
}

// PreparedImport describes the exact native Copy candidates. The sender must
// still establish physical path isolation and a qualified preservation profile.
type PreparedImport struct {
	DownloadID string
	Files      []PreparedImportFile
}

type PreparedImportFile struct {
	Path         string
	FolderName   string
	Targets      []TargetRef
	QualityID    int64
	Quality      ImportQualityRevision
	Languages    []int64
	ReleaseGroup string
	IndexerFlags uint64
	ReleaseType  string
}

type ImportQualityRevision struct {
	Version  int64
	Real     int64
	IsRepack bool
}

// PreparedReject selects one tracked queue row and verifies the original grab.
// The only orchestrated route blocklists, skips redownload and keeps the
// category. RemoveFromClient selects the separately qualified destructive path.
type PreparedReject struct {
	QueueID          int64
	DownloadID       string
	GrabHistoryID    int64
	RemoveFromClient bool
}

// PreparedSearch references one independently verified rejection. The bound
// operation supplies the exact episode IDs or singleton movie ID to search.
type PreparedSearch struct {
	ParentReject AttemptID
	Evidence     []EvidenceID
}

// Binds validates selector shape and exact operation scope. Qualification and
// freshness are checked by the caller against authoritative records.
func (p PreparedAction) Binds(op Operation) bool {
	if !selectorText(p.Version, 128) || !selectorText(p.AdapterRevision, 128) ||
		p.ConnectionRevision == 0 || p.OperationKey == "" || p.OperationKey != op.Key ||
		!op.Job.Instance.Valid() || op.Job.ID == "" || op.Job.Generation == 0 ||
		!preparedTargets(p.Product, op.Job.Instance, op.Targets) {
		return false
	}
	switch op.Action {
	case ActionImport:
		return p.Reject == nil && p.Search == nil && p.Import.binds(op) && preparedTargetEffects(op, EffectLibraryImport, EffectSourceRetained)
	case ActionReject:
		return p.Import == nil && p.Search == nil && p.Reject.binds(op)
	case ActionSearch:
		return p.Import == nil && p.Reject == nil && p.Search.valid() && preparedTargetEffects(op, EffectSearchDispatch, "")
	default:
		return false
	}
}

func preparedTargets(product Product, instance InstanceRef, targets []TargetRef) bool {
	if len(targets) == 0 || len(targets) > 128 || (product == ProductRadarr && len(targets) != 1) {
		return false
	}
	seen := make(map[int64]bool, len(targets))
	for _, target := range targets {
		switch product {
		case ProductSonarr:
			if target.Kind != TargetEpisode || target.SeriesID != targets[0].SeriesID {
				return false
			}
		case ProductRadarr:
			if target.Kind != TargetMovie {
				return false
			}
		default:
			return false
		}
		if target.Instance != instance || !target.Valid() || seen[target.ID] {
			return false
		}
		seen[target.ID] = true
	}
	return true
}

func (p *PreparedImport) binds(op Operation) bool {
	if p == nil || !selectorText(p.DownloadID, 256) || len(p.Files) == 0 || len(p.Files) > len(op.Targets) {
		return false
	}
	seen := make(map[TargetRef]bool, len(op.Targets))
	paths := make(map[string]bool, len(p.Files))
	for _, file := range p.Files {
		if !selectorText(file.Path, 4096) || !selectorText(file.FolderName, 1024) || paths[file.Path] ||
			!file.validMetadata() || len(file.Targets) == 0 || len(file.Targets) > len(op.Targets) {
			return false
		}
		paths[file.Path] = true
		for _, target := range file.Targets {
			if seen[target] || !slices.Contains(op.Targets, target) {
				return false
			}
			seen[target] = true
		}
	}
	return len(seen) == len(op.Targets)
}

func (file PreparedImportFile) validMetadata() bool {
	if file.QualityID <= 0 || file.Quality.Version < 0 || file.Quality.Real < 0 ||
		len(file.Languages) == 0 || len(file.Languages) > 32 ||
		(file.ReleaseGroup != "" && !selectorText(file.ReleaseGroup, 256)) ||
		(file.ReleaseType != "" && !selectorText(file.ReleaseType, 64)) {
		return false
	}
	seen := make(map[int64]bool, len(file.Languages))
	for _, language := range file.Languages {
		if language <= 0 || seen[language] {
			return false
		}
		seen[language] = true
	}
	return true
}

func (p *PreparedReject) binds(op Operation) bool {
	if p == nil || !selectorText(p.DownloadID, 256) || p.QueueID <= 0 || p.GrabHistoryID <= 0 {
		return false
	}
	kinds := make(map[EffectKind]bool)
	for _, effect := range op.Effects {
		if effect.Target != (TargetRef{}) || kinds[effect.Kind] {
			return false
		}
		switch effect.Kind {
		case EffectQueueRemoval, EffectBlocklist, EffectSourceRetained, EffectClientRemoval, EffectDataRemoval:
			kinds[effect.Kind] = true
		default:
			return false
		}
	}
	return kinds[EffectQueueRemoval] && kinds[EffectBlocklist] &&
		kinds[EffectSourceRetained] != p.RemoveFromClient &&
		kinds[EffectClientRemoval] == p.RemoveFromClient && kinds[EffectDataRemoval] == p.RemoveFromClient
}

func preparedTargetEffects(op Operation, targetKind, jobKind EffectKind) bool {
	expected := len(op.Targets)
	if jobKind != "" {
		expected++
	}
	if len(op.Effects) != expected {
		return false
	}
	seen := make(map[Effect]bool, expected)
	for _, effect := range op.Effects {
		if seen[effect] {
			return false
		}
		seen[effect] = true
		if effect.Kind == targetKind && slices.Contains(op.Targets, effect.Target) {
			continue
		}
		if jobKind == "" || effect != (Effect{Kind: jobKind}) {
			return false
		}
	}
	return true
}

func (p *PreparedSearch) valid() bool {
	if p == nil || !selectorText(string(p.ParentReject), 128) || len(p.Evidence) == 0 || len(p.Evidence) > 64 {
		return false
	}
	seen := make(map[EvidenceID]bool, len(p.Evidence))
	for _, evidence := range p.Evidence {
		if !selectorText(string(evidence), 128) || seen[evidence] {
			return false
		}
		seen[evidence] = true
	}
	return true
}

func selectorText(value string, limit int) bool {
	return len(value) > 0 && len(value) <= limit && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) == -1
}
