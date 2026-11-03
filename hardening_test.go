package pixlub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientPrintedByValueHidesTheKey(t *testing.T) {
	client := newClientFor(t, newFake(t))
	for _, text := range []string{
		fmt.Sprintf("%v", *client),
		fmt.Sprintf("%+v", *client),
		fmt.Sprintf("%#v", *client),
		fmt.Sprintf("%s", *client),
		fmt.Sprintf("%v", client),
	} {
		if strings.Contains(text, testKey) {
			t.Fatalf("the key leaked into printed text: %s", text)
		}
	}
}

func TestConcurrentCatalogCallsShareOneRequest(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	client := newClientFor(t, f)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.Tools.Retrieve(context.Background(), "removeBgApi"); err != nil {
				t.Errorf("Retrieve: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := f.count("GET /v1/modules"); got != 1 {
		t.Fatalf("the catalog was requested %d times, want 1", got)
	}
}

func TestRunRefusesAnUnknownToolBeforeSending(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "nosuchtool", RunRequest{
		Image: FromBytes("a.png", pngBytes),
	})
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
	if got := f.count("POST /v1/modules/nosuchtool"); got != 0 {
		t.Fatalf("the unknown tool was sent %d times", got)
	}
}

func TestRunWithTooManyOutputsSaysItWasCharged(t *testing.T) {
	f := newFake(t)
	f.standardRoutes(t)
	output := fmt.Sprintf(`{"key":"out","name":"file","url":"%s/r/run-1_result.json?exp=1&sig=s","content_type":"application/json"}`,
		f.files.srv.URL)
	outputs := make([]string, 0, maxRunOutputs+1)
	for i := 0; i < maxRunOutputs+1; i++ {
		outputs = append(outputs, output)
	}
	answer := fmt.Sprintf(`{"id":"run-1","module":"removeBgApi","outputs":[%s]}`, strings.Join(outputs, ","))
	f.route("POST /v1/modules/removeBgApi", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, answer)
	})
	client := newClientFor(t, f)

	_, err := client.Tools.Run(context.Background(), "removeBgApi", RunRequest{Image: FromBytes("a.png", pngBytes)})
	if !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("err = %v, want ErrUnexpectedResponse", err)
	}
	if !strings.Contains(err.Error(), "run-1") || !strings.Contains(err.Error(), "charged") {
		t.Fatalf("err = %v, want the run id and the fact that it was charged", err)
	}
}

func TestSaveRefusesToWriteThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need extra rights on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "result.png")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	output := &Output{Name: "image", Content: []byte("new bytes")}
	if err := output.Save(link, true); err == nil {
		t.Fatal("Save wrote through a symbolic link")
	}
	if got, _ := os.ReadFile(target); string(got) != "keep me" {
		t.Fatalf("the target of the link was overwritten: %q", got)
	}
}

func TestSaveWritesAPrivateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "result.png")
	output := &Output{Name: "image", Content: []byte("x")}
	if err := output.Save(path, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %v, want 0600", mode)
	}
}

func TestCustomHTTPClientWithoutTimeoutGetsTheDefault(t *testing.T) {
	client := newClientFor(t, newFake(t), WithHTTPClient(&http.Client{}))
	if client.timeout != defaultTimeout || client.http.Timeout != 0 {
		t.Fatalf("timeout = %v (http %v), want %v", client.timeout, client.http.Timeout, defaultTimeout)
	}

	kept := newClientFor(t, newFake(t), WithHTTPClient(&http.Client{Timeout: 42 * time.Second}))
	if kept.timeout != 42*time.Second || kept.http.Timeout != 0 {
		t.Fatalf("a timeout set by the caller was replaced: %v (http %v)", kept.timeout, kept.http.Timeout)
	}

	explicit := newClientFor(t, newFake(t), WithHTTPClient(&http.Client{Timeout: 42 * time.Second}), WithTimeout(time.Second))
	if explicit.timeout != time.Second {
		t.Fatalf("WithTimeout must win over the timeout of the client: %v", explicit.timeout)
	}
}

func TestErrorMessagesLoseTheirControlCharacters(t *testing.T) {
	err := apiErrorFromResponse(&response{
		status: http.StatusBadRequest,
		body:   []byte(`{"message":"bad\u001b[2J input"}`),
	}, testKey)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if strings.ContainsRune(apiErr.Message, '\x1b') {
		t.Fatalf("the message keeps an escape character: %q", apiErr.Message)
	}
}

func TestOutputNamesFromTheAPIAreSanitized(t *testing.T) {
	answer := `{"id":"run-1","module":"removeBgApi","outputs":[` +
		`{"name":"../../evil.sh","url":"/v1/results/run-1_result.png?exp=1&sig=s","content_type":"image/png"}]}`
	rcpt, err := parseReceipt([]byte(answer), "https://api.example", testKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := rcpt.outputs[0].name; got != "file" {
		t.Fatalf("name = %q, want the generic label", got)
	}
}

func TestOversizedRunAnswerIsRefused(t *testing.T) {
	body := []byte(`{"id":"run-1","outputs":[` + strings.Repeat(" ", maxRunAnswerBytes) + `]}`)
	if _, err := parseReceipt(body, "https://api.example", testKey); !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("err = %v, want ErrUnexpectedResponse", err)
	}
}
