package pixlub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var bigPNG = append(append([]byte(nil), pngBytes...), bytes.Repeat([]byte{7}, 4096)...)

const testDirectLimit = 1024

type uploadLog struct {
	mu     sync.Mutex
	opened []map[string]any
	puts   []putRecord
	claims []claimRecord
}

type putRecord struct {
	path          string
	apiKey        string
	contentLength int64
	chunked       bool
	contentType   string
	body          []byte
}

type claimRecord struct {
	uploadID string
	files    map[string][]string
}

func (f *fake) uploadRoutes(t *testing.T, put func(n int, id string) int) *uploadLog {
	t.Helper()
	log := &uploadLog{}
	f.route("POST /v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != testKey || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, `{"error":"bad upload request"}`, http.StatusBadRequest)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		log.mu.Lock()
		log.opened = append(log.opened, body)
		id := fmt.Sprintf("up-%d", len(log.opened))
		log.mu.Unlock()
		f.files.route("PUT /u/"+id, func(w http.ResponseWriter, r *http.Request) {
			data, _ := io.ReadAll(r.Body)
			log.mu.Lock()
			log.puts = append(log.puts, putRecord{
				path:          r.URL.Path,
				apiKey:        r.Header.Get("X-API-Key"),
				contentLength: r.ContentLength,
				chunked:       len(r.TransferEncoding) > 0,
				contentType:   r.Header.Get("Content-Type"),
				body:          data,
			})
			n := len(log.puts)
			log.mu.Unlock()
			status := http.StatusOK
			if put != nil {
				status = put(n, id)
			}
			switch status {
			case http.StatusOK:
				fmt.Fprintf(w, `{"bytes":%d}`, len(data))
			case http.StatusForbidden:
				http.Error(w, `{"error":"Link expired","code":"invalid_link"}`, status)
			default:
				http.Error(w, `{"error":"upload failed","code":"upload_failed"}`, status)
			}
		})
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"upload_id":%q,"upload_url":"%s/u/%s?exp=1&sig=s","method":"PUT","expires_at":%q}`,
			id, f.files.srv.URL, id, time.Now().Add(15*time.Minute).UTC().Format(time.RFC3339))
	})
	return log
}

func (f *fake) claimRoute(t *testing.T, log *uploadLog, module string, answer func(n int, w http.ResponseWriter) bool) {
	t.Helper()
	f.route("POST /v1/modules/"+module, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, `{"error":"bad multipart"}`, http.StatusBadRequest)
			return
		}
		record := claimRecord{files: map[string][]string{}}
		if values := r.MultipartForm.Value["upload_id"]; len(values) == 1 {
			record.uploadID = values[0]
		}
		for field, headers := range r.MultipartForm.File {
			for _, header := range headers {
				record.files[field] = append(record.files[field], header.Filename)
			}
		}
		log.mu.Lock()
		log.claims = append(log.claims, record)
		n := len(log.claims)
		log.mu.Unlock()
		if answer != nil && answer(n, w) {
			return
		}
		_, _ = io.WriteString(w, f.runAnswer())
	})
}

func TestRunSendsFilesUpToTheLimitDirectly(t *testing.T) {
	override(t, &directUploadLimit, testDirectLimit)
	f := newFake(t)
	f.standardRoutes(t)
	log := f.uploadRoutes(t, nil)
	f.claimRoute(t, log, "mask", nil)
	client := newClientFor(t, f)

	exact := append(append([]byte(nil), pngBytes...), make([]byte, testDirectLimit-2*len(pngBytes))...)
	_, err := client.Tools.Run(context.Background(), "mask", RunRequest{Inputs: map[string]Source{
		"image": FromBytes("a.png", exact),
		"mask":  FromBytes("m.png", pngBytes),
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.count("POST /v1/uploads") != 0 || len(log.claims) != 1 || log.claims[0].uploadID != "" {
		t.Fatalf("files of %d bytes in all must go in the run request: %+v", testDirectLimit, log.claims)
	}
	if got := log.claims[0].files; len(got["image"]) != 1 || len(got["mask"]) != 1 {
		t.Fatalf("files sent = %v", got)
	}
}

func TestRunUploadsALargeMainFile(t *testing.T) {
	override(t, &directUploadLimit, testDirectLimit)
	f := newFake(t)
	f.standardRoutes(t)
	log := f.uploadRoutes(t, nil)
	f.claimRoute(t, log, "mask", nil)
	client := newClientFor(t, f)

	path := filepath.Join(t.TempDir(), "big.png")
	if err := os.WriteFile(path, bigPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := client.Tools.Run(context.Background(), "mask", RunRequest{Inputs: map[string]Source{
		"image": FromPath(path),
		"mask":  FromBytes("m.png", pngBytes),
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(result.Image()) != string(pngBytes) {
		t.Errorf("the result was not downloaded")
	}

	if len(log.opened) != 1 {
		t.Fatalf("uploads opened = %d, want 1", len(log.opened))
	}
	opened := log.opened[0]
	if opened["module"] != "mask" || opened["filename"] != "big.png" || opened["bytes"] != float64(len(bigPNG)) {
		t.Errorf("upload request = %v, want the module, the file name and its exact size", opened)
	}
	if len(log.puts) != 1 {
		t.Fatalf("PUTs = %d, want 1", len(log.puts))
	}
	put := log.puts[0]
	if put.apiKey != "" {
		t.Errorf("the API key reached the upload link")
	}
	if put.contentLength != int64(len(bigPNG)) || put.chunked {
		t.Errorf("Content-Length = %d (chunked %v), want exactly %d", put.contentLength, put.chunked, len(bigPNG))
	}
	if put.contentType != "image/png" || !bytes.Equal(put.body, bigPNG) {
		t.Errorf("PUT type = %q, %d bytes; want the PNG as is", put.contentType, len(put.body))
	}
	if len(log.claims) != 1 || log.claims[0].uploadID != "up-1" {
		t.Fatalf("run requests = %+v, want one with upload_id up-1", log.claims)
	}
	if got := log.claims[0].files; len(got["image"]) != 0 || len(got["mask"]) != 1 {
		t.Errorf("files of the run request = %v, want only the mask", got)
	}
}

func TestQueuedUploadIsFollowedUntilItsLink(t *testing.T) {
	override(t, &directUploadLimit, testDirectLimit)
	override(t, &uploadQueuePause, time.Millisecond)
	f := newFake(t)
	f.standardRoutes(t)
	log := &uploadLog{}
	f.route("POST /v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"upload_id":"up-1","status":"queued","position":2}`)
	})
	var mu sync.Mutex
	var follows []string
	f.route("GET /v1/uploads/up-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		follows = append(follows, r.Header.Get("X-API-Key")+" "+r.Header.Get("Prefer"))
		n := len(follows)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"upload_id":"up-1","status":"queued","position":1}`)
			return
		}
		fmt.Fprintf(w, `{"upload_id":"up-1","status":"ready","upload_url":"%s/u/up-1?exp=1&sig=s","method":"PUT"}`, f.files.srv.URL)
	})
	f.files.route("PUT /u/up-1", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		log.mu.Lock()
		log.puts = append(log.puts, putRecord{apiKey: r.Header.Get("X-API-Key"), body: data})
		log.mu.Unlock()
		fmt.Fprintf(w, `{"bytes":%d}`, len(data))
	})
	f.claimRoute(t, log, "mask", nil)
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "mask", RunRequest{Inputs: map[string]Source{
		"image": FromBytes("big.png", bigPNG),
		"mask":  FromBytes("m.png", pngBytes),
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(follows) != 2 || follows[0] != testKey+" wait=25" {
		t.Fatalf("follow-ups = %q, want 2 with the key and Prefer: wait=25", follows)
	}
	if len(log.puts) != 1 || log.puts[0].apiKey != "" || !bytes.Equal(log.puts[0].body, bigPNG) {
		t.Fatalf("PUTs = %d, want the file once, without the key", len(log.puts))
	}
	if len(log.claims) != 1 || log.claims[0].uploadID != "up-1" {
		t.Fatalf("run requests = %+v, want one with upload_id up-1", log.claims)
	}
}

func TestQueuedUploadThatIsGivenUpFails(t *testing.T) {
	override(t, &directUploadLimit, testDirectLimit)
	override(t, &uploadQueuePause, time.Millisecond)
	f := newFake(t)
	f.standardRoutes(t)
	f.route("POST /v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"upload_id":"up-1","status":"queued","position":1}`)
	})
	f.route("GET /v1/uploads/up-1", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"Unknown upload","code":"upload_not_found"}`, http.StatusNotFound)
	})
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "mask", RunRequest{Inputs: map[string]Source{
		"image": FromBytes("big.png", bigPNG),
		"mask":  FromBytes("m.png", pngBytes),
	}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "upload_not_found" {
		t.Fatalf("Run = %v, want the upload_not_found error", err)
	}
	if f.count("POST /v1/modules/mask") != 0 {
		t.Fatalf("the tool ran without its file")
	}
}

func TestRunRetriesAnInterruptedUpload(t *testing.T) {
	override(t, &directUploadLimit, testDirectLimit)
	f := newFake(t)
	f.standardRoutes(t)
	log := f.uploadRoutes(t, func(n int, _ string) int {
		if n == 1 {
			return http.StatusInsufficientStorage
		}
		return http.StatusOK
	})
	f.claimRoute(t, log, "removeBgApi", nil)
	client := newClientFor(t, f)

	if _, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("big.png", bigPNG)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(log.opened) != 1 || len(log.puts) != 2 {
		t.Fatalf("uploads opened = %d, PUTs = %d; want 1 and 2 (same link)", len(log.opened), len(log.puts))
	}
	if !bytes.Equal(log.puts[1].body, bigPNG) || log.puts[0].path != log.puts[1].path {
		t.Errorf("the retry did not send the same file to the same link")
	}
}

func TestExpiredUploadLinkOpensANewUploadOnce(t *testing.T) {
	override(t, &directUploadLimit, testDirectLimit)
	f := newFake(t)
	f.standardRoutes(t)
	log := f.uploadRoutes(t, func(_ int, id string) int {
		if id == "up-1" {
			return http.StatusForbidden
		}
		return http.StatusOK
	})
	f.claimRoute(t, log, "removeBgApi", nil)
	client := newClientFor(t, f)

	if _, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("big.png", bigPNG)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(log.opened) != 2 || len(log.claims) != 1 || log.claims[0].uploadID != "up-2" {
		t.Fatalf("opened = %d, claims = %+v; want a second upload, then a run on it", len(log.opened), log.claims)
	}

	f2 := newFake(t)
	f2.standardRoutes(t)
	log2 := f2.uploadRoutes(t, func(int, string) int { return http.StatusForbidden })
	f2.claimRoute(t, log2, "removeBgApi", nil)
	client2 := newClientFor(t, f2)
	_, err := client2.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("big.png", bigPNG)})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_link" {
		t.Fatalf("Run error = %v, want invalid_link", err)
	}
	if len(log2.opened) != 2 || len(log2.claims) != 0 {
		t.Errorf("opened = %d, claims = %d; want 2 and 0", len(log2.opened), len(log2.claims))
	}
}

func TestRunOnAnUploadWaitsForTheFile(t *testing.T) {
	override(t, &directUploadLimit, testDirectLimit)
	override(t, &uploadIncompletePause, time.Millisecond)
	f := newFake(t)
	f.standardRoutes(t)
	log := f.uploadRoutes(t, nil)
	f.claimRoute(t, log, "removeBgApi", func(n int, w http.ResponseWriter) bool {
		if n < 3 {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"File not received yet","code":"upload_incomplete"}`)
			return true
		}
		return false
	})
	client := newClientFor(t, f)

	if _, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("big.png", bigPNG)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(log.claims) != 3 || len(log.opened) != 1 {
		t.Fatalf("claims = %d, uploads = %d; want 3 runs on one upload", len(log.claims), len(log.opened))
	}
	for _, claim := range log.claims {
		if claim.uploadID != "up-1" {
			t.Errorf("upload id = %q, want up-1 each time", claim.uploadID)
		}
	}
}

func TestRunOnAnUploadGivesUpAfterFiveTries(t *testing.T) {
	override(t, &directUploadLimit, testDirectLimit)
	override(t, &uploadIncompletePause, time.Millisecond)
	f := newFake(t)
	f.standardRoutes(t)
	log := f.uploadRoutes(t, nil)
	f.claimRoute(t, log, "removeBgApi", func(_ int, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"File not received yet","code":"upload_incomplete"}`)
		return true
	})
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("big.png", bigPNG)})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusConflict {
		t.Fatalf("Run error = %v, want the 409", err)
	}
	if len(log.claims) != uploadIncompleteTries {
		t.Errorf("tries = %d, want %d", len(log.claims), uploadIncompleteTries)
	}
}

func TestUploadIDIsNeverSentAgainAfterAnotherAnswer(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusNotFound, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			override(t, &directUploadLimit, testDirectLimit)
			f := newFake(t)
			f.standardRoutes(t)
			log := f.uploadRoutes(t, nil)
			f.claimRoute(t, log, "removeBgApi", func(_ int, w http.ResponseWriter) bool {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"failed","code":"upload_not_found"}`)
				return true
			})
			client := newClientFor(t, f)

			_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("big.png", bigPNG)})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.HTTPStatus != status {
				t.Fatalf("Run error = %v, want HTTP %d", err, status)
			}
			if len(log.claims) != 1 || len(log.opened) != 1 {
				t.Errorf("claims = %d, uploads = %d; the upload id must be used once", len(log.claims), len(log.opened))
			}
		})
	}
}

func TestUnexpectedUseUploadsSwitchesToAnUploadOnce(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	log := f.uploadRoutes(t, nil)
	f.claimRoute(t, log, "removeBgApi", func(n int, w http.ResponseWriter) bool {
		if n == 1 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = io.WriteString(w, `{"error":"too large","code":"use_uploads","max_bytes":1024}`)
			return true
		}
		return false
	})
	client := newClientFor(t, f)

	if _, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("big.png", bigPNG)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(log.claims) != 2 || log.claims[0].uploadID != "" || log.claims[1].uploadID != "up-1" {
		t.Fatalf("claims = %+v, want a direct run, then a run on an upload", log.claims)
	}
	if len(log.puts) != 1 || !bytes.Equal(log.puts[0].body, bigPNG) {
		t.Fatalf("the file was not uploaded")
	}

	g := newFake(t)
	g.standardRoutes(t)
	glog := g.uploadRoutes(t, nil)
	g.claimRoute(t, glog, "collage", func(_ int, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"error":"too large","code":"use_uploads","max_bytes":1024}`)
		return true
	})
	gclient := newClientFor(t, g)
	_, err := gclient.Tools.Run(context.Background(), "collage", RunRequest{InputLists: map[string][]Source{
		"images": {FromBytes("a.png", bigPNG), FromBytes("b.png", bigPNG)},
	}})
	var apiErr *APIError
	if !errors.Is(err, ErrFileTooLarge) || !errors.As(err, &apiErr) || apiErr.Code != "use_uploads" {
		t.Fatalf("Run error = %v, want the use_uploads answer", err)
	}
	if len(glog.claims) != 1 || len(glog.opened) != 0 {
		t.Errorf("claims = %d, uploads = %d; want 1 and 0", len(glog.claims), len(glog.opened))
	}
}

func TestRunsThatCannotFitAreRefusedBeforeSending(t *testing.T) {
	override(t, &directUploadLimit, testDirectLimit)
	f := newFake(t)
	f.standardRoutes(t)
	client := newClientFor(t, f)

	cases := map[string]RunRequest{
		"several large files in the main input": {InputLists: map[string][]Source{
			"images": {FromBytes("a.png", bigPNG), FromBytes("b.png", bigPNG)},
		}},
		"a large secondary input": {Inputs: map[string]Source{
			"image": FromBytes("a.png", bigPNG), "mask": FromBytes("m.png", bigPNG),
		}},
		"a large file next to a source URL": {Inputs: map[string]Source{
			"image": FromURL("https://images.example/a.png"), "mask": FromBytes("m.png", bigPNG),
		}},
	}
	tools := map[string]string{
		"several large files in the main input": "collage",
		"a large secondary input":               "mask",
		"a large file next to a source URL":     "mask",
	}
	for name, req := range cases {
		_, err := client.Tools.Run(context.Background(), tools[name], req)
		if !errors.Is(err, ErrFileTooLarge) || !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: error = %v, want ErrFileTooLarge and ErrInvalidInput", name, err)
		}
	}
	if f.count("POST /v1/uploads") != 0 || f.count("POST /v1/modules/mask") != 0 || f.count("POST /v1/modules/collage") != 0 {
		t.Errorf("a run that cannot fit was sent")
	}
}

func TestUploadLinkInPlainHTTPIsRefused(t *testing.T) {
	override(t, &directUploadLimit, testDirectLimit)
	f := newFake(t)
	f.standardRoutes(t)
	f.route("POST /v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"upload_id":"up-1","upload_url":"http://files.example/u/up-1","method":"PUT"}`)
	})
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("big.png", bigPNG)})
	if !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("Run error = %v, want ErrUnexpectedResponse", err)
	}
	if f.count("POST /v1/modules/removeBgApi") != 0 {
		t.Errorf("the run was sent without its upload")
	}
}

func TestSourceURLIsSentAsAField(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	var gotURL string
	var gotFiles int
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		gotURL = r.MultipartForm.Value["source_url"][0]
		gotFiles = len(r.MultipartForm.File)
		_, _ = io.WriteString(w, f.runAnswer())
	})
	client := newClientFor(t, f)

	const source = "https://images.example/photo.png?token=abc"
	if _, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromURL(source)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotURL != source || gotFiles != 0 {
		t.Errorf("source_url = %q, files = %d", gotURL, gotFiles)
	}
}

func TestSourceURLRules(t *testing.T) {
	mask := &Tool{ID: "mask", Executable: true, Inputs: []ToolInput{
		{ID: "image", Required: true}, {ID: "mask", Required: true},
	}}
	refused := map[string]RunRequest{
		"plain http":      {Inputs: map[string]Source{"image": FromURL("http://images.example/a.png"), "mask": FromBytes("", pngBytes)}},
		"other port":      {Inputs: map[string]Source{"image": FromURL("https://images.example:8443/a.png"), "mask": FromBytes("", pngBytes)}},
		"credentials":     {Inputs: map[string]Source{"image": FromURL("https://u:p@images.example/a.png"), "mask": FromBytes("", pngBytes)}},
		"too long":        {Inputs: map[string]Source{"image": FromURL("https://images.example/" + strings.Repeat("a", 2048)), "mask": FromBytes("", pngBytes)}},
		"secondary input": {Inputs: map[string]Source{"image": FromBytes("", pngBytes), "mask": FromURL("https://images.example/m.png")}},
		"two URLs":        {Inputs: map[string]Source{"image": FromURL("https://images.example/a.png"), "mask": FromURL("https://images.example/m.png")}},
	}
	for name, req := range refused {
		if _, err := planRun("mask", mask, req); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: error = %v, want ErrInvalidInput", name, err)
		}
	}
	ok := RunRequest{Inputs: map[string]Source{"image": FromURL("https://images.example:443/a.png"), "mask": FromBytes("", pngBytes)}}
	if _, err := planRun("mask", mask, ok); err != nil {
		t.Errorf("a URL on port 443 for the main input: %v", err)
	}
}

func TestRetriedRunSendsTheSameFileAgain(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	var bodies [][]byte
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		file, _, err := r.FormFile("image")
		if err != nil {
			http.Error(w, "no file", http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(file)
		bodies = append(bodies, data)
		if len(bodies) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":"slow down","code":"rate_limited"}`, http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, f.runAnswer())
	})
	client := newClientFor(t, f)

	sources := map[string]Source{
		"path":     FromPath(writeTemp(t, bigPNG)),
		"seeker":   FromReader("a.png", bytes.NewReader(bigPNG)),
		"stream":   FromReader("a.png", io.MultiReader(bytes.NewReader(bigPNG))),
		"sized":    FromReaderSize("a.png", io.MultiReader(bytes.NewReader(bigPNG)), int64(len(bigPNG))),
		"in bytes": FromBytes("a.png", bigPNG),
	}
	for name, src := range sources {
		bodies = nil
		if _, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: src}); err != nil {
			t.Fatalf("%s: Run: %v", name, err)
		}
		if len(bodies) != 2 || !bytes.Equal(bodies[0], bigPNG) || !bytes.Equal(bodies[1], bigPNG) {
			t.Errorf("%s: the two attempts did not send the whole file", name)
		}
	}
}

func TestLargeStreamIsSpooledToATemporaryFile(t *testing.T) {
	override(t, &directUploadLimit, testDirectLimit)
	f := newFake(t)
	f.standardRoutes(t)
	log := f.uploadRoutes(t, nil)
	f.claimRoute(t, log, "removeBgApi", nil)
	client := newClientFor(t, f)
	t.Setenv("TMPDIR", t.TempDir())

	large := append(append([]byte(nil), pngBytes...), bytes.Repeat([]byte{3}, spoolMemoryMax)...)
	if _, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{
		Image: FromReader("big.png", io.MultiReader(bytes.NewReader(large))),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(log.puts) != 1 || !bytes.Equal(log.puts[0].body, large) || log.puts[0].contentLength != int64(len(large)) {
		t.Fatalf("the spooled stream was not uploaded as is")
	}
	left, _ := filepath.Glob(filepath.Join(os.TempDir(), "pixlub-input-*"))
	if len(left) != 0 {
		t.Errorf("temporary copies left behind: %v", left)
	}
}

func TestFromReaderSizeIsChecked(t *testing.T) {
	cases := map[string]Source{
		"seeker of another size": FromReaderSize("a.png", bytes.NewReader(pngBytes), 10),
		"stream shorter":         FromReaderSize("a.png", io.MultiReader(bytes.NewReader(pngBytes)), int64(len(pngBytes)+5)),
		"no size":                FromReaderSize("a.png", bytes.NewReader(pngBytes), 0),
		"nil reader":             FromReaderSize("a.png", nil, 10),
	}
	for name, src := range cases {
		if _, err := prepareFile(src, "image", "input 'image'", 1<<20); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: error = %v, want ErrInvalidInput", name, err)
		}
	}

	never := readerFunc(func([]byte) (int, error) { t.Fatal("the stream was read"); return 0, io.EOF })
	_, err := prepareFile(FromReaderSize("a.png", never, 2<<20), "image", "input 'image'", 1<<20)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("too large: error = %v, want ErrFileTooLarge", err)
	}
	file, err := prepareFile(FromReaderSize("", io.MultiReader(bytes.NewReader(pngBytes)), int64(len(pngBytes))), "image", "input 'image'", 1<<20)
	if err != nil || file.size != int64(len(pngBytes)) || file.contentType != "image/png" {
		t.Fatalf("sized stream = %+v, %v", file, err)
	}
}

func TestFileThatChangedIsNotRetried(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, f.runAnswer())
	})
	client := newClientFor(t, f)

	path := writeTemp(t, bigPNG)
	file, err := prepareFile(FromPath(path), "image", "input 'image'", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = client.postRun(context.Background(), "removeBgApi", []formField{{name: "params", value: "{}"}}, []*inputFile{file}, policyCreate)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	if n := f.count("POST /v1/modules/removeBgApi"); n > 1 {
		t.Errorf("a changed file was sent %d times", n)
	}
}

func TestFormSizeIsExact(t *testing.T) {
	files := []*inputFile{}
	for _, src := range []Source{FromBytes("a.png", pngBytes), FromBytes(`b"c.png`, bigPNG)} {
		file, err := prepareFile(src, "images", "input 'images'", 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	form, err := newForm([]formField{{name: "params", value: `{"a":1}`}, {name: "upload_id", value: "up-1"}}, files)
	if err != nil {
		t.Fatal(err)
	}
	reader, _ := form.open()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	form.wait()
	if int64(len(data)) != form.size {
		t.Fatalf("form of %d bytes, announced %d", len(data), form.size)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.png")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
