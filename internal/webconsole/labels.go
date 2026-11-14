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
	switch code {
	case "upstream_intervention_required":
		return "Arr flagged this download for manual intervention"
	case "no_eligible_files":
		return "Arr found no files eligible for import"
	case "series_matched_by_id":
		return "Sonarr found the series by ID and needs you to confirm the episode before importing"
	case "awaiting_import_assessment":
		return "Download complete. Import still pending"
	case "download_failed":
		return "Download failed"
	case "target_ambiguous":
		return "Arr did not identify every target in this download"
	case "":
		return "Reason unavailable"
	}
	return strings.ReplaceAll(string(code), "_", " ")
}

func caseTitle(item domain.Case) string {
	display := item.Display
	if display.Title != "" {
		if len(item.Targets) == 1 && len(display.Media) == 1 && display.Media[0].Episode != "" {
			return display.Title + " · " + display.Media[0].Episode
		}
		return display.Title
	}
	if display.Release != "" {
		return display.Release
	}
	return "Unidentified download"
}

func serviceLabel(name string, product domain.Product) string {
	if product == "" {
		if name == "" {
			return "Unknown service"
		}
		return name
	}
	kind := connectionProduct(product)
	if name == "" || strings.EqualFold(name, kind) {
		return kind
	}
	return name + " · " + kind
}

func caseTargetLabel(item domain.Case, target domain.TargetRef) string {
	for _, media := range item.Display.Media {
		if media.Target != target {
			continue
		}
		if media.Title != "" && media.Episode != "" {
			return media.Episode + " · " + media.Title
		}
		return media.Episode + media.Title
	}
	if target.Kind == domain.TargetMovie && item.Display.Title != "" {
		return item.Display.Title
	}
	return targetLabel(target)
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
