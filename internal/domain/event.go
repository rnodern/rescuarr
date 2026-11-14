package domain

import "time"

type (
	PatternID string
	EventID   string
	OutboxID  string
	RouteID   string
	EventKind string
)

type PatternSample struct {
	Pattern    PatternID
	Scope      Scope
	Job        JobRef
	Target     TargetRef
	Treatment  Action
	Outcome    Outcome
	Eligible   bool
	Exclusion  ReasonCode
	RecordedAt time.Time
}

type Event struct {
	ID         EventID
	Sequence   uint64
	Kind       EventKind
	Actor      ActorID
	Scope      Scope
	CaseID     CaseID
	IncidentID IncidentID
	IntentID   IntentID
	Reason     ReasonCode
	Evidence   []EvidenceID
	OccurredAt time.Time
}

type OutboxState string

const (
	OutboxPending    OutboxState = "pending"
	OutboxSending    OutboxState = "sending"
	OutboxAccepted   OutboxState = "accepted"
	OutboxSuppressed OutboxState = "suppressed"
	OutboxDelayed    OutboxState = "delayed"
	OutboxFailed     OutboxState = "failed"
	OutboxUncertain  OutboxState = "uncertain"
)

type OutboxItem struct {
	ID            OutboxID
	EventID       EventID
	RouteID       RouteID
	RouteRevision Revision
	DedupeKey     string
	State         OutboxState
	ScheduledAt   time.Time
	ExpiresAt     time.Time
	Attempts      uint
	Reason        ReasonCode
}
