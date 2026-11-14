package prowl

import (
	"context"
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
	for _, m := range []Message{{Priority: 3, Event: "x"}, {Priority: 0}, {Priority: 0, Event: "x", URL: "https://user:pass@example.test/x"}} {
		if _, err := c.Send(context.Background(), "key", m); err == nil {
			t.Fatalf("expected validation error for %+v", m)
		}
	}
}
