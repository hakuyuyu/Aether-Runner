package api

import (
	"encoding/json"
	"net/http"
)

// APIError represents a structured API error
type APIError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	StatusCode int    `json:"-"`
}

// Error implements the error interface
func (e *APIError) Error() string {
	return e.Message
}

// Predefined error types
var (
	ErrResourceExhausted = &APIError{
		Code:       "RESOURCE_EXHAUSTED",
		Message:    "No GPUs available with sufficient VRAM",
		StatusCode: http.StatusServiceUnavailable,
	}

	ErrGPUUnavailable = &APIError{
		Code:       "GPU_UNAVAILABLE",
		Message:    "Requested GPU index not found or not accessible",
		StatusCode: http.StatusServiceUnavailable,
	}

	ErrDockerFailed = &APIError{
		Code:       "DOCKER_FAILED",
		Message:    "Docker daemon error occurred",
		StatusCode: http.StatusInternalServerError,
	}

	ErrInvalidImage = &APIError{
		Code:       "INVALID_IMAGE",
		Message:    "Container image not found or invalid",
		StatusCode: http.StatusBadRequest,
	}

	ErrTenantQuotaExceeded = &APIError{
		Code:       "TENANT_QUOTA_EXCEEDED",
		Message:    "Tenant has reached maximum concurrent workers",
		StatusCode: http.StatusTooManyRequests,
	}

	ErrWorkerNotFound = &APIError{
		Code:       "WORKER_NOT_FOUND",
		Message:    "Worker with the specified ID was not found",
		StatusCode: http.StatusNotFound,
	}

	ErrInvalidRequest = &APIError{
		Code:       "INVALID_REQUEST",
		Message:    "The request body is invalid or malformed",
		StatusCode: http.StatusBadRequest,
	}

	ErrInternalError = &APIError{
		Code:       "INTERNAL_ERROR",
		Message:    "An internal error occurred",
		StatusCode: http.StatusInternalServerError,
	}

	ErrInvalidStateTransition = &APIError{
		Code:       "INVALID_STATE_TRANSITION",
		Message:    "The requested state transition is not valid",
		StatusCode: http.StatusConflict,
	}
)

// NewAPIError creates a new API error with a custom message
func NewAPIError(code string, message string, statusCode int) *APIError {
	return &APIError{
		Code:       code,
		Message:    message,
		StatusCode: statusCode,
	}
}

// WithMessage returns a copy of the error with a custom message
func (e *APIError) WithMessage(message string) *APIError {
	return &APIError{
		Code:       e.Code,
		Message:    message,
		StatusCode: e.StatusCode,
	}
}

// ErrorResponse is the JSON structure returned for errors
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains the error details
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError writes an error response to the HTTP response writer
func WriteError(w http.ResponseWriter, err *APIError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(err.StatusCode)

	response := ErrorResponse{
		Error: ErrorDetail{
			Code:    err.Code,
			Message: err.Message,
		},
	}

	json.NewEncoder(w).Encode(response)
}

// WriteErrorWithMessage writes an error response with a custom message
func WriteErrorWithMessage(w http.ResponseWriter, err *APIError, message string) {
	customErr := err.WithMessage(message)
	WriteError(w, customErr)
}

// FromError attempts to convert a generic error to an APIError
func FromError(err error) *APIError {
	if apiErr, ok := err.(*APIError); ok {
		return apiErr
	}

	return &APIError{
		Code:       "INTERNAL_ERROR",
		Message:    err.Error(),
		StatusCode: http.StatusInternalServerError,
	}
}
