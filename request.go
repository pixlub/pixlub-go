package pixlub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 8 * time.Second
	maxRetryAfter  = 60 * time.Second

	uploadIncompleteTries = 5
)

var (
	runReadFloor  = 70 * time.Second
	pollReadFloor = 35 * time.Second

	uploadIncompletePause = time.Second
)

type retryPolicy int

const (
	policyRead retryPolicy = iota

	policyCreate

	policyClaim

	policyUpload
)

func (p retryPolicy) retriesFailure(failure *transportFailure) bool {
	switch p {
	case policyRead, policyUpload:
		return true
	case policyClaim:

		return !failure.headersSent
	}

	return !failure.requestSent
}

func (p retryPolicy) retriesStatus(status int) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	switch p {
	case policyRead:
		switch status {
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	case policyUpload:
		switch status {
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
			http.StatusGatewayTimeout, http.StatusInsufficientStorage:
			return true
		}
	}
	return false
}

type request struct {
	method string
	url    string
	header http.Header

	body     *payload
	maxBytes int64
	policy   retryPolicy

	readFloor time.Duration

	validUntil  time.Time
	linkExpired bool

	charges bool

	target string
}

type payload struct {
	size int64
	open func() (io.ReadCloser, error)
}

func bytesPayload(data []byte) *payload {
	return &payload{
		size: int64(len(data)),
		open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
	}
}

type response struct {
	status   int
	header   http.Header
	body     []byte
	tooLarge bool
}

type transportFailure struct {
	requestSent bool
	headersSent bool
	timedOut    bool

	cancelled error

	fatal error
}

func (c *Client) send(ctx context.Context, req *request) (*response, error) {
	retries, incomplete := 0, 0
	for {
		resp, failure := c.attempt(ctx, req)
		var delay time.Duration
		switch {
		case failure != nil:
			if failure.fatal != nil {
				return nil, failure.fatal
			}
			if failure.cancelled != nil {
				return nil, failure.cancelled
			}
			if !req.policy.retriesFailure(failure) || retries >= c.maxRetries {
				return nil, connectionError(failure, req)
			}
			delay = backoff(retries, 0, false)
			retries++
		case req.policy == policyClaim && resp.status == http.StatusConflict && errorCode(resp.body) == "upload_incomplete":

			incomplete++
			if incomplete >= uploadIncompleteTries {
				return resp, nil
			}
			if hint, ok := retryAfterOf(resp); ok {
				delay = backoff(0, hint, true)
			} else {
				delay = uploadIncompletePause * time.Duration(1<<(incomplete-1))
			}
		case req.policy.retriesStatus(resp.status) && retries < c.maxRetries:
			hint, ok := retryAfterOf(resp)
			delay = backoff(retries, hint, ok)
			retries++
		default:
			return resp, nil
		}

		if !req.validUntil.IsZero() && time.Now().Add(delay).After(req.validUntil) {

			req.linkExpired = true
			if failure != nil {
				return nil, connectionError(failure, req)
			}
			return resp, nil
		}
		if err := sleepContext(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func (c *Client) attempt(ctx context.Context, req *request) (*response, *transportFailure) {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wait := max(c.timeout, req.readFloor)
	initial := wait
	if req.body != nil {
		initial = c.timeout
	}
	dog := startWatchdog(initial, cancel)
	defer dog.stop()

	var body *progressReader
	var reader io.Reader
	if req.body != nil {
		opened, err := req.body.open()
		if err != nil {
			return nil, &transportFailure{fatal: err}
		}
		body = &progressReader{source: opened, dog: dog, idle: c.timeout}
		defer body.Close()
		reader = body
	}

	var wrote, wroteHeaders atomic.Bool
	trace := &httptrace.ClientTrace{
		WroteHeaders: func() { wroteHeaders.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) {
			wrote.Store(true)
			dog.reset(wait)
		},
	}
	httpReq, err := http.NewRequestWithContext(httptrace.WithClientTrace(attemptCtx, trace), req.method, req.url, reader)
	if err != nil {

		return nil, &transportFailure{fatal: wrapf(ErrInvalidRequest, "the request URL is malformed")}
	}
	httpReq.Header = req.header
	if req.body != nil {
		httpReq.ContentLength = req.body.size
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &transportFailure{cancelled: fmt.Errorf("pixlub: request cancelled: %w", ctxErr)}
		}
		if body != nil {
			if inputErr := body.inputError(); inputErr != nil {
				return nil, &transportFailure{fatal: inputErr}
			}
		}
		return nil, classify(err, wrote.Load(), wroteHeaders.Load(), dog.expired())
	}
	defer resp.Body.Close()

	data, tooLarge, err := readLimited(resp.Body, req.maxBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &transportFailure{cancelled: fmt.Errorf("pixlub: request cancelled: %w", ctxErr)}
		}

		return nil, classify(err, true, true, dog.expired())
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: data, tooLarge: tooLarge}, nil
}

type watchdog struct {
	mu    sync.Mutex
	timer *time.Timer
	fired bool
}

func startWatchdog(d time.Duration, cancel context.CancelFunc) *watchdog {
	dog := &watchdog{}
	dog.mu.Lock()
	defer dog.mu.Unlock()
	dog.timer = time.AfterFunc(d, func() {
		dog.mu.Lock()
		dog.fired = true
		dog.mu.Unlock()
		cancel()
	})
	return dog
}

func (w *watchdog) reset(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.fired {
		w.timer.Reset(d)
	}
}

func (w *watchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer.Stop()
}

func (w *watchdog) expired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

type progressReader struct {
	source io.ReadCloser
	dog    *watchdog
	idle   time.Duration
	once   sync.Once
	mu     sync.Mutex
	failed error
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if n > 0 {
		r.dog.reset(r.idle)
	}
	if err != nil && err != io.EOF {
		var inputErr *inputReadError
		if errors.As(err, &inputErr) {
			r.mu.Lock()
			r.failed = inputErr
			r.mu.Unlock()
		}
	}
	return n, err
}

func (r *progressReader) Close() error {
	var err error
	r.once.Do(func() { err = r.source.Close() })
	return err
}

func (r *progressReader) inputError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

func classify(err error, requestSent, headersSent, timedOut bool) *transportFailure {
	return &transportFailure{
		requestSent: requestSent,
		headersSent: headersSent || requestSent,
		timedOut:    timedOut || isTimeout(err),
	}
}

func connectionError(failure *transportFailure, req *request) error {
	target, advice := req.target, " Check your network."
	if target == "" {
		target, advice = "the Pixlub API", " Check the base URL and your network."
	}
	hint := ""
	if failure.requestSent && req.charges {
		hint = " The request may have reached the server: check your credit balance before retrying a tool run."
	}
	if failure.timedOut {
		return &ConnectionError{
			Message:     target + " did not answer in time." + hint,
			RequestSent: failure.requestSent,
			kind:        ErrTimeout,
		}
	}
	return &ConnectionError{
		Message:     "could not reach " + target + "." + advice + hint,
		RequestSent: failure.requestSent,
		kind:        ErrConnection,
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func readLimited(body io.Reader, limit int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return nil, true, nil
	}
	return data, false, nil
}

func backoff(attempt int, retryAfter time.Duration, hasRetryAfter bool) time.Duration {
	if hasRetryAfter {
		delay := min(max(retryAfter, 0), maxRetryAfter)

		return delay + time.Duration(rand.Float64()*0.25*float64(delay))
	}
	ceiling := min(maxBackoff, initialBackoff*time.Duration(1<<attempt))

	return time.Duration(float64(ceiling) * (0.5 + rand.Float64()*0.5))
}

func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(time.Until(when), 0), true
}

type errorPeek struct {
	Code       string   `json:"code"`
	RetryAfter *float64 `json:"retry_after"`
}

func peekError(body []byte) errorPeek {
	var peek errorPeek
	if len(body) <= maxErrorBody {
		_ = json.Unmarshal(body, &peek)
	}
	return peek
}

func errorCode(body []byte) string { return peekError(body).Code }

func retryAfterOf(resp *response) (time.Duration, bool) {
	if wait, ok := parseRetryAfter(resp.header.Get("Retry-After")); ok {
		return wait, true
	}
	if seconds := peekError(resp.body).RetryAfter; seconds != nil && *seconds >= 0 {
		return time.Duration(min(*seconds, maxRetryAfter.Seconds()) * float64(time.Second)), true
	}
	return 0, false
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("pixlub: request cancelled: %w", ctx.Err())
	}
}

func (c *Client) apiHeader(accept string) http.Header {
	header := make(http.Header)
	header.Set("X-API-Key", c.apiKey)
	header.Set("User-Agent", userAgent)
	header.Set("Accept", accept)
	return header
}

func linkHeader() http.Header {
	header := make(http.Header)
	header.Set("User-Agent", userAgent)
	header.Set("Accept", "*/*")
	return header
}

func (c *Client) getJSON(ctx context.Context, path string, limit int64) ([]byte, error) {
	resp, err := c.send(ctx, &request{
		method:   http.MethodGet,
		url:      c.baseURL + path,
		header:   c.apiHeader("application/json"),
		maxBytes: limit,
		policy:   policyRead,
	})
	if err != nil {
		return nil, err
	}
	if err := checkStatus(resp, c.apiKey); err != nil {
		return nil, err
	}
	if resp.tooLarge {
		return nil, wrapf(ErrUnexpectedResponse, "the API returned more data than the SDK accepts")
	}
	return resp.body, nil
}

func checkStatus(resp *response, apiKey string) error {
	switch {
	case resp.status >= 200 && resp.status < 300:
		return nil
	case resp.status >= 300 && resp.status < 400:
		return wrapf(ErrUnexpectedResponse,
			"the API answered with a redirect (HTTP %d), which the SDK does not follow", resp.status)
	}
	return apiErrorFromResponse(resp, apiKey)
}
