// Package learning evaluates attributable, verified recovery patterns from local evidence.
package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

const FingerprintVersion = 1

var ErrInvalid = errors.New("invalid learning input")

// Epochs are deliberately separate from rule thresholds. A change in any
// material epoch puts old results outside the active pattern.
type Epochs struct {
	Installation domain.InstanceRef
	Adapter      domain.Epoch
	Config       domain.Epoch
	Policy       domain.Epoch
}

type Family string

const (
	OriginalImport Family = "original_import"
	Replacement    Family = "replacement"
	IncidentCause  Family = "incident_cause"
)

type Shape string

const (
	SingleFile Shape = "single_file"
	Pack       Shape = "pack"
	Dependency Shape = "dependency"
)

type ClientClass string

const (
	ClientSAB    ClientClass = "sabnzbd"
	ClientArr    ClientClass = "arr_managed"
	ClientUnused ClientClass = "not_applicable"
)

type Phase string

const (
	CompletedImport Phase = "completed_import"
	TerminalFailure Phase = "terminal_failure"
	DependencyFault Phase = "dependency_fault"
)

// Signature contains only structural facts. CausalScope.Key must be a local
// opaque scope identifier, such as a locally assigned UUID.
type Signature struct {
	Version        uint8
	Epochs         Epochs
	Product        domain.Product
	Client         ClientClass
	Protocol       domain.Protocol
	Phase          Phase
	Family         Family
	Reason         domain.ReasonCode
	Corroborating  []domain.ReasonCode
	CausalScope    domain.Scope
	Shape          Shape
	UniqueMapping  bool
	StableComplete bool
}

type Fingerprint struct {
	ID        domain.PatternID
	Signature Signature
}

func BuildFingerprint(signature Signature) (Fingerprint, error) {
	if signature.Version != FingerprintVersion || !validEpochs(signature.Epochs) ||
		(signature.Product != domain.ProductSonarr && signature.Product != domain.ProductRadarr) || !containsReason(signature.Reason) ||
		len(signature.Corroborating) > 16 || !validStructure(signature) || !validCausalScope(signature.CausalScope, signature.Epochs.Installation) {
		return Fingerprint{}, ErrInvalid
	}
	signature.Corroborating = slices.Clone(signature.Corroborating)
	for _, reason := range signature.Corroborating {
		if !containsReason(reason) {
			return Fingerprint{}, ErrInvalid
		}
	}
	slices.Sort(signature.Corroborating)
	signature.Corroborating = slices.Compact(signature.Corroborating)
	if len(signature.Corroborating) == 0 {
		signature.Corroborating = nil
	}
	encoded, err := json.Marshal(signature)
	if err != nil {
		return Fingerprint{}, ErrInvalid
	}
	hash := sha256.Sum256(encoded)
	return Fingerprint{ID: domain.PatternID(hex.EncodeToString(hash[:])), Signature: signature}, nil
}

func validEpochs(e Epochs) bool {
	return e.Installation.Valid() && safeID(string(e.Installation.ID)) && e.Adapter > 0 && e.Config > 0 && e.Policy > 0
}

func validStructure(s Signature) bool {
	if s.Family == IncidentCause {
		return s.Phase == DependencyFault && s.Shape == Dependency && s.CausalScope.Kind != "" &&
			s.Client == ClientUnused && s.Protocol == domain.ProtocolUnknown && !s.UniqueMapping && !s.StableComplete
	}
	if (s.Client != ClientSAB && s.Client != ClientArr) ||
		(s.Protocol != domain.ProtocolUsenet && s.Protocol != domain.ProtocolTorrent) ||
		(s.Client == ClientSAB && s.Protocol != domain.ProtocolUsenet) ||
		(s.Shape != SingleFile && (s.Shape != Pack || s.Product != domain.ProductSonarr)) || !s.UniqueMapping {
		return false
	}
	return (s.Family == OriginalImport && s.Phase == CompletedImport && s.StableComplete) ||
		(s.Family == Replacement && s.Phase == TerminalFailure)
}

func containsReason(code domain.ReasonCode) bool {
	switch code {
	case "upstream_unavailable", "authentication_failed", "contract_changed", "root_path_unavailable", "path_mapping_missing",
		"permission_denied", "capacity_low", "post_processing", "safe_import_candidate", "partially_recoverable_pack",
		"quality_or_format_rejected", "archive_unresolved", "password_required", "sample_only", "file_unreadable", "download_failed":
		return true
	default:
		return false
	}
}

func validCausalScope(scope domain.Scope, instance domain.InstanceRef) bool {
	if scope == (domain.Scope{}) {
		return true
	}
	if scope.Instance != instance {
		return false
	}
	if scope.Kind == domain.ScopeInstance {
		return scope.Key == ""
	}
	return (scope.Kind == domain.ScopeRoot || scope.Kind == domain.ScopeClient || scope.Kind == domain.ScopeVolume) && safeID(scope.Key)
}

func safeID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

type SampleState string

const (
	SampleSuccess       SampleState = "verified_success"
	SampleFailure       SampleState = "verified_failure"
	SampleNeutral       SampleState = "neutral"
	SampleContradiction SampleState = "contradiction"
)

// Proof is an exact fact derived from a bounded, trusted read or the durable
// ledger. The details of producing the fact belong to the respective adapter.
type Proof struct {
	Evidence domain.EvidenceID
	Observed time.Time
	Job      domain.JobRef
	Target   domain.TargetRef
	Scope    domain.Scope
}

type SafetyFinding string

const (
	UnexpectedEffect SafetyFinding = "unexpected_effect"
	WrongTarget      SafetyFinding = "wrong_target"
	NeededFileLost   SafetyFinding = "needed_file_lost"
	WorsenedQuality  SafetyFinding = "worsened_quality"
)

type Candidate struct {
	Sample              domain.PatternSample
	Plan                domain.Intent
	Attempt             domain.Attempt
	FollowupJob         domain.JobRef // independently recovered job for incidents or replacement job
	StartedAt           time.Time
	MediaAssociation    Proof
	ImportHistory       Proof
	NoReversalThrough   Proof
	ReplacementGrab     Proof
	HealthyDependency   Proof
	IndependentRecovery Proof
	Override            bool
	DispatchVerified    bool
	IdentityVerified    bool
	QualifiedPlan       bool
	Safety              SafetyFinding
	SafetyEvidence      Proof
}

type SampleResult struct {
	Sample domain.PatternSample
	State  SampleState
	Reason domain.ReasonCode
}
