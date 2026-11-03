package pixlub

import (
	"errors"
	"strings"
	"testing"
)

const testKey = "pix.AbCdEf12.Secret0123456789abcdefghijklmnopqrstuv"

func TestValidateAPIKey(t *testing.T) {
	if err := validateAPIKey(testKey); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	for _, key := range []string{"", "not-a-key", "pix.only-two-parts", "sk_live_123"} {
		err := validateAPIKey(key)
		if !errors.Is(err, ErrConfiguration) {
			t.Errorf("validateAPIKey(%q) = %v, want ErrConfiguration", key, err)
		}
		if key != "" && strings.Contains(err.Error(), key) {
			t.Errorf("error echoes the key: %v", err)
		}
	}
}

func TestKeyMaskAndRedact(t *testing.T) {
	if got := keyMask(testKey); got != "pix.AbCdEf12.***" {
		t.Errorf("keyMask = %q", got)
	}
	text := "bad key " + testKey + " in the body"
	if got := redact(text, testKey); strings.Contains(got, testKey) {
		t.Errorf("redact left the key in place: %q", got)
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{raw: "https://api.pixlub.test", want: "https://api.pixlub.test"},
		{raw: "https://api.pixlub.test/", want: "https://api.pixlub.test"},
		{raw: "http://localhost:8080", want: "http://localhost:8080"},
		{raw: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{raw: "http://api.pixlub.test", wantErr: true},
		{raw: "https://user:pass@api.pixlub.test", wantErr: true},
		{raw: "https://api.pixlub.test/?x=1", wantErr: true},
		{raw: "ftp://api.pixlub.test", wantErr: true},
		{raw: "api.pixlub.test", wantErr: true},
	}
	for _, tc := range cases {
		got, err := normalizeBaseURL(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeBaseURL(%q) = %q, want an error", tc.raw, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("normalizeBaseURL(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
}

func TestResolveDownloadURL(t *testing.T) {
	base := "https://api.pixlub.test"

	got, err := resolveDownloadURL("/v1/results/abc_result.png?exp=1&sig=s", base)
	if err != nil || got != "https://api.pixlub.test/v1/results/abc_result.png?exp=1&sig=s" {
		t.Errorf("relative link = %q, %v", got, err)
	}

	refused := []string{
		"http://evil.example/x.png",
		"ftp://api.pixlub.test/x",
		"https://user:pass@api.pixlub.test/x",
		"",
	}
	for _, raw := range refused {
		if _, err := resolveDownloadURL(raw, base); !errors.Is(err, ErrUnexpectedResponse) {
			t.Errorf("resolveDownloadURL(%q) error = %v, want ErrUnexpectedResponse", raw, err)
		}
	}
}

func TestNewClientReadsEnvironment(t *testing.T) {
	t.Setenv(APIKeyEnv, testKey)
	t.Setenv(BaseURLEnv, "https://env.pixlub.test")

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.baseURL != "https://env.pixlub.test" {
		t.Errorf("base URL = %q, want the environment value", client.baseURL)
	}
}

func TestNewClientRequiresKey(t *testing.T) {
	t.Setenv(APIKeyEnv, "")
	if _, err := NewClient(); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("NewClient without key = %v, want ErrConfiguration", err)
	}
}

func TestNewClientRejectsBadOptions(t *testing.T) {
	cases := map[string]Option{
		"retries too high":  WithMaxRetries(6),
		"retries negative":  WithMaxRetries(-1),
		"zero input limit":  WithMaxInputBytes(0),
		"zero timeout":      WithTimeout(0),
		"plain http remote": WithBaseURL("http://api.pixlub.test"),
	}
	for name, opt := range cases {
		_, err := NewClient(WithAPIKey(testKey), opt)
		if !errors.Is(err, ErrConfiguration) {
			t.Errorf("%s: NewClient error = %v, want ErrConfiguration", name, err)
		}
	}
}

func TestClientStringHidesKey(t *testing.T) {
	client, err := NewClient(WithAPIKey(testKey), WithBaseURL("https://api.pixlub.test"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	for _, rendered := range []string{client.String(), client.GoString()} {
		if strings.Contains(rendered, testKey) || !strings.Contains(rendered, "pix.AbCdEf12.***") {
			t.Errorf("client rendering exposes the key or lacks the mask: %s", rendered)
		}
	}
}
