package arradapter_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/domain"
)

func TestFixtureCommandAmbiguityMakesExactlyOneExchange(t *testing.T) {
	valid := commandReply("EpisodeSearch", 91)
	type replyFixture struct {
		header string
		code   int
		body   string
	}
	for name, sample := range map[string]replyFixture{
		"redirect":          {"text/html", http.StatusTemporaryRedirect, "<html>"},
		"unauthorized":      {"application/json", http.StatusUnauthorized, `{"message":"synthetic denied"}`},
		"not found":         {"application/json", http.StatusNotFound, `{"message":"synthetic not found"}`},
		"server error":      {"application/json", http.StatusBadGateway, `{"message":"synthetic fault"}`},
		"unrecognised 2xx":  {"application/json", http.StatusOK, valid},
		"login page":        {"text/html", http.StatusCreated, "<html>"},
		"missing type":      {"", http.StatusCreated, valid},
		"location only":     {"application/json", http.StatusCreated, ""},
		"malformed":         {"application/json", http.StatusCreated, `{"name":`},
		"trailing JSON":     {"application/json", http.StatusCreated, valid + "{}"},
		"wrong shape":       {"application/json", http.StatusCreated, "[" + valid + "]"},
		"invalid status":    {"application/json", http.StatusCreated, strings.Replace(valid, "queued", "unknown", 1)},
		"wrong exact name":  {"application/json", http.StatusCreated, commandReply("episodEsearch", 91)},
		"wrong route":       {"application/json", http.StatusCreated, commandReply("ManualImport", 91)},
		"ID alone":          {"application/json", http.StatusCreated, `{"id":91}`},
		"null ID":           {"application/json", http.StatusCreated, strings.Replace(valid, "91", "null", 1)},
		"string ID":         {"application/json", http.StatusCreated, strings.Replace(valid, "91", `"91"`, 1)},
		"floating-point ID": {"application/json", http.StatusCreated, strings.Replace(valid, "91", "91.0", 1)},
		"out of range ID":   {"application/json", http.StatusCreated, strings.Replace(valid, "91", "2147483648", 1)},
		"duplicate ID":      {"application/json", http.StatusCreated, `{"id":12,` + valid[1:]},
		"missing ID":        {"application/json", http.StatusCreated, strings.Replace(valid, `"id":91,`, "", 1)},
		"zero ID":           {"application/json", http.StatusCreated, commandReply("EpisodeSearch", 0)},
		"negative ID":       {"application/json", http.StatusCreated, commandReply("EpisodeSearch", -91)},
	} {
		t.Run(name, func(t *testing.T) {
			expectUncertain(t, func(*http.Request) (*http.Response, error) {
				reply := response(sample.code, sample.header, sample.body)
				reply.Header.Set("Location", "https://foreign.example.invalid/api/v3/command/91")
				return reply, nil
			})
		})
	}

	t.Run("transport error after possible send", func(t *testing.T) {
		expectUncertain(t, func(*http.Request) (*http.Response, error) {
			return nil, errors.New("synthetic transport failed with private request data")
		})
	})

	t.Run("no response", func(t *testing.T) {
		expectUncertain(t, func(*http.Request) (*http.Response, error) {
			return nil, nil
		})
	})

	t.Run("body limit and close", func(t *testing.T) {
		source := &measuredBody{Reader: strings.NewReader(valid + strings.Repeat(" ", 2<<20))}
		expectUncertain(t, func(*http.Request) (*http.Response, error) {
			reply := response(http.StatusCreated, "application/json", "")
			reply.ContentLength = -1
			reply.Body = source
			return reply, nil
		})
		if !source.closed || source.bytes == 0 || source.bytes > 65_537 {
			t.Fatalf("unbounded or unclosed acknowledgement body: %d bytes", source.bytes)
		}
	})

	t.Run("incomplete response despite valid prefix", func(t *testing.T) {
		source := &measuredBody{Reader: io.MultiReader(strings.NewReader(valid), brokenReader{})}
		expectUncertain(t, func(*http.Request) (*http.Response, error) {
			reply := response(http.StatusCreated, "application/json", "")
			reply.Body = source
			reply.ContentLength = -1
			return reply, nil
		})
		if !source.closed {
			t.Fatal("faulting acknowledgement body was left open")
		}
	})

	t.Run("mismatched response length", func(t *testing.T) {
		expectUncertain(t, func(*http.Request) (*http.Response, error) {
			reply := response(http.StatusCreated, "application/json", valid)
			reply.ContentLength++
			return reply, nil
		})
	})

	t.Run("encoded response", func(t *testing.T) {
		expectUncertain(t, func(*http.Request) (*http.Response, error) {
			reply := response(http.StatusCreated, "application/json", valid)
			reply.Header.Set("Content-Encoding", "gzip")
			return reply, nil
		})
	})
}

func TestFixtureCancellationAfterExchangeIsUncertain(t *testing.T) {
	cfg, op, prepared := fixture(domain.ProductSonarr, domain.ActionSearch)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sent := 0
	adapter := newAdapter(t, cfg, func(*http.Request) (*http.Response, error) {
		sent++
		cancel()
		return response(http.StatusCreated, "application/json", commandReply("EpisodeSearch", 12)), nil
	}, positive, unexpectedReadback(t), fixedTime)
	ack, err := adapter.DispatchPrepared(ctx, op, prepared)
	if sent != 1 || err != arradapter.ErrUncertain || ack.State != domain.DispatchUncertain || ack.UpstreamReference != "" {
		t.Fatalf("cancelled request gave %d exchanges, %+v and %v", sent, ack, err)
	}
}

func TestFixtureUnprovenQueueResponsesAreUncertain(t *testing.T) {
	for name, reply := range map[string]*http.Response{
		"undocumented empty": response(http.StatusNoContent, "", ""),
		"login page":         response(http.StatusOK, "text/html", "<html>"),
		"not found":          response(http.StatusNotFound, "", ""),
		"partial error":      response(http.StatusInternalServerError, "", ""),
	} {
		t.Run(name, func(t *testing.T) {
			cfg, op, prepared := fixture(domain.ProductSonarr, domain.ActionReject)
			sent := 0
			adapter := newAdapter(t, cfg, func(*http.Request) (*http.Response, error) {
				sent++
				return reply, nil
			}, positive, unexpectedReadback(t), fixedTime)
			ack, err := adapter.DispatchPrepared(t.Context(), op, prepared)
			if sent != 1 || err != arradapter.ErrUncertain || ack.State != domain.DispatchUncertain || ack.UpstreamReference != "" {
				t.Fatalf("queue response gave %d exchanges, %+v and %v", sent, ack, err)
			}
		})
	}
}

func TestFixtureReadbackSeparatelyScopesAmbiguousCommand(t *testing.T) {
	cfg, op, prepared := fixture(domain.ProductSonarr, domain.ActionSearch)
	sent, qualified, read := 0, 0, 0
	adapter := newAdapter(t, cfg, func(*http.Request) (*http.Response, error) {
		sent++
		return nil, io.ErrUnexpectedEOF
	}, func(ctx context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
		qualified++
		return positive(ctx, binding)
	}, func(ctx context.Context, route arradapter.Route, attempt domain.Attempt) (domain.Outcome, error) {
		read++
		if _, bounded := ctx.Deadline(); !bounded {
			t.Fatal("readback lacks a deadline")
		}
		if attempt.ID != "fixture-attempt" || attempt.Operation.Key != op.Key || route != arradapter.RouteEpisodeSearch ||
			attempt.Prepared.Search.ParentReject != prepared.Search.ParentReject || attempt.Acknowledgement.State != domain.DispatchUncertain {
			t.Fatal("readback received the wrong scope or dispatch outcome")
		}
		readback := domain.Outcome{
			AttemptID: attempt.ID, ObservedAt: fixtureTime.Add(time.Second), Verification: domain.VerificationEffect,
			Effects: []domain.EffectResult{{Effect: attempt.Operation.Effects[0], Verification: domain.VerificationEffect,
				Evidence: []domain.EvidenceID{"independently-observed-search"}}},
		}
		attempt.Operation.Targets[0].ID = 99
		attempt.Operation.Effects[0].Target.ID = 99
		attempt.Preflight[0] = "wrong-evidence"
		attempt.Prepared.Search.Evidence[0] = "wrong-evidence"
		attempt.Prepared.Search.ParentReject = "wrong-parent"
		return readback, nil
	}, fixedTime)
	ack, err := adapter.DispatchPrepared(t.Context(), op, prepared)
	if sent != 1 || qualified != 1 || read != 0 || err != arradapter.ErrUncertain || ack.State != domain.DispatchUncertain {
		t.Fatal("response loss did not remain a distinct uncertain acknowledgement")
	}
	attempt := domain.Attempt{ID: "fixture-attempt", Operation: op, Prepared: &prepared,
		Preflight: []domain.EvidenceID{"fenced-preflight"}, SentAt: fixtureTime, Acknowledgement: ack,
	}
	outcome, err := adapter.Reconcile(t.Context(), attempt)
	if err != nil || outcome.AttemptID != attempt.ID || outcome.Verification != domain.VerificationEffect ||
		len(outcome.Effects) != 1 || outcome.Effects[0].Evidence[0] != "independently-observed-search" {
		t.Fatalf("unexpected readback: %+v, %v", outcome, err)
	}
	if sent != 1 || qualified != 1 || read != 1 {
		t.Fatalf("readback made %d exchanges, %d qualification checks and %d reads", sent, qualified, read)
	}
	if attempt.Operation.Effects[0].Target.ID != 31 || attempt.Operation.Targets[0].ID != 31 ||
		attempt.Preflight[0] != "fenced-preflight" || attempt.Prepared.Search.ParentReject != "fixture-parent" ||
		attempt.Prepared.Search.Evidence[0] != "evidence-exact-reject" {
		t.Fatal("readback changed the caller's durable attempt scope")
	}
	attempt.Operation.Job.Instance.ID = "different-instance"
	if _, err := adapter.Reconcile(t.Context(), attempt); err != arradapter.ErrReadback || read != 1 {
		t.Fatal("a foreign attempt reached readback")
	}
}

func expectUncertain(t *testing.T, reply arradapter.FixtureTransport) {
	t.Helper()
	cfg, op, prepared := fixture(domain.ProductSonarr, domain.ActionSearch)
	sent := 0
	adapter := newAdapter(t, cfg, func(req *http.Request) (*http.Response, error) {
		sent++
		return reply(req)
	}, positive, unexpectedReadback(t), fixedTime)
	ack, err := adapter.DispatchPrepared(t.Context(), op, prepared)
	if sent != 1 || err != arradapter.ErrUncertain || ack != (domain.Acknowledgement{State: domain.DispatchUncertain, Reason: "dispatch_uncertain"}) {
		t.Fatalf("ambiguous response gave %d exchanges, %+v and %v", sent, ack, err)
	}
}

type measuredBody struct {
	io.Reader
	bytes  int
	closed bool
}

func (r *measuredBody) Read(buffer []byte) (int, error) {
	read, err := r.Reader.Read(buffer)
	r.bytes += read
	return read, err
}

func (r *measuredBody) Close() error {
	r.closed = true
	return nil
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
