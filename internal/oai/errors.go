package oai

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// OpenAI-shaped error handling.
//
// Clients built against the OpenAI SDKs parse failures out of a specific envelope
// and will report a useful message only if we match it. Everything this server
// returns on an error path therefore goes through here.

// ErrorEnvelope is the body of a failed request.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the error detail OpenAI clients look for.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`

	// RequestID lets an operator find the full detail in the server log. Client
	// errors are deliberately vague about upstream internals — several providers
	// echo the request, API key included, inside their error bodies — so this is
	// the supported way to correlate a user report with what actually happened.
	RequestID string `json:"request_id,omitempty"`
}

// Error types, matching OpenAI's vocabulary.
const (
	ErrTypeInvalidRequest = "invalid_request_error"
	ErrTypeAuthentication = "authentication_error"
	ErrTypePermission     = "permission_error"
	ErrTypeNotFound       = "not_found_error"
	ErrTypeRateLimit      = "rate_limit_error"
	ErrTypeAPI            = "api_error"
	ErrTypeOverloaded     = "overloaded_error"
)

// APIError is an error that knows its own HTTP status and OpenAI error type.
type APIError struct {
	Status    int
	Type      string
	Message   string
	Param     string
	Code      string
	RequestID string

	// Err is the underlying cause. It is logged, never serialised to the client.
	Err error
}

func (e *APIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *APIError) Unwrap() error { return e.Err }

// Envelope renders the client-facing body.
func (e *APIError) Envelope() ErrorEnvelope {
	return ErrorEnvelope{Error: ErrorBody{
		Message:   e.Message,
		Type:      e.Type,
		Param:     e.Param,
		Code:      e.Code,
		RequestID: e.RequestID,
	}}
}

// Constructors for the errors this server actually returns.

// NewInvalidRequest reports a malformed or unacceptable request.
func NewInvalidRequest(msg string, param string) *APIError {
	return &APIError{Status: http.StatusBadRequest, Type: ErrTypeInvalidRequest, Message: msg, Param: param}
}

// NewAuthError reports a missing or wrong bearer token.
func NewAuthError(msg string) *APIError {
	return &APIError{Status: http.StatusUnauthorized, Type: ErrTypeAuthentication, Message: msg}
}

// NewNotFound reports an unknown resource, such as an expired session.
func NewNotFound(msg string) *APIError {
	return &APIError{Status: http.StatusNotFound, Type: ErrTypeNotFound, Message: msg}
}

// NewRateLimit reports that the server is shedding load.
func NewRateLimit(msg string) *APIError {
	return &APIError{Status: http.StatusTooManyRequests, Type: ErrTypeRateLimit, Message: msg}
}

// NewAPIError reports an internal or upstream failure. The cause is attached for
// logging but does not reach the client.
func NewAPIError(msg string, cause error) *APIError {
	return &APIError{Status: http.StatusInternalServerError, Type: ErrTypeAPI, Message: msg, Err: cause}
}

// WriteError renders err as an OpenAI-shaped error response. Anything that is not
// an *APIError becomes a generic 500, because an unrecognised error is by
// definition one whose message we have not vetted for leaking internals.
func WriteError(w http.ResponseWriter, err error, requestID string) {
	apiErr, ok := err.(*APIError)
	if !ok {
		apiErr = NewAPIError("internal server error", err)
	}
	if apiErr.RequestID == "" {
		apiErr.RequestID = requestID
	}
	if apiErr.Status == 0 {
		apiErr.Status = http.StatusInternalServerError
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(apiErr.Status)
	_ = json.NewEncoder(w).Encode(apiErr.Envelope())
}
