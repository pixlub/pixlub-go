package pixlub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

const jobWaitSeconds = 25

var jobPollPause = 2 * time.Second

var jobIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type jobState struct {
	JobID      string `json:"job_id"`
	Status     string `json:"status"`
	HTTPStatus int    `json:"http_status"`
}

func (c *Client) followJob(ctx context.Context, accepted *response) (*response, string, error) {
	var started jobState
	if err := json.Unmarshal(accepted.body, &started); err != nil || !jobIDPattern.MatchString(started.JobID) {
		return nil, "", chargedError(runRef{}, "the API accepted it without a job id to follow", nil)
	}
	jobID := started.JobID
	ref := runRef{jobID: jobID}

	jobCtx, cancel := context.WithTimeout(ctx, c.jobTimeout)
	defer cancel()
	stopped := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return chargedError(ref, "the wait for its job was cancelled", err)
		case jobCtx.Err() != nil:
			return chargedError(ref, fmt.Sprintf("its job did not finish within %s (see WithJobTimeout)", c.jobTimeout), ErrJobTimeout)
		}
		return chargedError(ref, "its job could not be followed to the end", err)
	}

	header := c.apiHeader("application/json")
	header.Set("Prefer", "wait="+strconv.Itoa(jobWaitSeconds))
	for {
		asked := time.Now()
		resp, err := c.send(jobCtx, &request{
			method:    http.MethodGet,
			url:       c.baseURL + "/v1/jobs/" + jobID,
			header:    header,
			maxBytes:  maxJSONBytes,
			policy:    policyRead,
			readFloor: pollReadFloor,
		})
		if err != nil {
			return nil, jobID, stopped(err)
		}
		if err := checkStatus(resp, c.apiKey); err != nil {
			return nil, jobID, stopped(err)
		}

		var state jobState
		if err := json.Unmarshal(resp.body, &state); err != nil {
			return nil, jobID, stopped(wrapf(ErrUnexpectedResponse, "the job answer is not a JSON object"))
		}
		switch state.Status {
		case "succeeded":
			return resp, jobID, nil
		case "failed":

			status := state.HTTPStatus
			if status < http.StatusBadRequest {
				status = http.StatusInternalServerError
			}
			failure := newAPIError(&response{status: status, header: resp.header, body: resp.body}, c.apiKey)
			failure.JobID = jobID
			return nil, jobID, failure
		case "queued", "running":
		default:
			return nil, jobID, stopped(wrapf(ErrUnexpectedResponse, "the job answer has an unknown status"))
		}

		if time.Since(asked) < jobPollPause {
			if err := sleepContext(jobCtx, jobPollPause); err != nil {
				return nil, jobID, stopped(err)
			}
		}
	}
}

type runRef struct {
	runID string
	jobID string
}

func (r runRef) String() string {
	switch {
	case r.runID != "" && r.jobID != "":
		return "run id " + r.runID + ", job id " + r.jobID
	case r.runID != "":
		return "run id " + r.runID
	case r.jobID != "":
		return "job id " + r.jobID
	}
	return "its run id is unknown"
}

func chargedError(ref runRef, problem string, cause error) error {
	message := fmt.Sprintf("the run was accepted and may have been charged (%s), but %s: "+
		"check your credit balance, and quote these ids to support, before running the tool again", ref, problem)
	if cause == nil {
		return wrapf(ErrUnexpectedResponse, "%s", message)
	}
	return fmt.Errorf("pixlub: %s: %w", message, cause)
}
