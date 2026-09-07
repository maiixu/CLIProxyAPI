package helps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ZedError preserves upstream HTTP and status-envelope failures.
type ZedError struct {
	Code       int
	Message    string
	RequestID  string
	RetryDelay *time.Duration
}

func (e *ZedError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("zed: %s (request %s)", e.Message, e.RequestID)
	}
	return "zed: " + e.Message
}
func (e *ZedError) StatusCode() int            { return e.Code }
func (e *ZedError) RetryAfter() *time.Duration { return e.RetryDelay }

// ZedFrame is either an unchanged Responses event or a transport status.
type ZedFrame struct {
	Event    json.RawMessage
	Type     string
	Response json.RawMessage
	Ended    bool
}

// DecodeZedFrame accepts the native NDJSON stream and Zed status envelopes.
func DecodeZedFrame(line []byte) (ZedFrame, error) {
	var frame ZedFrame
	var object map[string]json.RawMessage
	if errDecode := json.Unmarshal(line, &object); errDecode != nil || object == nil {
		return frame, fmt.Errorf("zed: malformed completion frame")
	}
	if status, ok := object["status"]; ok {
		var name string
		if json.Unmarshal(status, &name) == nil {
			switch name {
			case "started":
				return frame, nil
			case "stream_ended":
				frame.Ended = true
				return frame, nil
			default:
				return frame, fmt.Errorf("zed: unknown completion status %q", name)
			}
		}
		var statusObject map[string]json.RawMessage
		if errDecode := json.Unmarshal(status, &statusObject); errDecode != nil || statusObject == nil {
			return frame, fmt.Errorf("zed: malformed completion status")
		}
		if failure, ok := statusObject["failed"]; ok {
			return frame, NewZedError(http.StatusBadGateway, failure)
		}
		if _, ok := statusObject["queued"]; ok {
			return frame, nil
		}
		return frame, fmt.Errorf("zed: unknown completion status")
	}
	event := line
	if wrapped, ok := object["event"]; ok {
		event = wrapped
		object = nil
		if errDecode := json.Unmarshal(event, &object); errDecode != nil || object == nil {
			return frame, fmt.Errorf("zed: malformed wrapped completion event")
		}
	}
	if errDecode := json.Unmarshal(object["type"], &frame.Type); errDecode != nil || frame.Type == "" {
		return frame, fmt.Errorf("zed: completion event has no type")
	}
	switch frame.Type {
	case "error", "response.error":
		failure := object["error"]
		if len(failure) == 0 {
			failure = event
		}
		return frame, NewZedError(http.StatusBadGateway, failure)
	case "response.failed":
		var response map[string]json.RawMessage
		_ = json.Unmarshal(object["response"], &response)
		return frame, NewZedError(http.StatusBadGateway, response["error"])
	case "response.completed", "response.incomplete":
		frame.Response = object["response"]
		if len(frame.Response) == 0 || bytes.Equal(bytes.TrimSpace(frame.Response), []byte("null")) {
			return frame, fmt.Errorf("zed: terminal event has no response")
		}
		var response map[string]json.RawMessage
		if errDecode := json.Unmarshal(frame.Response, &response); errDecode != nil || response == nil {
			return frame, fmt.Errorf("zed: malformed terminal response")
		}
	}
	frame.Event = bytes.Clone(event)
	return frame, nil
}

// NewZedError preserves failure classification and a bounded diagnostic message.
func NewZedError(defaultCode int, body []byte) *ZedError {
	result := &ZedError{Code: defaultCode, Message: http.StatusText(defaultCode)}
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		var message string
		if json.Unmarshal(body, &message) != nil {
			message = string(body)
		}
		if bounded := boundedZedErrorMessage(message); bounded != "" {
			result.Message = bounded
		}
		return result
	}
	if nested, ok := object["error"]; ok {
		var message string
		if json.Unmarshal(nested, &message) == nil {
			if bounded := boundedZedErrorMessage(message); bounded != "" {
				result.Message = bounded
			}
		}
		var inner map[string]json.RawMessage
		if json.Unmarshal(nested, &inner) == nil && inner != nil {
			object = inner
		}
	}
	var code string
	if raw := object["code"]; len(raw) > 0 {
		if json.Unmarshal(raw, &code) != nil {
			code = string(raw)
		}
		if parsed, errParse := strconv.Atoi(code); errParse == nil && parsed >= 400 && parsed <= 599 {
			result.Code = parsed
		} else {
			switch strings.ToLower(code) {
			case "unauthorized", "authentication_error", "invalid_api_key":
				result.Code = http.StatusUnauthorized
			case "forbidden", "permission_denied", "access_denied":
				result.Code = http.StatusForbidden
			case "rate_limit_exceeded", "rate_limit_error", "too_many_requests":
				result.Code = http.StatusTooManyRequests
			}
		}
	}
	var message string
	if json.Unmarshal(object["message"], &message) == nil && message != "" {
		if bounded := boundedZedErrorMessage(message); bounded != "" {
			result.Message = bounded
		}
	}
	_ = json.Unmarshal(object["request_id"], &result.RequestID)
	var seconds float64
	if json.Unmarshal(object["retry_after"], &seconds) == nil && seconds > 0 {
		delay := time.Duration(seconds * float64(time.Second))
		result.RetryDelay = &delay
	}
	return result
}

// boundedZedErrorMessage keeps schema diagnostics useful without returning error pages.
func boundedZedErrorMessage(message string) string {
	const limit = 512
	message = strings.TrimSpace(message)
	truncated := len(message) > limit
	if truncated {
		message = message[:limit]
	}
	lower := strings.ToLower(message)
	if strings.HasPrefix(lower, "<") || strings.Contains(lower, "<html") ||
		strings.Contains(lower, "<body") || strings.Contains(lower, "<script") ||
		strings.Contains(lower, "<!doctype") {
		return ""
	}
	message = strings.ToValidUTF8(message, "")
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, message)
	if truncated {
		message += "..."
	}
	return message
}
