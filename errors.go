package pixlub

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Sentinel errors: test them with errors.Is, and read details with errors.As
// into *APIError or *ConnectionError.
var (
	// ErrConfiguration means the API key or base URL is missing or invalid.
	// Nothing was sent.
	ErrConfiguration = errors.New("pixlub: configuration error")

	// ErrInvalidInput means an argument was refused locally. Nothing was sent.
	ErrInvalidInput = errors.New("pixlub: invalid input")

	// ErrUnknownTool means the tool id is not in the catalog. It also matches
	// ErrInvalidInput.
	ErrUnknownTool = fmt.Errorf("%w: unknown tool", ErrInvalidInput)

	// ErrFileTooLarge means a file is too large, locally or for the tool or your
	// plan (HTTP 413, see APIError.MaxBytes).
	ErrFileTooLarge = errors.New("pixlub: file too large")

	// ErrNoData means the run produced no data output.
	ErrNoData = errors.New("pixlub: the run produced no data output")

	// ErrConnection means the API could not be reached, or the connection broke.
	ErrConnection = errors.New("pixlub: connection error")

	// ErrTimeout means the API did not answer in time. It also matches ErrConnection.
	ErrTimeout = fmt.Errorf("%w: timeout", ErrConnection)

	// ErrJobTimeout means a long run did not finish within the job timeout (see
	// WithJobTimeout). The run was accepted and may have been charged. It also
	// matches ErrTimeout.
	ErrJobTimeout = fmt.Errorf("%w: the job did not finish in time", ErrTimeout)

	// ErrUnexpectedResponse means the answer of the API could not be read.
	ErrUnexpectedResponse = errors.New("pixlub: unexpected response")

	// ErrAPI means the API answered with an error status. Every error below
	// matches it.
	ErrAPI = errors.New("pixlub: API error")

	// ErrInvalidRequest matches HTTP 4xx answers, such as invalid settings.
	ErrInvalidRequest = fmt.Errorf("%w: invalid request", ErrAPI)
	// ErrAuthentication matches HTTP 401: the key is unknown or revoked.
	ErrAuthentication = fmt.Errorf("%w: authentication failed", ErrAPI)
	// ErrPermissionDenied matches HTTP 403: the key is valid, but the account
	// may not do this, for example on a free plan.
	ErrPermissionDenied = fmt.Errorf("%w: permission denied", ErrAPI)
	// ErrNotFound matches HTTP 404.
	ErrNotFound = fmt.Errorf("%w: not found", ErrAPI)
	// ErrInsufficientCredits matches HTTP 402: not enough credits for the tool.
	ErrInsufficientCredits = fmt.Errorf("%w: insufficient credits", ErrAPI)
	// ErrRateLimited matches HTTP 429: too many requests. It is never charged.
	ErrRateLimited = fmt.Errorf("%w: rate limited", ErrAPI)
	// ErrServer matches HTTP 5xx: something failed on the server side.
	ErrServer = fmt.Errorf("%w: server error", ErrAPI)
)

// APIError is an error answer of the API.
type APIError struct {
	// HTTPStatus is the HTTP status code of the answer.
	HTTPStatus int
	// Code is the machine-readable code, such as "insufficient_credits", or "".
	Code string
	// Message is the message of the API.
	Message string
	// Details lists the invalid settings of an HTTP 400 answer, or is nil.
	Details any
	// RequiredCredits is the cost of the run (HTTP 402), or nil.
	RequiredCredits *int64
	// MaxBytes is the largest file accepted (HTTP 413), or nil.
	MaxBytes *int64
	// MaxPixels is the largest image accepted (Code "image_too_large"), or nil.
	MaxPixels *int64
	// Limit is the limit that was reached, such as the files of a batch for your
	// plan (Code "batch_limit"), or nil.
	Limit *int64
	// ResetAt is when a quota resets, when the API says so. It is zero otherwise.
	ResetAt time.Time
	// RetryAfter is how long to wait before trying again, or zero.
	RetryAfter time.Duration
	// JobID is the id of the long run that failed, or "".
	JobID string

	kind error
}

func (e *APIError) Error() string {
	text := fmt.Sprintf("%s (HTTP %d", e.Message, e.HTTPStatus)
	if e.Code != "" {
		text += ", code=" + e.Code
	}
	if e.JobID != "" {
		text += ", job=" + e.JobID
	}
	return text + ")"
}

// Unwrap returns the category sentinel, such as ErrRateLimited.
func (e *APIError) Unwrap() error { return e.kind }

// Is makes an HTTP 413 answer match ErrFileTooLarge, besides its category.
func (e *APIError) Is(target error) bool {
	return target == ErrFileTooLarge && e.HTTPStatus == http.StatusRequestEntityTooLarge
}

// ConnectionError means no usable answer came from the API.
type ConnectionError struct {
	// Message explains the failure.
	Message string
	// RequestSent is true when the run may have been charged: check your
	// balance before running it again.
	RequestSent bool

	kind error
}

func (e *ConnectionError) Error() string { return e.Message }

// Unwrap returns ErrConnection or ErrTimeout.
func (e *ConnectionError) Unwrap() error { return e.kind }

func wrapf(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", sentinel, fmt.Sprintf(format, args...))
}

type tooLargeError struct{ message string }

func (e *tooLargeError) Error() string   { return "pixlub: invalid input: " + e.message }
func (e *tooLargeError) Unwrap() []error { return []error{ErrInvalidInput, ErrFileTooLarge} }

func tooLargef(format string, args ...any) error {
	return &tooLargeError{message: fmt.Sprintf(format, args...)}
}
