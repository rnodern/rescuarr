// Package app composes local service dependencies and defines the console boundary.
package app

import (
	"context"
	"errors"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

type Clock interface {
	Now() time.Time
}

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

type FixedClock struct{ At time.Time }

func (c FixedClock) Now() time.Time { return c.At.UTC() }

type Overview struct {
	Revision          domain.Revision
	Mode              domain.Mode
	Paused            bool
	ReconcileRequired bool
	ScanningEpisodes  bool
	Cases             int
	HeldCases         int
	OpenIncidents     int
}

type CaseQuery struct {
	ExcludeClosed bool
	Instance      domain.InstanceID
	State         domain.CaseState
	Reason        domain.ReasonCode
	Search        string
	After         string
	Limit         int
}

type CasePage struct {
	Cases []domain.Case
	After string
}

type CaseDetail struct {
	Case      domain.Case
	Proposals []domain.Proposal
	Outcomes  []domain.Outcome
}

type ImportPreview struct {
	Reason      domain.ReasonCode
	Candidates  int
	Eligible    int
	Hold        string
	Episode     *domain.EpisodeNameReview
	Replacement *ReplacementPreview
	Audio       []AudioLanguageReview
}

// ReplacementPreview describes a read-only plan for the exact Sonarr download.
type ReplacementPreview struct {
	Release      string
	Episodes     []ReplacementEpisode
	SearchCount  int
	UnclearCount int
	Language     *PackLanguageReview
}

type PackLanguageReview struct {
	FilenameLanguages []string
	SonarrLanguages   []string
	OriginalLanguage  string
	Agreement         bool
	NeedsReview       bool
	Files             []PackLanguageFile
}

type PackLanguageFile struct {
	Episode                string
	Filename               string
	Origin                 string
	AudioLanguages         []string
	AudioLanguagesComplete bool
	Audio                  *AudioLanguageReview
}

type AudioLanguageReview struct {
	Label             string
	OriginalLanguage  string
	PreferredLanguage string
	FilenameLanguages []string
	ReleaseLanguages  []string
	TrackLanguages    []string
	domain.AudioAssessment
}

type ReplacementEpisode struct {
	Target domain.TargetRef
	Label  string
	Status ReplacementStatus
}

type ReplacementStatus string

const (
	ReplacementSearch        ReplacementStatus = "search"
	ReplacementInLibrary     ReplacementStatus = "in_library"
	ReplacementOtherDownload ReplacementStatus = "other_download"
	ReplacementUnmonitored   ReplacementStatus = "unmonitored"
	ReplacementUnaired       ReplacementStatus = "unaired"
	ReplacementUnclear       ReplacementStatus = "unclear"
)

type ImportPreviewReader interface {
	ImportPreview(context.Context, domain.CaseID, domain.Revision) (ImportPreview, error)
}

// RecoveryActivityEntry describes a recorded recovery event. CaseTitle is empty
// when the case is no longer available.
type RecoveryActivityEntry struct {
	Sequence   int64
	CaseID     domain.CaseID
	CaseTitle  string
	Kind       domain.EventKind
	Reason     domain.ReasonCode
	OccurredAt time.Time
}

type RecoveryActivityReader interface {
	// RecoveryActivity returns up to limit entries in descending sequence order.
	// before excludes that sequence and newer entries. Zero starts at the newest.
	RecoveryActivity(ctx context.Context, limit int, before int64) ([]RecoveryActivityEntry, error)
}

type ConsoleReader interface {
	Overview(context.Context) (Overview, error)
	Cases(context.Context, CaseQuery) (CasePage, error)
	Case(context.Context, domain.CaseID) (CaseDetail, error)
	Intent(context.Context, domain.IntentID) (domain.Intent, error)
}

type ApproveProposal struct {
	CaseID     domain.CaseID
	ProposalID domain.ProposalID
	Revision   domain.Revision
	RequestKey string
	Actor      domain.ActorID
}

type ConsoleCommands interface {
	Approve(context.Context, ApproveProposal) (domain.Intent, error)
}

var (
	ErrNotFound = errors.New("record not found")
	ErrConflict = errors.New("revision conflict")
	ErrHeld     = errors.New("command held")
)
