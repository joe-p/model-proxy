package server

import (
	"encoding/json"
	"net/http"
)

type apiError struct {
	Status  int
	Type    string
	Code    string
	Message string
	Allow   string
}

func (e *apiError) Error() string {
	return e.Message
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    string `json:"code"`
}

func writeAPIError(w http.ResponseWriter, lr *requestLog, e *apiError) {
	if lr != nil {
		lr.errorCode = e.Code
	}
	if e.Allow != "" {
		w.Header().Set("Allow", e.Allow)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{
		Message: e.Message,
		Type:    e.Type,
		Param:   nil,
		Code:    e.Code,
	}})
}

func errInvalidAPIKey() *apiError {
	return &apiError{
		Status:  http.StatusUnauthorized,
		Type:    "invalid_request_error",
		Code:    "invalid_api_key",
		Message: "Invalid API key",
	}
}

func errModelNotFound() *apiError {
	return &apiError{
		Status:  http.StatusNotFound,
		Type:    "invalid_request_error",
		Code:    "model_not_found",
		Message: "Model not found",
	}
}

func errInvalidRequest(message string) *apiError {
	return &apiError{
		Status:  http.StatusBadRequest,
		Type:    "invalid_request_error",
		Code:    "invalid_request",
		Message: message,
	}
}

func errRequestBodyTooLarge() *apiError {
	return &apiError{
		Status:  http.StatusRequestEntityTooLarge,
		Type:    "invalid_request_error",
		Code:    "request_too_large",
		Message: "Request body is too large",
	}
}

func errUpstreamUnavailable() *apiError {
	return &apiError{
		Status:  http.StatusBadGateway,
		Type:    "api_error",
		Code:    "upstream_unavailable",
		Message: "Upstream provider is unavailable",
	}
}

func errUpstreamTimeout() *apiError {
	return &apiError{
		Status:  http.StatusGatewayTimeout,
		Type:    "api_error",
		Code:    "upstream_timeout",
		Message: "Upstream provider timed out",
	}
}

func errInternal() *apiError {
	return &apiError{
		Status:  http.StatusInternalServerError,
		Type:    "server_error",
		Code:    "internal_error",
		Message: "Internal server error",
	}
}

func errMethodNotAllowed(allow string) *apiError {
	return &apiError{
		Status:  http.StatusMethodNotAllowed,
		Type:    "invalid_request_error",
		Code:    "method_not_allowed",
		Message: "Method not allowed",
		Allow:   allow,
	}
}

func errNotFound() *apiError {
	return &apiError{
		Status:  http.StatusNotFound,
		Type:    "invalid_request_error",
		Code:    "not_found",
		Message: "Not found",
	}
}
