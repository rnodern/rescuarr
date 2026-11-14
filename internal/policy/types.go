// Package policy evaluates frozen recovery evidence without contacting upstream services.
package policy

import (
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

// Path distinguishes workflows that share one domain action but have different effects.
type Path string

const (
	ImportOriginal Path = "import_original"
	RejectRetain   Path = "reject_retain"
	RejectRemove   Path = "reject_remove"
	SearchScoped   Path = "search_scoped"
)

type Authority string

const (
	AuthorityNone       Authority = "none"
	AuthorityPreview    Authority = "preview"
	AuthorityAssist     Authority = "assist"
	AuthorityAutonomous Authority = "autonomous"
)

type Permission string

const (
	PermitReview    Permission = "review"
	PermitAutomatic Permission = "automatic"
	Deny            Permission = "deny"
)

type Health string

const (
	HealthAvailable      Health = "available"
	HealthUnavailable    Health = "unavailable"
	HealthAuthentication Health = "authentication_failed"
	HealthContract       Health = "contract_changed"
)

type Phase string

const (
	PhaseDownload Phase = "download_active"
	PhaseProcess  Phase = "post_processing"
	PhaseImport   Phase = "arr_import_in_progress"
	PhaseComplete Phase = "completed"
	PhaseFailed   Phase = "failed"
)

type Intent string

const (
	NeedFirstImport Intent = "first_import"
	NeedUpgrade     Intent = "upgrade_eligible"
	NeedSatisfied   Intent = "already_satisfied"
	NeedWithdrawn   Intent = "target_withdrawn"
)

// TargetEvidence records Arr's current intent and the profile revision it evaluated.
type TargetEvidence struct {
	Target          domain.Target
	Intent          domain.Fact[Intent]
	ProfileAtGrab   domain.Revision
	CompetingGrab   domain.Fact[bool]
	SearchAvailable domain.Fact[bool]
}

type Assessment string

const (
	ImportAll    Assessment = "all_eligible"
	ImportSubset Assessment = "subset_eligible"
	ImportNone   Assessment = "none_usable"
)

// Candidate identifies one exact accepted Arr candidate, including all targets
// that the file would affect. Key is a stable, opaque selector, never a title.
type Candidate struct {
	Key             string
	Targets         []domain.TargetRef
	Profiles        map[domain.TargetRef]domain.Revision
	Approved        domain.Fact[bool]
	PathContained   domain.Fact[bool]
	StableAndUsable domain.Fact[bool]
}

type ImportEvidence struct {
	Assessment domain.Fact[Assessment]
	Candidates []Candidate
}

// Failure needs independent, current exact-job sources for a terminal conclusion.
type Failure struct {
	Reason       domain.ReasonCode
	Primary      domain.Fact[bool]
	Corroborated domain.Fact[bool]
}

type SearchAuthority string

const (
	SearchByRescuarr SearchAuthority = "rescuarr"
	SearchByArr      SearchAuthority = "arr"
)

// Capability is a certificate for this installation epoch, adapter and protocol.
// CoversFullJobPreservation includes selected/unselected files, parent and client job.
type Capability struct {
	Path                      Path
	Instance                  domain.InstanceRef
	Protocol                  domain.Protocol
	Revision                  domain.Revision
	Certified                 bool
	Effects                   []domain.EffectKind
	CoversFullJobPreservation bool
	SearchAuthority           SearchAuthority
	DedicatedUsenetRemoval    bool
}

// PriorReject is a reconciled, attributable rejection of this exact job.
// OwnedSearch is recorded with the rejection so search cannot switch owners later.
type PriorReject struct {
	Job                domain.JobRef
	Path               Path
	CapabilityRevision domain.Revision
	OwnedSearch        SearchAuthority
	Targets            []domain.TargetRef
	Outcome            domain.Outcome
}

type Capacity struct {
	Scope           domain.Scope
	FreeBytes       domain.Fact[uint64]
	RequiredBytes   uint64
	ReservedBytes   uint64
	FloorBytes      uint64
	AllocationKnown bool
}

// Gate is the current ledger result for a particular path and overlap scope.
type Gate struct {
	BudgetKnown bool
	Remaining   int
	NotBefore   time.Time
}

type Policy struct {
	Revision        domain.Revision
	Global          domain.Mode
	Instance        domain.Mode
	GlobalActions   map[Path]Permission
	InstanceActions map[Path]Permission
	Paused          bool
	PausedScopes    []domain.Scope
	PausedActions   []Path
}

type Input struct {
	Now                time.Time
	Instance           domain.Instance
	Case               domain.Case
	Policy             Policy
	PatternRevision    domain.Revision
	JobPhase           domain.Fact[Phase]
	ServiceHealth      domain.Fact[Health]
	IdentityValid      domain.Fact[bool]
	OwnershipVerified  domain.Fact[bool]
	DedicatedUsenet    domain.Fact[bool]
	Seeding            domain.Fact[bool]
	FirstCompletedAt   time.Time
	PreviousCoherentAt time.Time
	Targets            []TargetEvidence
	RelatedScopes      []domain.Scope // independently verified roots, clients or volumes
	Import             ImportEvidence
	Failure            Failure
	Incidents          []domain.Incident
	DependencyFaults   []DependencyFault
	Capabilities       map[Path]Capability
	Capacities         map[Path][]Capacity
	Gates              map[Path]Gate
	Overlap            domain.Fact[bool]
	PriorReject        *PriorReject
}

type DependencyFault struct {
	Reason   domain.ReasonCode
	Evidence domain.Observation
}

type Finding struct {
	Code     domain.ReasonCode
	Evidence []domain.EvidenceID
}

type Result struct {
	State     domain.CaseState
	Reason    domain.ReasonCode
	Findings  []Finding
	Holds     []Finding
	Scopes    []domain.Scope
	NextCheck time.Time
	Path      Path
	Authority Authority
	Proposal  *domain.Proposal
}
