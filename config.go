package pixlub

import (
	"net"
	"net/url"
	"regexp"
	"strings"
)

const (
	// APIKeyEnv is the environment variable read when no key is passed.
	APIKeyEnv = "PIXLUB_API_KEY"
	// BaseURLEnv is the environment variable read when no base URL is passed.
	BaseURLEnv = "PIXLUB_BASE_URL"
	// DefaultBaseURL is used when neither WithBaseURL nor BaseURLEnv is set.
	DefaultBaseURL = "https://api.pixlub.com"
)

const maxSourceURL = 2048

var apiKeyPattern = regexp.MustCompile(`^pix\.[A-Za-z0-9_-]{1,64}\.[A-Za-z0-9_-]{1,256}$`)

func validateAPIKey(key string) error {
	if key == "" {
		return wrapf(ErrConfiguration,
			"no API key provided: pass WithAPIKey or set the %s environment variable", APIKeyEnv)
	}
	if !apiKeyPattern.MatchString(key) {
		return wrapf(ErrConfiguration,
			"the API key is malformed: copy the whole key (pix.<prefix>.<secret>) from your dashboard")
	}
	return nil
}

func keyMask(key string) string {
	parts := strings.Split(key, ".")
	if len(parts) < 3 {
		return "***"
	}
	return "pix." + parts[1] + ".***"
}

func redact(text, key string) string {
	if key == "" {
		return text
	}
	return strings.ReplaceAll(text, key, keyMask(key))
}

func normalizeBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", wrapf(ErrConfiguration,
			"invalid base URL: expected a form like https://api.example.com")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return "", wrapf(ErrConfiguration,
			"the base URL must start with http:// or https:// and name a host")
	}
	if parsed.User != nil {
		return "", wrapf(ErrConfiguration,
			"do not put credentials in the base URL: use the API key instead")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", wrapf(ErrConfiguration,
			"the base URL must not contain a query string or a fragment")
	}
	if parsed.Scheme == "http" && !isLoopback(parsed.Hostname()) {
		return "", wrapf(ErrConfiguration,
			"plain http:// is only accepted for localhost: use https:// so the API key is encrypted in transit")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}

func resolveDownloadURL(raw, base string) (string, error) {
	return resolveLink(raw, base, "result link")
}

func resolveLink(raw, base, what string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", wrapf(ErrUnexpectedResponse, "the API sent an empty %s", what)
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", wrapf(ErrConfiguration, "invalid base URL")
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return "", wrapf(ErrUnexpectedResponse, "refused a malformed %s", what)
	}
	absolute := baseURL.ResolveReference(ref)
	if (absolute.Scheme != "http" && absolute.Scheme != "https") || absolute.Hostname() == "" {
		return "", wrapf(ErrUnexpectedResponse, "refused a %s that is not an http(s) URL", what)
	}
	if absolute.User != nil {
		return "", wrapf(ErrUnexpectedResponse, "refused a %s that carries credentials", what)
	}
	if absolute.Scheme == "http" && !isLoopback(absolute.Hostname()) {
		return "", wrapf(ErrUnexpectedResponse, "refused a %s that uses plain http outside localhost", what)
	}
	return absolute.String(), nil
}

func checkSourceURL(raw string) error {
	if len(raw) > maxSourceURL {
		return wrapf(ErrInvalidInput, "the source URL is longer than %d characters", maxSourceURL)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return wrapf(ErrInvalidInput, "the source URL must be an https:// URL that names a host")
	}
	if parsed.User != nil {
		return wrapf(ErrInvalidInput, "the source URL must not contain credentials")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return wrapf(ErrInvalidInput, "the source URL must use the https port, 443")
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
