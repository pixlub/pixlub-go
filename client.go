package pixlub

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// DirectUploadMaxBytes is the largest total of files sent in one request.
// Larger files are uploaded automatically.
const DirectUploadMaxBytes = 90 << 20

const (
	defaultMaxRetries = 2
	maxRetriesLimit   = 5

	defaultMaxInputBytes = 3 << 30

	defaultTimeout = 5 * time.Minute

	defaultJobTimeout = 30 * time.Minute

	connectTimeout = 10 * time.Second

	maxJSONBytes = 10 << 20

	maxSchemaBytes = 512 << 10

	maxRunAnswerBytes = 256 << 10

	maxOutputBytes = 100 << 20

	maxRunOutputs = 64 * 4

	maxRunOutputBytes = 1 << 30

	catalogTTL = 10 * time.Minute
)

// Option configures a Client. Pass options to NewClient.
type Option func(*settings)

type settings struct {
	apiKey        string
	baseURL       string
	timeout       time.Duration
	timeoutSet    bool
	jobTimeout    time.Duration
	maxRetries    int
	maxInputBytes int64
	httpClient    *http.Client
}

// WithAPIKey sets the API key. The default is PIXLUB_API_KEY.
func WithAPIKey(key string) Option {
	return func(s *settings) { s.apiKey = key }
}

// WithBaseURL sets the address of the API. The default is PIXLUB_BASE_URL, then
// https://api.pixlub.com. Plain http is accepted only for localhost.
func WithBaseURL(baseURL string) Option {
	return func(s *settings) { s.baseURL = baseURL }
}

// WithTimeout sets how long one request may wait. The default is five minutes.
// Use the context of each call to bound the whole call.
func WithTimeout(timeout time.Duration) Option {
	return func(s *settings) {
		s.timeout = timeout
		s.timeoutSet = true
	}
}

// WithJobTimeout sets how long Run waits for a long run. The default is 30
// minutes. After that, Run fails with ErrJobTimeout: the run may still finish,
// and may have been charged.
func WithJobTimeout(timeout time.Duration) Option {
	return func(s *settings) { s.jobTimeout = timeout }
}

// WithMaxRetries sets how many times a request is retried, from 0 to 5. The
// default is 2. A request that may have been charged is never retried.
func WithMaxRetries(n int) Option {
	return func(s *settings) { s.maxRetries = n }
}

// WithMaxInputBytes sets the largest local file accepted. The default is 3 GiB.
func WithMaxInputBytes(n int64) Option {
	return func(s *settings) { s.maxInputBytes = n }
}

// WithHTTPClient uses this *http.Client, for proxies or custom TLS settings.
// Its timeout, if any, becomes the default of WithTimeout.
func WithHTTPClient(client *http.Client) Option {
	return func(s *settings) { s.httpClient = client }
}

// Client talks to the Pixlub API. Create one with NewClient and reuse it. It is
// safe for concurrent use.
type Client struct {
	apiKey        string
	baseURL       string
	http          *http.Client
	timeout       time.Duration
	jobTimeout    time.Duration
	maxRetries    int
	maxInputBytes int64

	catalog *catalogCache

	// Tools lists, describes and runs tools.
	Tools *Tools
}

// NewClient checks the configuration and returns a Client.
func NewClient(opts ...Option) (*Client, error) {
	s := settings{
		timeout:       defaultTimeout,
		jobTimeout:    defaultJobTimeout,
		maxRetries:    defaultMaxRetries,
		maxInputBytes: defaultMaxInputBytes,
	}
	for _, opt := range opts {
		opt(&s)
	}

	key := s.apiKey
	if key == "" {
		key = os.Getenv(APIKeyEnv)
	}
	if err := validateAPIKey(key); err != nil {
		return nil, err
	}

	raw := s.baseURL
	if raw == "" {
		raw = os.Getenv(BaseURLEnv)
	}
	if raw == "" {
		raw = DefaultBaseURL
	}
	baseURL, err := normalizeBaseURL(raw)
	if err != nil {
		return nil, err
	}

	if s.maxRetries < 0 || s.maxRetries > maxRetriesLimit {
		return nil, wrapf(ErrConfiguration, "max retries must be from 0 to %d", maxRetriesLimit)
	}
	if s.maxInputBytes <= 0 {
		return nil, wrapf(ErrConfiguration, "the maximum input size must be positive")
	}
	if s.timeoutSet && s.timeout <= 0 {
		return nil, wrapf(ErrConfiguration, "the timeout must be positive")
	}
	if s.jobTimeout <= 0 {
		return nil, wrapf(ErrConfiguration, "the job timeout must be positive")
	}

	timeout := s.timeout
	if !s.timeoutSet && s.httpClient != nil && s.httpClient.Timeout > 0 {
		timeout = s.httpClient.Timeout
	}

	client := &Client{
		apiKey:        key,
		baseURL:       baseURL,
		http:          newHTTPClient(s),
		timeout:       timeout,
		jobTimeout:    s.jobTimeout,
		maxRetries:    s.maxRetries,
		maxInputBytes: s.maxInputBytes,
		catalog:       &catalogCache{},
	}
	client.Tools = &Tools{client: client}
	return client, nil
}

func newHTTPClient(s settings) *http.Client {
	var client http.Client
	if s.httpClient != nil {
		client = *s.httpClient
	} else {
		client = http.Client{Transport: defaultTransport()}
	}

	client.Timeout = 0
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}

func defaultTransport() *http.Transport {
	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.DialContext = (&net.Dialer{Timeout: connectTimeout}).DialContext
	return transport
}

// String describes the client, with its key masked.
func (c Client) String() string {
	return fmt.Sprintf("pixlub.Client{base_url: %q, api_key: %q}", c.baseURL, keyMask(c.apiKey))
}

// GoString is the %#v form of String.
func (c Client) GoString() string { return c.String() }
