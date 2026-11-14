// Package prowl implements the bounded, privacy-conscious Prowl transport.
package prowl

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	URL         string
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
	return &Client{http: httpClient}
}

func (c *Client) Send(ctx context.Context, key string, m Message) (Result, error) {
	if key == "" || !utf8Valid(key) {
		return Result{}, errors.New("invalid Prowl key")
	}
	if m.Priority < -2 || m.Priority > 2 {
		return Result{}, errors.New("invalid Prowl priority")
	}
	if m.Event == "" && m.Description == "" {
		return Result{}, errors.New("Prowl event or description is required")
	}
	if m.URL != "" {
		u, err := url.Parse(m.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return Result{}, errors.New("invalid Prowl URL")
		}
	}
	form := url.Values{"apikey": {key}, "application": {truncate(m.Application, maxApplication)}, "priority": {fmt.Sprint(m.Priority)}}
	if m.Event != "" {
		form.Set("event", truncate(m.Event, maxEvent))
	}
	if m.Description != "" {
		form.Set("description", truncate(m.Description, maxDescription))
	}
	if m.URL != "" {
		form.Set("url", m.URL)
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
	result := Result{HTTPStatus: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	if resp.StatusCode == http.StatusUnauthorized {
		result.Status = InvalidKey
		return result, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		result.Status = Quota
		return result, nil
	}
	var envelope struct {
		XMLName xml.Name `xml:"prowl"`
		Success *struct {
			Code      int   `xml:"code,attr"`
			Remaining int   `xml:"remaining,attr"`
			Reset     int64 `xml:"resetdate,attr"`
		} `xml:"success"`
		Error *struct {
			Code int `xml:"code,attr"`
		} `xml:"error"`
	}
	if err := xml.Unmarshal(bytes.TrimSpace(body), &envelope); err != nil || envelope.XMLName.Local != "prowl" {
		result.Status = Uncertain
		return result, nil
	}
	if envelope.Success != nil {
		if envelope.Success.Code != 200 || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			result.Status = Uncertain
			return result, nil
		}
		result.Status, result.Remaining = Accepted, envelope.Success.Remaining
		if envelope.Success.Reset > 0 {
			result.ResetAt = time.Unix(envelope.Success.Reset, 0).UTC()
		}
		return result, nil
	}
	if envelope.Error == nil {
		result.Status = Uncertain
		return result, nil
	}
	result.ProviderCode = envelope.Error.Code
	switch {
	case envelope.Error.Code == 401 || resp.StatusCode == http.StatusUnauthorized:
		result.Status = InvalidKey
	case envelope.Error.Code == 406 || resp.StatusCode == http.StatusTooManyRequests:
		result.Status = Quota
	case envelope.Error.Code == 500 || resp.StatusCode >= 500:
		result.Status = Transient
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		result.Status = Rejected
	default:
		result.Status = Uncertain
	}
	return result, nil
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

// Kept local so truncation is measured in UTF-8 bytes and never splits a rune.
func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "") == s }

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
