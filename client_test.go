package pixlub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const catalog = `[
 {"id":"removeBgApi","name":"Remove background","description":"","category":"filter","executable":true,
  "handler":"remote","inputs":[{"id":"image","label":"Image","description":"","required":true,"type":"image",
   "multiple":false,"per_file":false,"max_files":1,"accept":["image/png","image/jpeg"]}],
  "outputs":[{"key":"out","label":"Image","description":"","kind":"image","multiple":false,"content_types":["image/png"]}]},
 {"id":"mask","name":"Mask","description":"","category":"compose","executable":true,"handler":"local",
  "inputs":[{"id":"image","label":"Image","required":true,"type":"image","multiple":false,"max_files":1},
            {"id":"mask","label":"Mask","required":true,"type":"image","multiple":false,"max_files":1}],
  "outputs":[{"key":"out","label":"Image","kind":"image","multiple":false,"content_types":["image/png"]}]},
 {"id":"collage","name":"Collage","description":"","category":"compose","executable":true,"handler":"local",
  "inputs":[{"id":"images","label":"Images","description":"The photos","required":true,"type":"image",
   "multiple":true,"per_file":false,"max_files":3,"accept":["image/png"]},
            {"id":"background","label":"Background","required":false,"type":"image","multiple":false,"max_files":1}],
  "outputs":[{"key":"out","label":"Collage","kind":"image","multiple":false,"content_types":["image/png"]},
             {"key":"layout","label":"Layout","kind":"data","multiple":false,"content_types":["application/json"]}]},
 {"id":"batchFlip","name":"Flip in batch","description":"","category":"filter","executable":true,"handler":"local",
  "inputs":[{"id":"image","label":"Images","required":true,"type":"batch","multiple":true,"per_file":true,"max_files":64}],
  "outputs":[{"key":"out","label":"Image","kind":"image","multiple":true,"content_types":["image/png"]}]},
 {"id":"editor","name":"Editor","description":"","category":"video","executable":false,"handler":"none","inputs":[],"outputs":[]}
]`

type fake struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hits   map[string]int
	routes map[string]http.HandlerFunc

	files *fake
}

func override[T any](t *testing.T, variable *T, value T) {
	t.Helper()
	saved := *variable
	*variable = value
	t.Cleanup(func() { *variable = saved })
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{hits: map[string]int{}, routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		f.hits[key]++
		handler := f.routes[key]
		f.mu.Unlock()
		if handler == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) route(key string, handler http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[key] = handler
}

func (f *fake) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[key]
}

func (f *fake) standardRoutes(t *testing.T) {
	t.Helper()
	f.files = newFake(t)
	f.route("GET /v1/modules", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != testKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, catalog)
	})
	f.route("GET /v1/modules/flip/schema", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"type":"object","properties":{"axis":{"enum":["h","v","both"]}}}`)
	})
	f.files.route("GET /r/run-1_result.png", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			http.Error(w, "the key must not reach result links", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	})
	f.files.route("GET /r/run-1_result.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"faces":[]}`)
	})
}

func (f *fake) runAnswer() string {
	return fmt.Sprintf(`{"id":"run-1","module":"removeBgApi","outputs":[`+
		`{"key":"out","name":"image","url":"%[1]s/r/run-1_result.png?exp=1&sig=s","content_type":"image/png","bytes":72},`+
		`{"key":"faces","name":"data","url":"%[1]s/r/run-1_result.json?exp=1&sig=s","content_type":"application/json","bytes":12}],`+
		`"expires_at":"2030-01-01T00:00:00Z","credits":{"cost":10,"balance":190}}`, f.files.srv.URL)
}

func newClientFor(t *testing.T, f *fake, opts ...Option) *Client {
	t.Helper()
	all := append([]Option{WithAPIKey(testKey), WithBaseURL(f.srv.URL)}, opts...)
	client, err := NewClient(all...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func TestRunDownloadsEveryOutput(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != testKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, f.runAnswer())
	})
	client := newClientFor(t, f)

	result, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{
		Image: FromBytes("photo.png", pngBytes),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(result.Image()) != string(pngBytes) {
		t.Errorf("Image() does not match the downloaded PNG")
	}
	var data struct {
		Faces []any `json:"faces"`
	}
	if err := result.Data(&data); err != nil || data.Faces == nil {
		t.Errorf("Data() = %+v, %v", data, err)
	}
	if result.Cost == nil || *result.Cost != 10 || result.CreditsBalance == nil || *result.CreditsBalance != 190 {
		t.Errorf("credits = %v / %v", result.Cost, result.CreditsBalance)
	}
	if result.ExpiresAt.Year() != 2030 || result.Module != "removeBgApi" || result.ID != "run-1" {
		t.Errorf("result metadata = %+v", result)
	}
	if got := result.File().Extension(); got != ".png" {
		t.Errorf("extension = %q", got)
	}
}

func TestRunSendsSettingsAndTypedImage(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	var gotParams, gotName, gotType string
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		gotParams = r.MultipartForm.Value["params"][0]
		header := r.MultipartForm.File["image"][0]
		gotName = header.Filename
		gotType = header.Header.Get("Content-Type")
		_, _ = io.WriteString(w, f.runAnswer())
	})
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{
		Image:  FromBytes("photo.png", pngBytes),
		Params: map[string]any{"axis": "v"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotParams != `{"axis":"v"}` || gotName != "photo.png" || gotType != "image/png" {
		t.Errorf("params=%q name=%q type=%q", gotParams, gotName, gotType)
	}
}

func TestRunRedirectIsNotFollowed(t *testing.T) {
	elsewhere := newFake(t)
	elsewhere.route("GET /v1/modules", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, catalog)
	})

	f := newFake(t)
	f.route("GET /v1/modules", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+"/v1/modules", http.StatusFound)
	})
	client := newClientFor(t, f)

	_, err := client.Tools.List(context.Background(), ListOptions{})
	if !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("List error = %v, want ErrUnexpectedResponse", err)
	}
	if elsewhere.count("GET /v1/modules") != 0 {
		t.Errorf("the redirect was followed: the key reached another host")
	}
}

func TestRunPostIsNotRetriedOnServerError(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	})
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("a.png", pngBytes)})
	if !errors.Is(err, ErrServer) {
		t.Fatalf("Run error = %v, want ErrServer", err)
	}
	if n := f.count("POST /v1/modules/removeBgApi"); n != 1 {
		t.Errorf("a run answered with 500 was sent %d times, want 1 (a retry could charge twice)", n)
	}
}

func TestRunPostIsRetriedOnRateLimit(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	calls := 0
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":"slow down"}`, http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, f.runAnswer())
	})
	client := newClientFor(t, f)

	if _, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("a.png", pngBytes)}); err != nil {
		t.Fatalf("Run after 429: %v", err)
	}
	if n := f.count("POST /v1/modules/removeBgApi"); n != 2 {
		t.Errorf("POST attempts = %d, want 2", n)
	}
}

func TestReadRoutesAreRetriedOnUnavailable(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	calls := 0
	f.route("GET /v1/modules", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, `{"error":"busy"}`, http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, catalog)
	})
	client := newClientFor(t, f)

	tools, err := client.Tools.List(context.Background(), ListOptions{})
	if err != nil || len(tools) != 5 {
		t.Fatalf("List = %d tools, %v", len(tools), err)
	}
	if calls != 2 {
		t.Errorf("GET attempts = %d, want 2", calls)
	}
}

func TestRunRefusesBadInputBeforeSending(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "mask", RunRequest{
		Inputs: map[string]Source{"base": FromBytes("a.png", pngBytes)},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Run error = %v, want ErrInvalidInput", err)
	}
	if n := f.count("POST /v1/modules/mask"); n != 0 {
		t.Errorf("a refused run was sent %d times, want 0", n)
	}

	if _, err := client.Tools.Run(context.Background(), "../internal", RunRequest{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("path-like id error = %v, want ErrInvalidInput", err)
	}
}

func TestRefusedConnectionIsNotSentAndIsRetried(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	base := http.DefaultTransport.(*http.Transport).Clone()
	var posts int
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			posts++
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		}
		return base.RoundTrip(req)
	})
	client := newClientFor(t, f, WithHTTPClient(&http.Client{Transport: transport}))

	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("a.png", pngBytes)})
	var connErr *ConnectionError
	if !errors.As(err, &connErr) || connErr.RequestSent {
		t.Fatalf("Run error = %v, want a ConnectionError with RequestSent=false", err)
	}
	if !errors.Is(err, ErrConnection) {
		t.Errorf("error does not match ErrConnection")
	}
	if posts != 1+defaultMaxRetries {
		t.Errorf("POST attempts = %d, want %d (refused connections are safe to retry)", posts, 1+defaultMaxRetries)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRunTimeoutAfterSendingIsNotRetried(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, f.runAnswer())
	})

	override(t, &runReadFloor, 0)
	client := newClientFor(t, f, WithTimeout(100*time.Millisecond))

	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("a.png", pngBytes)})
	var connErr *ConnectionError
	if !errors.Is(err, ErrTimeout) || !errors.As(err, &connErr) || !connErr.RequestSent {
		t.Fatalf("Run error = %v, want a timeout with RequestSent=true", err)
	}
	if n := f.count("POST /v1/modules/removeBgApi"); n != 1 {
		t.Errorf("a timed-out run was sent %d times, want 1", n)
	}
}

func TestAPIErrorsCarryTheirDetails(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
		check  func(*testing.T, *APIError)
	}{
		{"credits", 402, `{"error":"Not enough credits","code":"insufficient_credits","required":10}`,
			ErrInsufficientCredits, func(t *testing.T, e *APIError) {
				if e.RequiredCredits == nil || *e.RequiredCredits != 10 {
					t.Errorf("RequiredCredits = %v, want 10", e.RequiredCredits)
				}
			}},
		{"settings", 400, `{"error":"Invalid parameters","details":[{"keyword":"enum"}]}`,
			ErrInvalidRequest, func(t *testing.T, e *APIError) {
				if e.Details == nil {
					t.Errorf("Details is nil, want the validation details")
				}
			}},
		{"plan", 403, `{"error":"Free plan","code":"plan_required"}`,
			ErrPermissionDenied, func(t *testing.T, e *APIError) {
				if e.Code != "plan_required" {
					t.Errorf("Code = %q", e.Code)
				}
			}},
		{"unknown key", 401, `{"message":"Invalid key"}`, ErrAuthentication, func(t *testing.T, e *APIError) {
			if e.Message != "Invalid key" {
				t.Errorf("Message = %q (message must win over error)", e.Message)
			}
		}},
		{"missing", 404, `{"error":"Unknown module"}`, ErrNotFound, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.standardRoutes(t)
			f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			client := newClientFor(t, f)

			_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("a.png", pngBytes)})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.HTTPStatus != tc.status {
				t.Fatalf("error = %#v, want an *APIError with status %d", err, tc.status)
			}
			if tc.check != nil {
				tc.check(t, apiErr)
			}
		})
	}
}

func TestErrorMessageNeverEchoesTheKey(t *testing.T) {
	f := newFake(t)
	f.route("GET /v1/modules", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unknown key `+testKey+`"}`, http.StatusUnauthorized)
	})
	client := newClientFor(t, f)

	_, err := client.Tools.List(context.Background(), ListOptions{})
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("error leaks the key: %v", err)
	}
	if !strings.Contains(err.Error(), "pix.AbCdEf12.***") {
		t.Errorf("error lacks the masked key: %v", err)
	}
}

func TestResultLinkOutsideTheApiIsRefused(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"x","module":"removeBgApi","outputs":[`+
			`{"name":"image","url":"http://evil.example/steal.png","content_type":"image/png"}]}`)
	})
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("a.png", pngBytes)})
	if !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("Run error = %v, want ErrUnexpectedResponse", err)
	}
}

func TestListRetrieveAndSchema(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	client := newClientFor(t, f)
	ctx := context.Background()

	executable, err := client.Tools.List(ctx, ListOptions{ExecutableOnly: true})
	if err != nil || len(executable) != 4 {
		t.Fatalf("executable tools = %d, %v", len(executable), err)
	}

	tool, err := client.Tools.Retrieve(ctx, "mask")
	if err != nil || len(tool.Inputs) != 2 || tool.Inputs[1].ID != "mask" {
		t.Fatalf("Retrieve(mask) = %+v, %v", tool, err)
	}
	if _, err := client.Tools.Retrieve(ctx, "nope"); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("Retrieve(nope) error = %v, want ErrUnknownTool", err)
	}

	schema, err := client.Tools.Schema(ctx, "flip")
	if err != nil || schema["type"] != "object" {
		t.Fatalf("Schema = %v, %v", schema, err)
	}
}

func TestSaveRefusesToReplaceAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.png")
	output := Output{Content: pngBytes}

	if err := output.Save(path, false); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := output.Save(path, false); !errors.Is(err, os.ErrExist) {
		t.Errorf("second save error = %v, want os.ErrExist", err)
	}
	if err := output.Save(path, true); err != nil {
		t.Errorf("save with overwrite: %v", err)
	}
}

func TestReadLimitedStopsAtTheLimit(t *testing.T) {
	data, tooLarge, err := readLimited(strings.NewReader("abcdef"), 3)
	if err != nil || !tooLarge || data != nil {
		t.Errorf("readLimited over the limit = %q, %v, %v", data, tooLarge, err)
	}
	data, tooLarge, err = readLimited(strings.NewReader("abc"), 3)
	if err != nil || tooLarge || string(data) != "abc" {
		t.Errorf("readLimited at the limit = %q, %v, %v", data, tooLarge, err)
	}
}

func TestBackoffAndRetryRules(t *testing.T) {
	if got := backoff(0, 0, true); got != 0 {
		t.Errorf("Retry-After: 0 gave %v, want 0", got)
	}

	if got := backoff(0, 120*time.Second, true); got < maxRetryAfter || got > maxRetryAfter+maxRetryAfter/4 {
		t.Errorf("long Retry-After gave %v, want about the cap %v", got, maxRetryAfter)
	}
	for attempt := 0; attempt < 6; attempt++ {
		got := backoff(attempt, 0, false)
		if got <= 0 || got > maxBackoff {
			t.Errorf("backoff(%d) = %v, outside (0, %v]", attempt, got, maxBackoff)
		}
	}

	if policyCreate.retriesStatus(http.StatusInternalServerError) || policyClaim.retriesStatus(http.StatusServiceUnavailable) {
		t.Errorf("a POST answered with 5xx must not be retried")
	}
	if !policyCreate.retriesStatus(http.StatusTooManyRequests) || !policyClaim.retriesStatus(http.StatusTooManyRequests) {
		t.Errorf("a POST answered with 429 must be retried")
	}
	if !policyRead.retriesStatus(http.StatusBadGateway) || policyRead.retriesStatus(http.StatusInsufficientStorage) {
		t.Errorf("a GET is retried on 502, not on 507")
	}
	if !policyUpload.retriesStatus(http.StatusInsufficientStorage) || policyUpload.retriesStatus(http.StatusForbidden) {
		t.Errorf("an upload is retried on 507, not on 403 (expired link)")
	}
	if policyCreate.retriesFailure(&transportFailure{requestSent: true}) || !policyCreate.retriesFailure(&transportFailure{}) {
		t.Errorf("a POST is retried only when it was not sent")
	}
	if policyClaim.retriesFailure(&transportFailure{headersSent: true}) || !policyClaim.retriesFailure(&transportFailure{}) {
		t.Errorf("a run on an upload is retried only when not a byte of it was sent")
	}
	if !policyUpload.retriesFailure(&transportFailure{requestSent: true}) {
		t.Errorf("an upload is retried after a network error")
	}

	if d, ok := parseRetryAfter("7"); !ok || d != 7*time.Second {
		t.Errorf("parseRetryAfter(7) = %v, %v", d, ok)
	}
	if _, ok := parseRetryAfter("not a date"); ok {
		t.Errorf("parseRetryAfter accepted garbage")
	}
}

func TestDownloadsAreRetriedButNeverCarryTheKey(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	calls := 0
	f.files.route("GET /r/run-1_result.png", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "busy", http.StatusBadGateway)
			return
		}
		_, _ = w.Write(pngBytes)
	})
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, f.runAnswer())
	})
	client := newClientFor(t, f)

	result, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("a.png", pngBytes)})
	if err != nil || string(result.Image()) != string(pngBytes) {
		t.Fatalf("Run with a flaky download: %v", err)
	}
	if calls != 2 {
		t.Errorf("download attempts = %d, want 2", calls)
	}
}

func TestContextCancellationStopsTheCall(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	f.route("GET /v1/modules", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, catalog)
	})
	client := newClientFor(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := client.Tools.List(ctx, ListOptions{})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("List with a short deadline = %v, want context.DeadlineExceeded", err)
	}
}

func TestRunWithInputsFromAReader(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	var gotName string
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		gotName = r.MultipartForm.File["image"][0].Filename
		_, _ = io.WriteString(w, f.runAnswer())
	})
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{
		Image: FromReader("stream.png", strings.NewReader(string(pngBytes))),
	})
	if err != nil {
		t.Fatalf("Run from a reader: %v", err)
	}
	if gotName != "stream.png" {
		t.Errorf("file name = %q", gotName)
	}
}

func TestCatalogDescribesInputsAndOutputs(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	client := newClientFor(t, f)

	tool, err := client.Tools.Retrieve(context.Background(), "collage")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	images := tool.Inputs[0]
	if images.ID != "images" || !images.Multiple || images.PerFile || images.MaxFiles != 3 ||
		images.Description != "The photos" || len(images.Accept) != 1 || images.Accept[0] != "image/png" {
		t.Errorf("input = %+v", images)
	}
	if len(tool.Outputs) != 2 || tool.Outputs[1].Key != "layout" || tool.Outputs[1].Kind != "data" ||
		tool.Outputs[1].ContentTypes[0] != "application/json" {
		t.Errorf("outputs = %+v", tool.Outputs)
	}
	batch, _ := client.Tools.Retrieve(context.Background(), "batchFlip")
	if !batch.Inputs[0].PerFile || batch.Inputs[0].MaxFiles != 64 || !batch.Outputs[0].Multiple {
		t.Errorf("batch tool = %+v", batch)
	}

	tool.Inputs[0].Accept[0] = "changed"
	tool.Outputs[0].ContentTypes[0] = "changed"
	again, _ := client.Tools.Retrieve(context.Background(), "collage")
	if again.Inputs[0].Accept[0] != "image/png" || again.Outputs[0].ContentTypes[0] != "image/png" {
		t.Errorf("changing a tool changed the cache: %+v", again)
	}
}

func TestRunSendsSeveralFilesForOneInput(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	var names []string
	var background int
	f.route("POST /v1/modules/collage", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		for _, header := range r.MultipartForm.File["images"] {
			names = append(names, header.Filename)
		}
		background = len(r.MultipartForm.File["background"])
		_, _ = io.WriteString(w, f.runAnswer())
	})
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "collage", RunRequest{
		Inputs: map[string]Source{"background": FromBytes("bg.png", pngBytes)},
		InputLists: map[string][]Source{
			"images": {FromBytes("a.png", pngBytes), FromBytes("b.png", pngBytes), FromBytes("c.png", pngBytes)},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Join(names, ",") != "a.png,b.png,c.png" || background != 1 {
		t.Errorf("images = %v, background = %d", names, background)
	}
}

func TestAPIErrorDetailsOfTheContract(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header map[string]string
		body   string
		want   error
		check  func(*testing.T, *APIError)
	}{
		{"file too large", 413, nil, `{"error":"File too large","code":"file_too_large","max_bytes":10485760}`,
			ErrFileTooLarge, func(t *testing.T, e *APIError) {
				if e.MaxBytes == nil || *e.MaxBytes != 10485760 || !errors.Is(e, ErrInvalidRequest) {
					t.Errorf("MaxBytes = %v", e.MaxBytes)
				}
			}},
		{"too many pixels", 422, nil, `{"error":"Image too large","code":"image_too_large","max_pixels":40000000}`,
			ErrInvalidRequest, func(t *testing.T, e *APIError) {
				if e.MaxPixels == nil || *e.MaxPixels != 40000000 || errors.Is(e, ErrFileTooLarge) {
					t.Errorf("MaxPixels = %v", e.MaxPixels)
				}
			}},
		{"batch limit", 403, nil, `{"error":"Batch too large","code":"batch_limit","limit":10}`,
			ErrPermissionDenied, func(t *testing.T, e *APIError) {
				if e.Limit == nil || *e.Limit != 10 {
					t.Errorf("Limit = %v", e.Limit)
				}
			}},
		{"daily quota", 429, nil, `{"error":"Daily quota reached","code":"quota","limit":5,"reset_at":"2030-01-02T00:00:00Z","retry_after":30}`,
			ErrRateLimited, func(t *testing.T, e *APIError) {
				if e.ResetAt.Year() != 2030 || e.RetryAfter != 30*time.Second || e.Limit == nil || *e.Limit != 5 {
					t.Errorf("ResetAt = %v, RetryAfter = %v, Limit = %v", e.ResetAt, e.RetryAfter, e.Limit)
				}
			}},
		{"busy", 503, map[string]string{"Retry-After": "5"}, `{"error":"Busy","code":"busy"}`,
			ErrServer, func(t *testing.T, e *APIError) {
				if e.RetryAfter != 5*time.Second || e.Code != "busy" {
					t.Errorf("RetryAfter = %v, Code = %q", e.RetryAfter, e.Code)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.standardRoutes(t)
			f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				for key, value := range tc.header {
					w.Header().Set(key, value)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			client := newClientFor(t, f, WithMaxRetries(0))

			_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("a.png", pngBytes)})
			var apiErr *APIError
			if !errors.Is(err, tc.want) || !errors.As(err, &apiErr) || apiErr.HTTPStatus != tc.status {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			tc.check(t, apiErr)
		})
	}
}

func TestRetryAfterFieldPacesTheRetry(t *testing.T) {
	resp := &response{status: 429, header: http.Header{}, body: []byte(`{"code":"rate_limited","retry_after":3}`)}
	if wait, ok := retryAfterOf(resp); !ok || wait != 3*time.Second {
		t.Errorf("retryAfterOf = %v, %v; want 3s", wait, ok)
	}
	resp.header.Set("Retry-After", "1")
	if wait, ok := retryAfterOf(resp); !ok || wait != time.Second {
		t.Errorf("the header must win: %v, %v", wait, ok)
	}
}

func TestResultFindsOutputsByKeyOrName(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, f.runAnswer())
	})
	client := newClientFor(t, f)

	result, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("a.png", pngBytes)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out := result.Output("out"); out == nil || out.Name != "image" || out.Bytes != int64(len(pngBytes)) {
		t.Errorf("Output(out) = %+v", out)
	}
	if out := result.Output("faces"); out == nil || out.Name != "data" {
		t.Errorf("Output(faces) = %+v", out)
	}
	if out := result.Output("data"); out == nil || out.Key != "faces" {
		t.Errorf("Output(data) = %+v", out)
	}
	if result.Output("nope") != nil {
		t.Errorf("Output(nope) found something")
	}
}

func TestResultExtensionFollowsTheProducedFile(t *testing.T) {
	webp := &Result{Outputs: []Output{{Name: "image", ContentType: "image/webp"}, {Name: "data", ContentType: "application/json"}}}
	if got := webp.Extension(); got != ".webp" {
		t.Errorf("Extension() = %q, want .webp", got)
	}
	dataOnly := &Result{Outputs: []Output{{Name: "data", ContentType: "application/json"}}}
	if got := dataOnly.Extension(); got != ".json" {
		t.Errorf("Extension() of a data-only run = %q, want .json", got)
	}
}

func TestRunOutputLimits(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	entries := make([]string, 0, maxRunOutputs)
	for i := 0; i < maxRunOutputs; i++ {
		entries = append(entries, fmt.Sprintf(`{"key":"out#%d","name":"image","url":"%s/r/run-1_result.png?exp=1&sig=s","content_type":"image/png","bytes":72}`,
			i, f.files.srv.URL))
	}
	f.route("POST /v1/modules/batchFlip", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"run-1","module":"batchFlip","outputs":[%s]}`, strings.Join(entries, ","))
	})
	client := newClientFor(t, f)

	result, err := client.Tools.Run(context.Background(), "batchFlip", RunRequest{Image: FromBytes("a.png", pngBytes)})
	if maxRunOutputs != 256 {
		t.Fatalf("maxRunOutputs = %d, want 256: 64 files per request, 4 outputs per run", maxRunOutputs)
	}
	if err != nil || len(result.Outputs) != maxRunOutputs || result.Output("out#255") == nil {
		t.Fatalf("Run with %d outputs: %v", maxRunOutputs, err)
	}

	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"run-2","module":"removeBgApi","outputs":[{"key":"out","name":"file","url":"%s/r/run-1_result.png","bytes":%d}]}`,
			f.files.srv.URL, maxOutputBytes+1)
	})
	before := f.files.count("GET /r/run-1_result.png")
	_, err = client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("a.png", pngBytes)})
	if !errors.Is(err, ErrUnexpectedResponse) || !strings.Contains(err.Error(), "run-2") {
		t.Fatalf("error = %v, want a charged error with the run id", err)
	}
	if f.files.count("GET /r/run-1_result.png") != before {
		t.Errorf("an output announced too large was downloaded")
	}
}

func TestDefaultsOfTheContract(t *testing.T) {
	t.Setenv(BaseURLEnv, "")
	client, err := NewClient(WithAPIKey(testKey))
	if err != nil {
		t.Fatal(err)
	}
	if client.baseURL != "https://api.pixlub.com" || client.maxInputBytes != 3<<30 || client.jobTimeout != 30*time.Minute {
		t.Errorf("defaults = %s, %d, %v", client.baseURL, client.maxInputBytes, client.jobTimeout)
	}
	if DirectUploadMaxBytes != 94371840 || !strings.HasPrefix(userAgent, "pixlub-go/0.3.1 ") {
		t.Errorf("DirectUploadMaxBytes = %d, User-Agent = %q", DirectUploadMaxBytes, userAgent)
	}
	// Images go up to 1 GiB: so does one output; a run reads at most 4 GiB.
	if maxOutputBytes != 1<<30 || maxRunOutputBytes != 4<<30 {
		t.Errorf("output limits = %d, %d", maxOutputBytes, maxRunOutputBytes)
	}
}
