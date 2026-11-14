// Package domain contains the contracts shared by the Rescuarr components.
package domain

import "time"

type (
	InstanceID string
	JobID      string
	CaseID     string
	EvidenceID string
	IncidentID string
	Epoch      uint64
	Revision   uint64
)

type Product string

const (
	ProductSonarr Product = "sonarr"
	ProductRadarr Product = "radarr"
	ProductSAB    Product = "sabnzbd"
)

type InstanceRef struct {
	ID    InstanceID
	Epoch Epoch
}

func (r InstanceRef) Valid() bool { return r.ID != "" && r.Epoch > 0 }

type Instance struct {
	Ref                InstanceRef
	Product            Product
	DisplayName        string
	BaseURL            string
	SecretRef          string
	UpstreamVersion    string
	ExpectedIdentity   string
	ConnectionRevision Revision
}

type ScopeKind string

const (
	ScopeInstance ScopeKind = "instance"
	ScopeRoot     ScopeKind = "root"
	ScopeClient   ScopeKind = "client"
	ScopeVolume   ScopeKind = "volume"
	ScopeStore    ScopeKind = "store"
)

type Scope struct {
	Kind     ScopeKind
	Instance InstanceRef
	Key      string
}

type Source string
type ObservationKind string

type ObservationState string

const (
	ObservationKnown       ObservationState = "known"
	ObservationAbsent      ObservationState = "verified_absent"
	ObservationMissing     ObservationState = "missing"
	ObservationUnknown     ObservationState = "unknown"
	ObservationStale       ObservationState = "stale"
	ObservationUnsupported ObservationState = "unsupported"
)

type Observation struct {
	ID                 EvidenceID
	Scope              Scope
	Source             Source
	Kind               ObservationKind
	SourceRecord       string
	AdapterRevision    string
	SchemaRevision     Revision
	SnapshotGeneration Revision
	ObservedAt         time.Time
	SourceAt           time.Time
	ExpiresAt          time.Time
	ContentHash        string
	State              ObservationState
	Complete           bool
}

func (o Observation) Fresh(at time.Time) bool {
	return o.Complete && (o.State == ObservationKnown || o.State == ObservationAbsent) &&
		!o.ObservedAt.IsZero() && !at.Before(o.ObservedAt) && at.Before(o.ExpiresAt)
}

type Fact[T any] struct {
	Observation Observation
	Value       T
}

type Protocol string

const (
	ProtocolUnknown Protocol = "unknown"
	ProtocolUsenet  Protocol = "usenet"
	ProtocolTorrent Protocol = "torrent"
)

type JobRef struct {
	Instance   InstanceRef
	ID         JobID
	Generation Revision
}

type QueueMember struct {
	QueueID            int64
	SnapshotGeneration Revision
	Target             TargetRef
}

type ReleaseIdentity struct {
	GrabHistoryID int64
	IndexerID     int64
	GUID          string
	Edition       string
}

type Job struct {
	Ref         JobRef
	Client      Scope
	DownloadID  string
	Protocol    Protocol
	Release     ReleaseIdentity
	Members     []QueueMember
	Evidence    []EvidenceID
	Shared      bool
	CompleteSet bool
}

type TargetKind string

const (
	TargetEpisode TargetKind = "episode"
	TargetMovie   TargetKind = "movie"
)

type TargetRef struct {
	Instance InstanceRef
	Kind     TargetKind
	ID       int64
	SeriesID int64
}

func (r TargetRef) Valid() bool {
	return r.Instance.Valid() && r.ID > 0 &&
		((r.Kind == TargetEpisode && r.SeriesID > 0) || (r.Kind == TargetMovie && r.SeriesID == 0))
}

type Target struct {
	Ref             TargetRef
	Root            Scope
	Monitored       Fact[bool]
	ProfileRevision Revision
	FileID          Fact[int64]
}

type CaseState string
type ReasonCode string

const (
	CaseObserved           CaseState = "observed"
	CaseWaiting            CaseState = "waiting"
	CaseAssessing          CaseState = "assessing"
	CaseProposed           CaseState = "proposed"
	CaseReady              CaseState = "ready"
	CaseExecuting          CaseState = "executing"
	CaseVerifying          CaseState = "verifying"
	CaseWaitingReplacement CaseState = "waiting_replacement"
	CaseHeld               CaseState = "held"
	CaseClosed             CaseState = "closed"
)

type Case struct {
	ID          CaseID
	Revision    Revision
	Job         Job
	Targets     []Target
	Incidents   []IncidentID
	State       CaseState
	Reason      ReasonCode
	Evidence    []EvidenceID
	NextCheckAt time.Time
	Display     CaseDisplay `json:",omitzero"`
}

// CaseDisplay contains operator-facing labels. Recovery decisions use typed
// identities and evidence separately.
type CaseDisplay struct {
	Title        string
	Release      string
	Media        []CaseMedia
	EpisodeCheck *CaseEpisodeCheck `json:",omitempty"`
	Service      string            `json:"-"`
	Product      Product           `json:"-"`
	Version      string            `json:"-"`
}

// CaseEpisodeCheck is display evidence from a queue scan, keyed to the
// connections and names inspected during that scan.
type CaseEpisodeCheck struct {
	EpisodeNameReview
	CheckedAt        time.Time
	InputFingerprint string
}

type EpisodeNameReview struct {
	Series       string
	Episode      string
	EpisodeTitle string
	Folder       string
	Filename     string
	TitleInNames bool
	Matched      bool
	Reason       string
	ArrRejection bool
}

type CaseMedia struct {
	Target  TargetRef
	Title   string
	Episode string
}

type Incident struct {
	ID         IncidentID
	Revision   Revision
	Scope      Scope
	Reason     ReasonCode
	Cases      []CaseID
	Evidence   []EvidenceID
	Hold       bool
	OpenedAt   time.Time
	ResolvedAt time.Time
}
