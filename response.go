package pixlub

import (
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const maxErrorMessage = 500

const maxErrorBody = 64 << 10

var outputNamePattern = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)

var outputKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_#-]{0,63}$`)

type catalogEntry struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Category    string          `json:"category"`
	Executable  bool            `json:"executable"`
	Handler     string          `json:"handler"`
	Inputs      []catalogInput  `json:"inputs"`
	Outputs     []catalogOutput `json:"outputs"`
}

type catalogInput struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Required    bool     `json:"required"`
	Type        string   `json:"type"`
	Multiple    bool     `json:"multiple"`
	PerFile     bool     `json:"per_file"`
	MaxFiles    int      `json:"max_files"`
	Accept      []string `json:"accept"`
}

type catalogOutput struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	Description  string   `json:"description"`
	Kind         string   `json:"kind"`
	Multiple     bool     `json:"multiple"`
	ContentTypes []string `json:"content_types"`
}

func parseCatalog(body []byte) ([]Tool, error) {
	var entries []catalogEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, wrapf(ErrUnexpectedResponse, "the tool catalog is not a list")
	}
	tools := make([]Tool, 0, len(entries))
	for _, entry := range entries {
		if entry.ID == "" {
			return nil, wrapf(ErrUnexpectedResponse, "the tool catalog contains an entry without an id")
		}
		tool := Tool{
			ID:          entry.ID,
			Name:        entry.Name,
			Description: entry.Description,
			Category:    entry.Category,
			Executable:  entry.Executable,
			Handler:     entry.Handler,
		}
		if tool.Name == "" {
			tool.Name = tool.ID
		}
		if tool.Handler == "" {
			tool.Handler = "none"
		}
		for _, in := range entry.Inputs {
			if in.ID == "" {
				continue
			}
			inputType := in.Type
			if inputType == "" {
				inputType = "image"
			}
			tool.Inputs = append(tool.Inputs, ToolInput{
				ID:          in.ID,
				Label:       in.Label,
				Description: in.Description,
				Required:    in.Required,
				Type:        inputType,
				Multiple:    in.Multiple,
				PerFile:     in.PerFile,
				MaxFiles:    max(in.MaxFiles, 0),
				Accept:      in.Accept,
			})
		}
		for _, out := range entry.Outputs {
			if out.Key == "" {
				continue
			}
			tool.Outputs = append(tool.Outputs, ToolOutput{
				Key:          out.Key,
				Label:        out.Label,
				Description:  out.Description,
				Kind:         out.Kind,
				Multiple:     out.Multiple,
				ContentTypes: out.ContentTypes,
			})
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

type outputLink struct {
	key         string
	name        string
	url         string
	contentType string

	bytes int64
}

type receipt struct {
	module    string
	id        string
	outputs   []outputLink
	expiresAt time.Time
	cost      *int64
	balance   *int64
	message   string
}

type runEntry struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
}

type runAnswer struct {
	ID        string     `json:"id"`
	Module    string     `json:"module"`
	Outputs   []runEntry `json:"outputs"`
	ExpiresAt string     `json:"expires_at"`
	Credits   *struct {
		Cost    *int64 `json:"cost"`
		Balance *int64 `json:"balance"`
	} `json:"credits"`
	Message string `json:"message"`
}

func parseReceipt(body []byte, baseURL, apiKey string) (*receipt, error) {
	if len(body) > maxRunAnswerBytes {
		return nil, wrapf(ErrUnexpectedResponse, "the run answer is larger than the SDK accepts")
	}
	var answer runAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, wrapf(ErrUnexpectedResponse, "the run response is not a JSON object")
	}
	if len(answer.Outputs) == 0 {
		return nil, wrapf(ErrUnexpectedResponse, "the run response lists no output")
	}

	out := &receipt{
		module:  answer.Module,
		id:      answer.ID,
		message: truncate(answer.Message, 1000),
	}
	for _, entry := range answer.Outputs {
		if entry.URL == "" {
			return nil, wrapf(ErrUnexpectedResponse, "the run response contains an output without a link")
		}
		link, err := resolveDownloadURL(entry.URL, baseURL)
		if err != nil {
			return nil, err
		}
		name := entry.Name
		if !outputNamePattern.MatchString(name) {
			name = "file"
		}
		contentType := entry.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		key := entry.Key
		if !outputKeyPattern.MatchString(key) {
			key = ""
		}
		out.outputs = append(out.outputs, outputLink{
			key:         key,
			name:        name,
			url:         link,
			contentType: contentType,
			bytes:       max(entry.Bytes, 0),
		})
	}
	if answer.Credits != nil {
		out.cost = answer.Credits.Cost
		out.balance = answer.Credits.Balance
	}
	if t, err := time.Parse(time.RFC3339, answer.ExpiresAt); err == nil {
		out.expiresAt = t
	}
	return out, nil
}

func outputFromResponse(link outputLink, resp *response, apiKey string) (Output, error) {
	if resp.tooLarge {
		return Output{}, wrapf(ErrUnexpectedResponse,
			"the tool output is larger than %d bytes and was not read", maxOutputBytes)
	}
	if err := checkStatus(resp, apiKey); err != nil {
		return Output{}, err
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.header.Get("Content-Type"), ";", 2)[0]))
	if contentType == "" {
		contentType = link.contentType
	}
	return Output{
		Key:         link.key,
		Name:        link.name,
		URL:         link.url,
		ContentType: contentType,
		Bytes:       int64(len(resp.body)),
		Content:     resp.body,
	}, nil
}

func apiErrorFromResponse(resp *response, apiKey string) error {
	return newAPIError(resp, apiKey)
}

func newAPIError(resp *response, apiKey string) *APIError {

	body := resp.body
	if len(body) > maxErrorBody {
		body = body[:maxErrorBody]
	}
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)

	message := ""
	for _, field := range []string{"message", "error"} {
		if text, ok := payload[field].(string); ok && strings.TrimSpace(text) != "" {
			message = strings.TrimSpace(text)
			break
		}
	}
	if message == "" {
		message = "the Pixlub API returned an error"
	}
	message = truncate(redact(cleanMessage(message), apiKey), maxErrorMessage)

	code, _ := payload["code"].(string)
	err := &APIError{
		HTTPStatus:      resp.status,
		Code:            truncate(cleanMessage(code), 64),
		Message:         message,
		Details:         payload["details"],
		RequiredCredits: numberField(payload, "required"),
		MaxBytes:        numberField(payload, "max_bytes"),
		MaxPixels:       numberField(payload, "max_pixels"),
		Limit:           numberField(payload, "limit"),
		kind:            kindForStatus(resp.status),
	}
	if text, ok := payload["reset_at"].(string); ok {
		if when, parseErr := time.Parse(time.RFC3339, text); parseErr == nil {
			err.ResetAt = when
		}
	}
	if wait, ok := parseRetryAfter(resp.header.Get("Retry-After")); ok {
		err.RetryAfter = wait
	} else if seconds := numberField(payload, "retry_after"); seconds != nil && *seconds >= 0 {
		err.RetryAfter = time.Duration(min(*seconds, maxRetryAfterField)) * time.Second
	}
	return err
}

const maxRetryAfterField = 24 * 60 * 60

func numberField(payload map[string]any, name string) *int64 {
	number, ok := payload[name].(float64)
	if !ok || number != math.Trunc(number) || math.Abs(number) > 1<<53 {
		return nil
	}
	value := int64(number)
	return &value
}

func cleanMessage(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
}

func kindForStatus(status int) error {
	switch {
	case status == http.StatusUnauthorized:
		return ErrAuthentication
	case status == http.StatusPaymentRequired:
		return ErrInsufficientCredits
	case status == http.StatusForbidden:
		return ErrPermissionDenied
	case status == http.StatusNotFound:
		return ErrNotFound
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status >= 400 && status < 500:
		return ErrInvalidRequest
	case status >= 500 && status < 600:
		return ErrServer
	}
	return ErrAPI
}
