package prowl

import (
	"context"
	"errors"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

// Delivery is a claimed outbox row joined to its redacted render and key
// reference. Secret material is resolved only immediately before sending.
type Delivery struct {
	Item    domain.OutboxItem
	Message Message
	KeyRef  string
}

// Outbox is the persistence seam. The app layer must implement ClaimDue and
// Complete transactionally; SQL schema and migrations are centrally owned.
type Outbox interface {
	ClaimDue(context.Context, time.Time) (Delivery, bool, error)
	Complete(context.Context, domain.OutboxID, domain.OutboxState, time.Time, domain.ReasonCode) error
}

type KeyResolver interface {
	Resolve(context.Context, string) (string, error)
}

type Worker struct {
	Outbox Outbox
	Keys   KeyResolver
	Client *Client
	Now    func() time.Time
}

// RunOne handles at most one due row. It never logs message or key data.
func (w Worker) RunOne(ctx context.Context) (bool, error) {
	if w.Outbox == nil || w.Keys == nil || w.Client == nil {
		return false, errors.New("Prowl worker dependencies are incomplete")
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	delivery, ok, err := w.Outbox.ClaimDue(ctx, now())
	if err != nil || !ok {
		return ok, err
	}
	key, err := w.Keys.Resolve(ctx, delivery.KeyRef)
	if err != nil {
		return true, w.complete(ctx, delivery.Item.ID, domain.OutboxDelayed, now().Add(time.Minute), domain.ReasonCode("notification_key_unavailable"))
	}
	result, err := w.Client.Send(ctx, key, delivery.Message)
	if err != nil {
		return true, w.complete(ctx, delivery.Item.ID, domain.OutboxFailed, time.Time{}, domain.ReasonCode("notification_invalid_request"))
	}
	state, retryAt, reason := outcome(result, now())
	return true, w.complete(ctx, delivery.Item.ID, state, retryAt, reason)
}

func (w Worker) complete(ctx context.Context, id domain.OutboxID, state domain.OutboxState, at time.Time, reason domain.ReasonCode) error {
	completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return w.Outbox.Complete(completeCtx, id, state, at, reason)
}

func outcome(r Result, now time.Time) (domain.OutboxState, time.Time, domain.ReasonCode) {
	switch r.Status {
	case Accepted:
		return domain.OutboxAccepted, time.Time{}, ""
	case InvalidKey:
		return domain.OutboxFailed, time.Time{}, "notification_key_invalid"
	case Rejected:
		return domain.OutboxFailed, time.Time{}, "notification_rejected"
	case Quota:
		delay := r.RetryAfter
		if delay <= 0 {
			delay = time.Hour
		}
		return domain.OutboxDelayed, now.Add(bounded(delay)), "notification_quota"
	case Transient:
		delay := r.RetryAfter
		if delay <= 0 {
			delay = time.Minute
		}
		return domain.OutboxDelayed, now.Add(bounded(delay)), "notification_transient"
	default:
		return domain.OutboxUncertain, time.Time{}, "notification_acknowledgement_unknown"
	}
}
