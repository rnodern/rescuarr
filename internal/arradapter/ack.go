package arradapter

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/rnodern/rescuarr/internal/domain"
)

const maxAckBytes = 64 << 10

func acknowledge(route Route, response *http.Response) (domain.Acknowledgement, bool) {
	if response == nil || response.Body == nil {
		return domain.Acknowledgement{}, false
	}
	encoding := response.Header.Values("Content-Encoding")
	if len(encoding) > 1 || (len(encoding) == 1 && !strings.EqualFold(encoding[0], "identity")) {
		return domain.Acknowledgement{}, false
	}
	ack := domain.Acknowledgement{State: domain.DispatchAcknowledged}
	switch route {
	case RouteRejectPreserve, RouteRejectRemove:
		// The pinned single-row DELETE declares HTTP 200 with no response
		// model. This acknowledgement carries no queue or deletion evidence.
		if response.StatusCode != http.StatusOK {
			return domain.Acknowledgement{}, false
		}
		body, ok := readAcknowledgement(response, 0)
		if !ok || len(body) != 0 {
			return domain.Acknowledgement{}, false
		}
	case RouteCopyImport, RouteEpisodeSearch, RouteMovieSearch:
		// Both pinned CommandControllers return Created for the tracked
		// command. A Location header alone cannot establish its native ID.
		if response.StatusCode != http.StatusCreated || !jsonResponse(response) {
			return domain.Acknowledgement{}, false
		}
		body, ok := readAcknowledgement(response, maxAckBytes)
		if !ok {
			return domain.Acknowledgement{}, false
		}
		id, ok := commandID(body, route)
		if !ok {
			return domain.Acknowledgement{}, false
		}
		ack.UpstreamReference = strconv.FormatInt(int64(id), 10)
	default:
		return domain.Acknowledgement{}, false
	}
	return ack, true
}

func jsonResponse(response *http.Response) bool {
	contentType := response.Header.Values("Content-Type")
	if len(contentType) != 1 {
		return false
	}
	kind, _, err := mime.ParseMediaType(contentType[0])
	return err == nil && strings.EqualFold(kind, "application/json")
}

func readAcknowledgement(response *http.Response, limit int) ([]byte, bool) {
	if response.ContentLength > int64(limit) {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	return body, err == nil && len(body) <= limit &&
		(response.ContentLength < 0 || response.ContentLength == int64(len(body)))
}

func commandID(body []byte, route Route) (int32, bool) {
	reader := json.NewDecoder(bytes.NewReader(body))
	if opening, err := reader.Token(); err != nil || opening != json.Delim('{') {
		return 0, false
	}
	fields := make(map[string]json.RawMessage)
	for reader.More() {
		token, err := reader.Token()
		field, ok := token.(string)
		if err != nil || !ok || fields[field] != nil {
			return 0, false
		}
		var value json.RawMessage
		if reader.Decode(&value) != nil {
			return 0, false
		}
		fields[field] = value
	}
	if closing, err := reader.Token(); err != nil || closing != json.Delim('}') {
		return 0, false
	}
	if _, err := reader.Token(); err != io.EOF {
		return 0, false
	}

	var id int32
	var name, status string
	if json.Unmarshal(fields["id"], &id) != nil || id <= 0 ||
		json.Unmarshal(fields["name"], &name) != nil || name != commandName(route) ||
		json.Unmarshal(fields["status"], &status) != nil {
		return 0, false
	}
	switch status {
	case "queued", "started", "completed", "failed", "aborted", "cancelled", "orphaned":
		return id, true
	default:
		return 0, false
	}
}

func commandName(route Route) string {
	switch route {
	case RouteCopyImport:
		return "ManualImport"
	case RouteEpisodeSearch:
		return "EpisodeSearch"
	case RouteMovieSearch:
		return "MoviesSearch"
	default:
		return ""
	}
}
