package domain

import (
	"context"
	"time"
)

type (
	ProposalID   string
	IntentID     string
	AttemptID    string
	OperationKey string
	ActorID      string
)

type Mode string

const (
	ModeObserve    Mode = "observe"
	ModeAssist     Mode = "assist"
	ModeAutonomous Mode = "autonomous"
)

type Action string

const (
	ActionImport Action = "import"
	ActionReject Action = "reject"
	ActionSearch Action = "search"
)

type Guard struct {
	Reason   ReasonCode
	Passed   bool
	Evidence []EvidenceID
}

type Proposal struct {
	ID                 ProposalID
	CaseID             CaseID
	CaseRevision       Revision
	PolicyRevision     Revision
	CapabilityRevision Revision
	PatternRevision    Revision
	DecisionHash       string
	Operation          Operation
	Guards             []Guard
	ExpiresAt          time.Time
}

type IntentState string

const (
	IntentPlanned              IntentState = "planned"
	IntentAuthorized           IntentState = "authorized"
	IntentReserved             IntentState = "reserved"
	IntentSending              IntentState = "sending"
	IntentAcknowledged         IntentState = "acknowledged"
	IntentRejectedBeforeEffect IntentState = "rejected_before_effect"
	IntentUncertain            IntentState = "uncertain"
	IntentReconciling          IntentState = "reconciling"
	IntentCompleted            IntentState = "completed"
	IntentCancelled            IntentState = "cancelled"
)

// Operation binds a local job generation, current upstream handles and planned effects.
type Operation struct {
	Key     OperationKey
	Job     JobRef
	Targets []TargetRef
	Action  Action
	Effects []Effect
}

type Intent struct {
	ID         IntentID
	Proposal   Proposal
	Actor      ActorID
	RequestKey string
	State      IntentState
	CreatedAt  time.Time
}

type DispatchState string

const (
	DispatchAcknowledged         DispatchState = "acknowledged"
	DispatchRejectedBeforeEffect DispatchState = "rejected_before_effect"
	DispatchUncertain            DispatchState = "uncertain"
)

type Acknowledgement struct {
	State             DispatchState
	UpstreamReference string
	Reason            ReasonCode
}

type Attempt struct {
	ID              AttemptID
	IntentID        IntentID
	Operation       Operation
	Preflight       []EvidenceID
	SentAt          time.Time
	Acknowledgement Acknowledgement
}

type EffectKind string

const (
	EffectLibraryImport   EffectKind = "library_import"
	EffectQueueRemoval    EffectKind = "queue_removal"
	EffectClientRemoval   EffectKind = "client_removal"
	EffectDataRemoval     EffectKind = "data_removal"
	EffectSourceRetained  EffectKind = "source_retained"
	EffectBlocklist       EffectKind = "blocklist"
	EffectSearchDispatch  EffectKind = "search_dispatch"
	EffectReplacementGrab EffectKind = "replacement_grab"
)

type Effect struct {
	Kind   EffectKind
	Target TargetRef
}

type Verification string

const (
	VerificationEffect     Verification = "verified_effect"
	VerificationPartial    Verification = "verified_partial"
	VerificationNoEffect   Verification = "verified_no_effect"
	VerificationExternal   Verification = "externally_resolved"
	VerificationUnresolved Verification = "unresolved"
)

type EffectResult struct {
	Effect       Effect
	Verification Verification
	Evidence     []EvidenceID
}

type Outcome struct {
	AttemptID    AttemptID
	Verification Verification
	Effects      []EffectResult
	ObservedAt   time.Time
}

// EffectAdapter performs typed operations. A dispatch error leaves effects uncertain.
// Reconcile consults authoritative readbacks, independent of the dispatch response.
type EffectAdapter interface {
	Dispatch(context.Context, Operation) (Acknowledgement, error)
	Reconcile(context.Context, Attempt) (Outcome, error)
}
