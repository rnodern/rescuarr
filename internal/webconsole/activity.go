package webconsole

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
)

const activityPageSize = 50

type activityRow struct {
	app.RecoveryActivityEntry
	Status    string
	Event     string
	Detail    string
	Attention bool
}

type activityView struct {
	Rows   []activityRow
	Before int64
	Next   int64
}

func (c console) activity(w http.ResponseWriter, r *http.Request) {
	before := int64(0)
	if value := r.URL.Query().Get("before"); value != "" {
		var err error
		before, err = strconv.ParseInt(value, 10, 64)
		if err != nil || before < 1 {
			c.errorPage(w, r, http.StatusBadRequest, "activity", "Invalid activity page", "Start again from Activity.")
			return
		}
	}
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	entries, err := c.RecoveryActivity.RecoveryActivity(r.Context(), activityPageSize+1, before)
	if err != nil {
		c.readError(w, r, err)
		return
	}
	content := activityView{Before: before}
	if len(entries) > activityPageSize {
		entries = entries[:activityPageSize]
		content.Next = entries[len(entries)-1].Sequence
	}
	for _, entry := range entries {
		if !localID(string(entry.CaseID)) {
			entry.CaseID = ""
		}
		state, event, attention := activityLabel(entry.Kind, entry.Reason)
		row := activityRow{RecoveryActivityEntry: entry, Status: state, Event: event, Attention: attention}
		if entry.Reason != "" && !strings.HasPrefix(string(entry.Kind), "recovery_manual_import_") && entry.Kind != "recovery_connection_removed" && entry.Kind != "recovery_empty_pack_done" && entry.Kind != "recovery_empty_pack_send_cancelled" && (entry.Kind != "action_readback_recorded" || state == "Recorded") {
			row.Detail = reasonLabel(entry.Reason)
		}
		content.Rows = append(content.Rows, row)
	}
	c.render(w, r, http.StatusOK, "activity", page{Title: "Activity", Active: "activity", Status: status, Body: content})
}

func activityLabel(kind domain.EventKind, reason domain.ReasonCode) (status, event string, attention bool) {
	switch kind {
	case "recovery_queued", "action_authorized":
		return "Pending", "Recovery queued", false
	case "action_reserved":
		return "Pending", "Recovery picked up by the worker", false
	case "action_sending", "action_sending_recorded":
		return "Pending", "Preparing to send the request", false
	case "action_acknowledged":
		return "Pending", "Arr accepted the request. Checking the result.", false
	case "action_dispatch_recorded":
		return "Pending", "Request response recorded", false
	case "action_reconciling":
		return "Pending", "Checking what changed in Arr", false
	case "recovery_review_needed":
		return "Needs review", "Download needs review", true
	case "recovery_unavailable":
		return "Held", "Recovery could not proceed", true
	case "recovery_waiting_for_qualification":
		return "Held", "Automatic import is unavailable for this setup", true
	case "action_uncertain", "action_unresolved":
		return "Needs review", "Waiting to confirm what happened in Arr", true
	case "action_rejected_before_effect":
		return "Held", "Arr rejected the request", true
	case "action_cancelled_unsent":
		return "Cancelled", "Recovery cancelled before sending", true
	case "action_pause":
		return "Paused", "Automatic recovery paused", true
	case "action_resume":
		return "Recorded", "Automatic recovery resumed", false
	case "recovery_manual_approved":
		return "Approved", "Manual recovery approved", false
	case "recovery_manual_declined":
		return "Declined", "Manual recovery declined", false
	case "recovery_manual_review_changed":
		return "Needs review", "The evidence changed before recovery", true
	case "recovery_manual_mode_enabled":
		return "Recorded", "Switched to Manual mode", false
	case "recovery_manual_mode_disabled":
		return "Recorded", "Switched to Observe mode", false
	case "recovery_manual_import_sending":
		return "Pending", "Sending the approved Copy import to Sonarr", false
	case "recovery_manual_import_verified":
		return "Verified", "Copy import verified. Sonarr linked the approved episode and kept the source file.", false
	case "recovery_manual_import_cleanup_profile_verified":
		return "Verified", "Import verified. Sonarr linked the approved episode. Sonarr manages download cleanup.", false
	case "recovery_manual_import_uncertain":
		return "Needs review", "Waiting to confirm the Copy import in Sonarr", true
	case "recovery_manual_import_failed":
		return "Failed", "Copy import failed", true
	case "recovery_manual_import_cancelled":
		return "Cancelled", "Copy import cancelled before sending", true
	case "recovery_manual_import_unconfirmed":
		return "Unconfirmed", "Owner closed the review without import confirmation. Rescuarr did not retry.", true
	case "recovery_external_import_verified":
		return "Verified", "Sonarr import verified. Case closed.", false
	case "recovery_queue_missing":
		return "Closed", "Download left the queue", true
	case "recovery_connection_removed":
		return "Closed", "Closed after connection removal", false
	case "recovery_empty_pack_reject_sending":
		return "Pending", "Removing and blocklisting the empty download", false
	case "recovery_empty_pack_reject_verified":
		return "Verified", "Empty download removed and release blocklisted", false
	case "recovery_empty_pack_search_sending":
		return "Pending", "Requesting a search for the remaining episodes", false
	case "recovery_empty_pack_send_cancelled":
		return "Held", "Recovery paused before sending because the checks changed", true
	case "recovery_empty_pack_done":
		if reason == "empty_pack_no_search_needed" {
			return "Done", "No episodes needed a replacement search", false
		}
		if reason == "empty_pack_no_approved_search_needed" {
			return "Done", "No approved episodes needed a replacement search", false
		}
		return "Requested", "Sonarr accepted the episode search", false
	case "recovery_verified":
		return "Verified", "Recovery verified", false
	case "action_readback_recorded", "action_outcome":
		switch domain.Verification(reason) {
		case domain.VerificationEffect:
			return "Verified", "Recovery verified", false
		case domain.VerificationNoEffect:
			return "Verified", "No change in Arr", false
		case domain.VerificationExternal:
			return "Verified", "Resolved outside Rescuarr", false
		case domain.VerificationPartial:
			return "Needs review", "Some changes verified", true
		case domain.VerificationUnresolved, "conflicting_effect_readback":
			return "Needs review", "Could not verify the result", true
		}
	}
	return "Recorded", strings.ReplaceAll(string(kind), "_", " "), false
}
