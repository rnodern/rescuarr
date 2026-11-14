package prowl

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestClientAcceptedAndUTF8FieldLimit(t *testing.T) {
	client := NewClient(&http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != endpoint || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("apikey") != "secret-test-key" {
			t.Fatal("key missing from form")
		}
		if got := r.Form.Get("event"); len(got) > maxEvent || !utf8Valid(got) {
			t.Fatalf("event is not bounded UTF-8: %d", len(got))
		}
		if strings.Contains(r.Form.Get("description"), "/private/path") {
			t.Fatal("transport unexpectedly added private path")
		}
		return response(200, `<prowl><success code="200" remaining="4" resetdate="1800000000"/></prowl>`), nil
	})})
	result, err := client.Send(context.Background(), "secret-test-key", Message{Application: "Rescuarr", Event: strings.Repeat("a", 1023) + "🙂", Description: "2 incidents", Priority: 0})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != Accepted || result.Remaining != 4 || result.ResetAt.Unix() != 1800000000 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestProviderResponseCategories(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   Status
	}{
		{"invalid key", 401, `<prowl><error code="401">invalid</error></prowl>`, InvalidKey},
		{"provider quota", 200, `<prowl><error code="406">limit</error></prowl>`, Quota},
		{"http quota", 429, `temporarily rate limited`, Quota},
		{"explicit transient", 200, `<prowl><error code="500">temporary</error></prowl>`, Transient},
		{"misclassified html", 200, `<html>success</html>`, Uncertain},
		{"misclassified http success", 503, `<prowl><success code="200" remaining="1"/></prowl>`, Uncertain},
		{"malformed", 200, `<prowl><success`, Uncertain},
		{"conflicting response", 200, `<prowl><success code="200"/><error code="401"/></prowl>`, Uncertain},
		{"duplicate response", 200, `<prowl><success code="200"/><success code="200"/></prowl>`, Uncertain},
		{"unknown response element", 200, `<prowl><success code="200"/><other/></prowl>`, Uncertain},
		{"nested response", 200, `<prowl><success code="200"><error code="401"/></success></prowl>`, Uncertain},
		{"foreign root namespace", 200, `<prowl xmlns="https://unexpected.invalid"><success code="200"/></prowl>`, Uncertain},
		{"foreign child namespace", 200, `<prowl xmlns:x="https://unexpected.invalid"><x:success code="200"/></prowl>`, Uncertain},
		{"namespaced code", 200, `<prowl xmlns:x="https://unexpected.invalid"><success x:code="200"/></prowl>`, Uncertain},
		{"duplicate code", 200, `<prowl><success code="200" code="401"/></prowl>`, Uncertain},
		{"trailing markup", 200, `<prowl><success code="200"/></prowl><prowl><success code="200"/></prowl>`, Uncertain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient(&http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return response(tt.status, tt.body), nil })})
			got, err := c.Send(context.Background(), "key", Message{Application: "Rescuarr", Event: "Notice"})
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.want {
				t.Fatalf("got %q want %q", got.Status, tt.want)
			}
		})
	}
}

func TestRedirectDoesNotReplayCredentialedRequest(t *testing.T) {
	requests := 0
	hc := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		if requests > 1 {
			t.Fatal("redirect replayed credentialed request")
		}
		if r.URL.String() != endpoint {
			t.Fatalf("unexpected URL %s", r.URL)
		}
		res := response(http.StatusTemporaryRedirect, "")
		res.Header.Set("Location", "https://attacker.invalid/collect")
		return res, nil
	})}
	got, err := NewClient(hc).Send(context.Background(), "secret-test-key", Message{Application: "Rescuarr", Event: "Test"})
	if err != nil || got.Status != Uncertain || requests != 1 {
		t.Fatalf("got %+v, requests=%d, err=%v", got, requests, err)
	}
}

func TestWorkerMapsDeliveryStates(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		result  Result
		state   domain.OutboxState
		reason  domain.ReasonCode
		delayed bool
	}{
		{Result{Status: Accepted}, domain.OutboxAccepted, "", false},
		{Result{Status: InvalidKey}, domain.OutboxFailed, "notification_key_invalid", false},
		{Result{Status: Quota}, domain.OutboxDelayed, "notification_quota", true},
		{Result{Status: Transient}, domain.OutboxDelayed, "notification_transient", true},
		{Result{Status: Uncertain}, domain.OutboxUncertain, "notification_acknowledgement_unknown", false},
	} {
		state, when, reason := outcome(tt.result, now)
		if state != tt.state || reason != tt.reason || (!when.IsZero()) != tt.delayed {
			t.Fatalf("outcome(%s) = %s %v %s", tt.result.Status, state, when, reason)
		}
	}
}

type testOutbox struct {
	delivery Delivery
	complete bool
	state    domain.OutboxState
	reason   domain.ReasonCode
}

func (o *testOutbox) ClaimDue(context.Context, time.Time) (Delivery, bool, error) {
	return o.delivery, true, nil
}
func (o *testOutbox) Complete(ctx context.Context, _ domain.OutboxID, state domain.OutboxState, _ time.Time, reason domain.ReasonCode) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("completion context has no bound")
	}
	o.complete, o.state, o.reason = true, state, reason
	return nil
}

type testKeys struct{}

func (testKeys) Resolve(context.Context, string) (string, error) { return "key", nil }

func TestWorkerCompletesInvalidLocalMessageAfterCancellation(t *testing.T) {
	o := &testOutbox{delivery: Delivery{Item: domain.OutboxItem{ID: "outbox-1"}, KeyRef: "ref", Message: Message{}}}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	w := Worker{Outbox: o, Keys: testKeys{}, Client: NewClient(&http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid local message reached transport")
		return nil, nil
	})})}
	processed, err := w.RunOne(canceled)
	if err != nil || !processed || !o.complete || o.state != domain.OutboxFailed || o.reason != "notification_invalid_request" {
		t.Fatalf("processed=%v err=%v outbox=%+v", processed, err, o)
	}
}

func TestDroppedResponseIsUncertainAndRateLimitBounded(t *testing.T) {
	c := NewClient(&http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })})
	got, err := c.Send(context.Background(), "key", Message{Application: "Rescuarr", Event: "Notice"})
	if err != nil || got.Status != Uncertain {
		t.Fatalf("got %+v, %v", got, err)
	}
	if d := parseRetryAfter("999999", time.Now()); d != time.Hour {
		t.Fatalf("retry bound = %s", d)
	}
}

func TestRequestValidation(t *testing.T) {
	c := NewClient(&http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { t.Fatal("request should not be sent"); return nil, nil })})
	for _, m := range []Message{{Priority: 3, Event: "x"}, {Priority: 0}} {
		if _, err := c.Send(context.Background(), "key", m); err == nil {
			t.Fatalf("expected validation error for %+v", m)
		}
	}
}
