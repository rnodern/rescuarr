package webconsole

import (
	"fmt"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

func modeLabel(mode domain.Mode) string {
	switch mode {
	case domain.ModeObserve:
		return "Observe"
	case domain.ModeAssist:
		return "Assist"
	case domain.ModeAutonomous:
		return "Autonomous"
	default:
		return "Mode unknown"
	}
}

func stateLabel(state domain.CaseState) string {
	switch state {
	case domain.CaseObserved:
		return "Watching"
	case domain.CaseWaiting:
		return "Waiting"
	case domain.CaseAssessing:
		return "Assessing"
	case domain.CaseProposed:
		return "Review available"
	case domain.CaseReady:
		return "Ready"
	case domain.CaseExecuting:
		return "Running"
	case domain.CaseVerifying:
		return "Verifying"
	case domain.CaseWaitingReplacement:
		return "Waiting for replacement"
	case domain.CaseHeld:
		return "Held"
	case domain.CaseClosed:
		return "Closed"
	default:
		return "Unknown"
	}
}

func reasonLabel(code domain.ReasonCode) string {
	if code == "" {
		return "Reason unavailable"
	}
	return strings.ReplaceAll(string(code), "_", " ")
}

func targetLabel(target domain.TargetRef) string {
	if !target.Valid() {
		return "Job scope"
	}
	if target.Kind == domain.TargetEpisode {
		return fmt.Sprintf("Episode #%d · Series #%d", target.ID, target.SeriesID)
	}
	return fmt.Sprintf("Movie #%d", target.ID)
}

func effectLabel(kind domain.EffectKind) string {
	switch kind {
	case domain.EffectLibraryImport:
		return "Import into the library"
	case domain.EffectQueueRemoval:
		return "Remove the Arr queue entry"
	case domain.EffectClientRemoval:
		return "Remove the download-client job"
	case domain.EffectDataRemoval:
		return "Delete downloaded data"
	case domain.EffectSourceRetained:
		return "Preserve the source download"
	case domain.EffectBlocklist:
		return "Blocklist the release"
	case domain.EffectSearchDispatch:
		return "Request a search"
	case domain.EffectReplacementGrab:
		return "Track a replacement grab"
	default:
		return "Unknown upstream effect"
	}
}

func verificationLabel(value domain.Verification) string {
	switch value {
	case domain.VerificationEffect:
		return "Effect verified"
	case domain.VerificationPartial:
		return "Partially verified"
	case domain.VerificationNoEffect:
		return "Verified no effect"
	case domain.VerificationExternal:
		return "Resolved externally"
	default:
		return "Unresolved"
	}
}

func actionLabel(action domain.Action) string {
	switch action {
	case domain.ActionImport:
		return "import"
	case domain.ActionReject:
		return "release rejection"
	case domain.ActionSearch:
		return "replacement search"
	default:
		return "action"
	}
}

func intentLabel(state domain.IntentState) string {
	switch state {
	case domain.IntentPlanned, domain.IntentAuthorized, domain.IntentReserved:
		return "Scheduled"
	case domain.IntentSending:
		return "Sending request"
	case domain.IntentAcknowledged:
		return "Arr accepted the request. Verification is pending."
	case domain.IntentRejectedBeforeEffect:
		return "Arr rejected the request before any effect"
	case domain.IntentUncertain:
		return "Checking whether Arr accepted the request"
	case domain.IntentReconciling:
		return "Verifying upstream effects"
	case domain.IntentCompleted:
		return "Verification finished"
	case domain.IntentCancelled:
		return "Cancelled"
	default:
		return "Status unknown"
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "Unavailable"
	}
	return t.UTC().Format("2 Jan 2006, 15:04 UTC")
}
