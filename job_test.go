package pixlub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testJobID = "3c9e5a20-7f41-4b8e-9d2a-6a51f0b4c8e7"

func TestRunFollowsALongJob(t *testing.T) {
	override(t, &jobPollPause, time.Millisecond)
	f := newFake(t)
	f.standardRoutes(t)
	f.acceptRun("removeBgApi")
	f.route("GET /v1/jobs/"+testJobID, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != testKey || r.Header.Get("Prefer") != "wait=25" {
			http.Error(w, `{"error":"bad follow-up"}`, http.StatusBadRequest)
			return
		}
		if f.count("GET /v1/jobs/"+testJobID) < 2 {
			fmt.Fprintf(w, `{"job_id":%q,"module":"removeBgApi","status":"running","created_at":"2030-01-01T00:00:00Z"}`, testJobID)
			return
		}
		answer := f.runAnswer()
		fmt.Fprintf(w, `{"job_id":%q,"status":"succeeded","created_at":"2030-01-01T00:00:00Z",`+
			`"finished_at":"2030-01-01T00:01:00Z",%s`, testJobID, answer[1:])
	})
	client := newClientFor(t, f)

	result, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("photo.png", pngBytes)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(result.Image()) != string(pngBytes) || result.ID != "run-1" || result.JobID != testJobID {
		t.Errorf("result = %+v", result)
	}
	if got := f.count("GET /v1/jobs/" + testJobID); got != 2 {
		t.Errorf("follow-ups = %d, want 2", got)
	}
}

func TestRunReportsAFailedJob(t *testing.T) {
	override(t, &jobPollPause, time.Millisecond)
	f := newFake(t)
	f.standardRoutes(t)
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"job_id":%q,"status":"running"}`, testJobID)
	})
	f.route("GET /v1/jobs/"+testJobID, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"job_id":%q,"module":"removeBgApi","status":"failed","created_at":"2030-01-01T00:00:00Z",`+
			`"finished_at":"2030-01-01T00:01:00Z","http_status":402,`+
			`"error":"not enough credits for this module","code":"insufficient_credits","required":10}`, testJobID)
	})
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("photo.png", pngBytes)})
	var apiErr *APIError
	if !errors.Is(err, ErrInsufficientCredits) || !errors.As(err, &apiErr) ||
		apiErr.HTTPStatus != 402 || apiErr.Code != "insufficient_credits" || apiErr.RequiredCredits == nil || *apiErr.RequiredCredits != 10 {
		t.Fatalf("Run error = %#v", err)
	}
	if apiErr.JobID != testJobID || !strings.Contains(err.Error(), testJobID) {
		t.Errorf("the error of a failed job lacks its id: %v", err)
	}
}

func TestRunAcceptedWithoutJob(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"running"}`)
	})
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("photo.png", pngBytes)})
	if !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("Run error = %v, want ErrUnexpectedResponse", err)
	}
}

func TestJobFollowUpStopsWithTheContext(t *testing.T) {
	override(t, &jobPollPause, time.Millisecond)
	f := newFake(t)
	f.standardRoutes(t)
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"job_id":%q,"status":"running"}`, testJobID)
	})
	f.route("GET /v1/jobs/"+testJobID, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"job_id":%q,"status":"running"}`, testJobID)
	})
	client := newClientFor(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := client.Tools.Run(ctx, "removeBgApi", RunRequest{Image: FromBytes("photo.png", pngBytes)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want the deadline", err)
	}
}

func (f *fake) acceptRun(module string) {
	f.route("POST /v1/modules/"+module, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"job_id":%[1]q,"module":%[2]q,"status":"running","poll":"%[3]s/v1/jobs/%[1]s"}`,
			testJobID, module, f.srv.URL)
	})
}

func TestJobTimeoutStopsTheFollowUp(t *testing.T) {
	override(t, &jobPollPause, time.Millisecond)
	f := newFake(t)
	f.standardRoutes(t)
	f.acceptRun("removeBgApi")
	f.route("GET /v1/jobs/"+testJobID, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"job_id":%q,"status":"queued","created_at":"2030-01-01T00:00:00Z"}`, testJobID)
	})
	client := newClientFor(t, f, WithJobTimeout(150*time.Millisecond))

	start := time.Now()
	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("photo.png", pngBytes)})
	if !errors.Is(err, ErrJobTimeout) || !errors.Is(err, ErrTimeout) {
		t.Fatalf("Run error = %v, want ErrJobTimeout", err)
	}
	if !strings.Contains(err.Error(), testJobID) || !strings.Contains(err.Error(), "may have been charged") {
		t.Errorf("error = %v, want the job id and the charge warning", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the job timeout took %v", elapsed)
	}
	if _, err := NewClient(WithAPIKey(testKey), WithJobTimeout(0)); !errors.Is(err, ErrConfiguration) {
		t.Errorf("a zero job timeout = %v, want ErrConfiguration", err)
	}
}

func TestFollowUpErrorsCarryTheJobID(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusUnauthorized, `{"error":"API key required"}`, ErrAuthentication},
		{http.StatusNotFound, `{"error":"Unknown job","code":"job_not_found"}`, ErrNotFound},
		{http.StatusServiceUnavailable, `{"error":"Job tracking unavailable"}`, ErrServer},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := newFake(t)
			f.standardRoutes(t)
			f.acceptRun("removeBgApi")
			f.route("GET /v1/jobs/"+testJobID, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			client := newClientFor(t, f, WithMaxRetries(0))

			_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("photo.png", pngBytes)})
			var apiErr *APIError
			if !errors.Is(err, tc.want) || !errors.As(err, &apiErr) || apiErr.HTTPStatus != tc.status {
				t.Fatalf("Run error = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), testJobID) || !strings.Contains(err.Error(), "may have been charged") {
				t.Errorf("error = %v, want the job id and the charge warning", err)
			}
		})
	}
}

func TestDownloadErrorAfterAJobNamesBothIDs(t *testing.T) {
	override(t, &jobPollPause, time.Millisecond)
	f := newFake(t)
	f.standardRoutes(t)
	f.acceptRun("removeBgApi")
	f.route("GET /v1/jobs/"+testJobID, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"job_id":%q,"status":"succeeded",%s`, testJobID, f.runAnswer()[1:])
	})
	f.files.route("GET /r/run-1_result.png", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"Link expired"}`, http.StatusForbidden)
	})
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("photo.png", pngBytes)})
	if !errors.Is(err, ErrPermissionDenied) || !strings.Contains(err.Error(), "run id run-1, job id "+testJobID) {
		t.Fatalf("Run error = %v, want the 403 with both ids", err)
	}
}

func TestReadTimeoutFloorsHoldWhateverTheTimeout(t *testing.T) {
	override(t, &runReadFloor, 2*time.Second)
	override(t, &pollReadFloor, 2*time.Second)
	override(t, &jobPollPause, time.Millisecond)
	f := newFake(t)
	f.standardRoutes(t)
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"job_id":%q,"module":"removeBgApi","status":"running"}`, testJobID)
	})
	f.route("GET /v1/jobs/"+testJobID, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprintf(w, `{"job_id":%q,"status":"succeeded",%s`, testJobID, f.runAnswer()[1:])
	})
	client := newClientFor(t, f, WithTimeout(100*time.Millisecond), WithMaxRetries(0))

	if _, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("photo.png", pngBytes)}); err != nil {
		t.Fatalf("Run with a short timeout: %v", err)
	}

	f.route("GET /v1/modules/flip/schema", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{}`)
	})
	if _, err := client.Tools.Schema(context.Background(), "flip"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Schema error = %v, want ErrTimeout", err)
	}
}
