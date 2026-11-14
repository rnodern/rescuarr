// Package prowl implements the bounded, privacy-conscious Prowl transport.
package prowl

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const endpoint = "https://api.prowlapp.com/publicapi/add"

const (
	maxEvent       = 1024
	maxDescription = 10000
	maxApplication = 255
	maxResponse    = 16 << 10
)

type Status string

const (
	Accepted   Status = "accepted"
	InvalidKey Status = "invalid_key"
	Quota      Status = "quota"
	Rejected   Status = "rejected"
	Transient  Status = "transient"
	Uncertain  Status = "uncertain"
)

// Message contains only approved notification fields. Callers must build
// privacy-safe summaries; this transport never serialises domain events.
type Message struct {
	Application string
	Event       string
	Description string
	Priority    int
}

type Result struct {
	Status       Status
	HTTPStatus   int
	ProviderCode int
	Remaining    int
	ResetAt      time.Time
	RetryAfter   time.Duration
}

type Client struct{ http *http.Client }

// NewClient uses the fixed HTTPS provider endpoint and a caller-supplied
// transport, allowing deterministic tests without live network access.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	copy := *httpClient
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{http: &copy}
}

func (c *Client) Send(ctx context.Context, key string, m Message) (Result, error) {
	if !ValidAPIKey(key) {
		return Result{}, errors.New("invalid Prowl key")
	}
	if m.Priority < -2 || m.Priority > 2 {
		return Result{}, errors.New("invalid Prowl priority")
	}
	if m.Event == "" && m.Description == "" {
		return Result{}, errors.New("Prowl event or description is required")
	}
	form := url.Values{"apikey": {key}, "application": {truncate(m.Application, maxApplication)}, "priority": {fmt.Sprint(m.Priority)}}
	if m.Event != "" {
		form.Set("event", truncate(m.Event, maxEvent))
	}
	if m.Description != "" {
		form.Set("description", truncate(m.Description, maxDescription))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{Status: Uncertain}, nil
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if readErr != nil || len(body) > maxResponse {
		return Result{Status: Uncertain, HTTPStatus: resp.StatusCode}, nil
	}
	result := Result{Status: Uncertain, HTTPStatus: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return result, nil
	}
	envelope, valid := parseEnvelope(bytes.TrimSpace(body))
	if !valid {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			result.Status = InvalidKey
		case http.StatusTooManyRequests:
			result.Status = Quota
		}
		return result, nil
	}
	if envelope.kind == "success" {
		if envelope.code != http.StatusOK || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return result, nil
		}
		result.Status, result.Remaining = Accepted, envelope.remaining
		if envelope.reset > 0 {
			result.ResetAt = time.Unix(envelope.reset, 0).UTC()
		}
		return result, nil
	}
	result.ProviderCode = envelope.code
	// HTTP 200 can carry a Prowl error. HTTP 429 and Prowl 406 both signal
	// quota. Otherwise disagreements leave the outcome uncertain.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != envelope.code &&
		!(resp.StatusCode == http.StatusTooManyRequests && envelope.code == http.StatusNotAcceptable) {
		return result, nil
	}
	switch {
	case envelope.code == http.StatusUnauthorized:
		result.Status = InvalidKey
	case envelope.code == http.StatusNotAcceptable || envelope.code == http.StatusTooManyRequests:
		result.Status = Quota
	case envelope.code == http.StatusInternalServerError:
		result.Status = Transient
	case envelope.code >= 400 && envelope.code < 500:
		result.Status = Rejected
	}
	return result, nil
}

// ValidAPIKey accepts exactly one Prowl user key. The provider also accepts
// comma-separated keys, but a route is deliberately isolated to one account.
func ValidAPIKey(key string) bool {
	if len(key) != 40 {
		return false
	}
	var decoded [20]byte
	_, err := hex.Decode(decoded[:], []byte(key))
	return err == nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for len(s) > max {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s
}

type providerEnvelope struct {
	kind      string
	code      int
	remaining int
	reset     int64
}

// parseEnvelope accepts exactly one recognised child under <prowl>, with no
// trailing document content or unrecognised provider response elements.
func parseEnvelope(body []byte) (providerEnvelope, bool) {
	d := xml.NewDecoder(bytes.NewReader(body))
	var out providerEnvelope
	for {
		tok, err := d.Token()
		if err != nil {
			return out, false
		}
		if start, ok := tok.(xml.StartElement); ok {
			if start.Name != (xml.Name{Local: "prowl"}) || len(start.Attr) != 0 {
				return out, false
			}
			break
		}
		if chars, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(chars)) != "" {
			return out, false
		}
	}
	children := 0
	for {
		tok, err := d.Token()
		if err != nil {
			return out, false
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			children++
			if children != 1 || tok.Name.Space != "" {
				return out, false
			}
			var code int
			var remaining int
			var reset int64
			codeSeen := false
			seenAttrs := make(map[string]bool, len(tok.Attr))
			for _, attr := range tok.Attr {
				if attr.Name.Space != "" || seenAttrs[attr.Name.Local] {
					return out, false
				}
				seenAttrs[attr.Name.Local] = true
				var err error
				switch attr.Name.Local {
				case "code":
					code, err = strconv.Atoi(attr.Value)
					codeSeen = err == nil
				case "remaining":
					remaining, err = strconv.Atoi(attr.Value)
				case "resetdate":
					reset, err = strconv.ParseInt(attr.Value, 10, 64)
				default:
					return out, false
				}
				if err != nil {
					return out, false
				}
			}
			if !codeSeen {
				return out, false
			}
			switch tok.Name.Local {
			case "success":
				out = providerEnvelope{kind: "success", code: code, remaining: remaining, reset: reset}
			case "error":
				if remaining != 0 || reset != 0 {
					return out, false
				}
				out = providerEnvelope{kind: "error", code: code}
			default:
				return out, false
			}
			closed := false
			for !closed {
				child, err := d.Token()
				if err != nil {
					return out, false
				}
				switch child := child.(type) {
				case xml.EndElement:
					if child.Name != tok.Name {
						return out, false
					}
					closed = true
				case xml.CharData:
					// Prowl's error response may include text; it is never surfaced.
					if out.kind == "success" && strings.TrimSpace(string(child)) != "" {
						return out, false
					}
				case xml.StartElement:
					return out, false
				}
			}
		case xml.EndElement:
			if tok.Name != (xml.Name{Local: "prowl"}) || children != 1 {
				return out, false
			}
			for {
				extra, err := d.Token()
				if err == io.EOF {
					return out, true
				}
				if err != nil {
					return out, false
				}
				if chars, ok := extra.(xml.CharData); !ok || strings.TrimSpace(string(chars)) != "" {
					return out, false
				}
			}
		case xml.CharData:
			if strings.TrimSpace(string(tok)) != "" {
				return out, false
			}
		}
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if d, err := time.ParseDuration(value + "s"); err == nil {
		return bounded(d)
	}
	if t, err := http.ParseTime(value); err == nil {
		return bounded(t.Sub(now))
	}
	return 0
}

func bounded(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > time.Hour {
		return time.Hour
	}
	return d
}
