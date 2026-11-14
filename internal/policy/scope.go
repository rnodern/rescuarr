package policy

import (
	"cmp"
	"slices"

	"github.com/rnodern/rescuarr/internal/domain"
)

func validScope(scope domain.Scope) bool {
	switch scope.Kind {
	case domain.ScopeStore:
		return scope.Instance == (domain.InstanceRef{})
	case domain.ScopeInstance:
		return scope.Instance.Valid() && scope.Key == ""
	case domain.ScopeRoot, domain.ScopeClient, domain.ScopeVolume:
		return scope.Instance.Valid() && scope.Key != ""
	default:
		return false
	}
}

func (a *assessment) collectScopes() {
	i := a.input
	scopes := []domain.Scope{{Kind: domain.ScopeInstance, Instance: i.Instance.Ref}, i.Case.Job.Client}
	for _, target := range i.Case.Targets {
		scopes = append(scopes, target.Root)
	}
	scopes = append(scopes, i.RelatedScopes...)
	for _, scope := range scopes {
		if validScope(scope) && scope.Instance == i.Instance.Ref && !slices.Contains(a.result.Scopes, scope) {
			a.result.Scopes = append(a.result.Scopes, scope)
		}
	}
	slices.SortFunc(a.result.Scopes, func(x, y domain.Scope) int {
		return cmp.Or(cmp.Compare(x.Kind, y.Kind), cmp.Compare(x.Instance.ID, y.Instance.ID),
			cmp.Compare(x.Instance.Epoch, y.Instance.Epoch), cmp.Compare(x.Key, y.Key))
	})
}

func (a *assessment) affects(scope domain.Scope) bool {
	return validScope(scope) && (scope.Kind == domain.ScopeStore || slices.Contains(a.result.Scopes, scope))
}

func evidenceIDs(observations []domain.Observation) []domain.EvidenceID {
	var ids []domain.EvidenceID
	for _, observation := range observations {
		if observation.ID != "" {
			ids = append(ids, observation.ID)
		}
	}
	return ids
}

func (a *assessment) find(code domain.ReasonCode, observations ...domain.Observation) {
	a.result.Findings = addFinding(a.result.Findings, code, evidenceIDs(observations))
}

func (a *assessment) hold(code domain.ReasonCode, observations ...domain.Observation) {
	a.addHold(code, evidenceIDs(observations))
}

func (a *assessment) addHold(code domain.ReasonCode, ids []domain.EvidenceID) {
	a.result.Findings = addFinding(a.result.Findings, code, ids)
	a.result.Holds = addFinding(a.result.Holds, code, ids)
}

func addFinding(findings []Finding, code domain.ReasonCode, ids []domain.EvidenceID) []Finding {
	for p := range findings {
		if findings[p].Code == code {
			findings[p].Evidence = append(findings[p].Evidence, ids...)
			return findings
		}
	}
	return append(findings, Finding{Code: code, Evidence: slices.Clone(ids)})
}

func (a *assessment) containsHold(codes ...domain.ReasonCode) bool {
	return slices.ContainsFunc(a.result.Holds, func(f Finding) bool { return slices.Contains(codes, f.Code) })
}

func (a *assessment) normalise() {
	if len(a.result.Findings) == 0 {
		a.find(StalledUnknown)
	}
	for _, findings := range [][]Finding{a.result.Findings, a.result.Holds} {
		for p := range findings {
			slices.Sort(findings[p].Evidence)
			findings[p].Evidence = slices.Compact(findings[p].Evidence)
		}
		slices.SortFunc(findings, func(x, y Finding) int {
			return cmp.Or(cmp.Compare(priority(x.Code), priority(y.Code)), cmp.Compare(x.Code, y.Code))
		})
	}
	a.result.Reason = a.result.Findings[0].Code
}

func priority(code domain.ReasonCode) int {
	switch code {
	case UpstreamUnavailable, AuthenticationFailed, ContractChanged:
		return 10
	case OwnershipConflict, TargetAmbiguous, UnknownProvenance:
		return 20
	case RootUnavailable, PathMappingMissing, PermissionDenied, LowCapacity:
		return 30
	case DownloadActive, PostProcessing, ArrImporting, CompletionGrace, CompetingDownload:
		return 40
	case TargetWithdrawn, RequestChanged:
		return 50
	case AlreadySatisfied:
		return 60
	case SafeImport:
		return 70
	case PartialPack:
		return 80
	case QualityRejected:
		return 90
	case ArchiveUnresolved, PasswordRequired, SampleOnly, FileUnreadable, DownloadFailed:
		return 100
	case StalledUnknown, UnsupportedPayload:
		return 110
	default:
		return 120
	}
}
