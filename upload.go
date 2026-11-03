package pixlub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var directUploadLimit int64 = DirectUploadMaxBytes

const uploadLinkTTL = 15 * time.Minute

const maxUploadAnswerBytes = 64 << 10

var uploadQueuePause = 2 * time.Second

var uploadIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type runRoute struct {
	upload *inputFile

	direct []*inputFile
}

func routeRun(tool *Tool, inputs *inputSet, limit int64) (runRoute, error) {
	if inputs.total() <= limit {
		return runRoute{direct: inputs.files}, nil
	}
	return routeUpload(tool, inputs, limit)
}

func routeUpload(tool *Tool, inputs *inputSet, limit int64) (runRoute, error) {
	total := inputs.total()
	if inputs.sourceURL != "" {
		return runRoute{}, tooLargef("the files total %d bytes; with a source URL, the other files must total %d bytes at most",
			total, limit)
	}
	main := primaryInput(tool)
	var route runRoute
	var rest int64
	count := 0
	for _, file := range inputs.files {
		if file.field == main {
			route.upload = file
			count++
			continue
		}
		route.direct = append(route.direct, file)
		rest += file.size
	}
	switch {
	case count == 0:
		return runRoute{}, tooLargef("the files total %d bytes, more than the %d bytes of one request, "+
			"and only the main input ('%s') can be uploaded", total, limit, main)
	case count > 1:
		return runRoute{}, tooLargef("input '%s' has %d files totalling more than %d bytes: "+
			"a large file goes alone, so run the tool once per file", main, count, limit)
	case rest > limit:
		return runRoute{}, tooLargef("the inputs other than '%s' total %d bytes; at most %d bytes can go with an uploaded file",
			main, rest, limit)
	}
	return route, nil
}

type uploadSlot struct {
	id         string
	url        string
	validUntil time.Time
}

func (c *Client) deposit(ctx context.Context, module string, file *inputFile) (string, error) {
	for reopened := false; ; reopened = true {
		slot, err := c.openUpload(ctx, module, file)
		if err != nil {
			return "", err
		}
		expired, err := c.putFile(ctx, slot, file)
		if err == nil {
			return slot.id, nil
		}
		if !expired || reopened {
			return "", err
		}
	}
}

func (c *Client) openUpload(ctx context.Context, module string, file *inputFile) (*uploadSlot, error) {
	body, err := json.Marshal(map[string]any{"module": module, "filename": file.filename, "bytes": file.size})
	if err != nil {
		return nil, wrapf(ErrInvalidInput, "cannot encode the upload request")
	}
	header := c.apiHeader("application/json")
	header.Set("Content-Type", "application/json")
	resp, err := c.send(ctx, &request{
		method:   http.MethodPost,
		url:      c.baseURL + "/v1/uploads",
		header:   header,
		body:     bytesPayload(body),
		maxBytes: maxUploadAnswerBytes,
		policy:   policyCreate,
	})
	if err != nil {
		return nil, err
	}
	answer, err := readUploadAnswer(resp, c.apiKey)
	if err != nil {
		return nil, err
	}
	if answer.Status == "queued" {
		if resp, answer, err = c.waitForUploadRoom(ctx, answer.UploadID); err != nil {
			return nil, err
		}
	}
	if method := strings.ToUpper(answer.Method); method != "" && method != http.MethodPut {
		return nil, wrapf(ErrUnexpectedResponse, "the API asks for an upload method the SDK does not know")
	}
	link, err := resolveLink(answer.UploadURL, c.baseURL, "upload link")
	if err != nil {
		return nil, err
	}
	return &uploadSlot{
		id:         answer.UploadID,
		url:        link,
		validUntil: time.Now().Add(linkLifetime(answer.ExpiresAt, resp.header)),
	}, nil
}

type uploadAnswer struct {
	UploadID  string `json:"upload_id"`
	Status    string `json:"status"`
	UploadURL string `json:"upload_url"`
	Method    string `json:"method"`
	ExpiresAt string `json:"expires_at"`
}

func readUploadAnswer(resp *response, apiKey string) (*uploadAnswer, error) {
	if err := checkStatus(resp, apiKey); err != nil {
		return nil, err
	}
	var answer uploadAnswer
	if resp.tooLarge || json.Unmarshal(resp.body, &answer) != nil {
		return nil, wrapf(ErrUnexpectedResponse, "the answer to the upload request is not a JSON object")
	}
	if !uploadIDPattern.MatchString(answer.UploadID) {
		return nil, wrapf(ErrUnexpectedResponse, "the API opened an upload without a valid upload id")
	}
	return &answer, nil
}

func (c *Client) waitForUploadRoom(ctx context.Context, id string) (*response, *uploadAnswer, error) {
	header := c.apiHeader("application/json")
	header.Set("Prefer", "wait="+strconv.Itoa(jobWaitSeconds))
	for {
		asked := time.Now()
		resp, err := c.send(ctx, &request{
			method:    http.MethodGet,
			url:       c.baseURL + "/v1/uploads/" + url.PathEscape(id),
			header:    header,
			maxBytes:  maxUploadAnswerBytes,
			policy:    policyRead,
			readFloor: pollReadFloor,
		})
		if err != nil {
			return nil, nil, err
		}
		answer, err := readUploadAnswer(resp, c.apiKey)
		if err != nil {
			return nil, nil, err
		}
		if answer.UploadID != id {
			return nil, nil, wrapf(ErrUnexpectedResponse, "the API answered for another upload")
		}
		if answer.Status != "queued" {
			return resp, answer, nil
		}
		if time.Since(asked) < uploadQueuePause {
			if err := sleepContext(ctx, uploadQueuePause); err != nil {
				return nil, nil, err
			}
		}
	}
}

func linkLifetime(expiresAt string, header http.Header) time.Duration {
	expires, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return uploadLinkTTL
	}
	now := time.Now()
	if date, err := http.ParseTime(header.Get("Date")); err == nil {
		now = date
	}
	if lifetime := expires.Sub(now); lifetime > 0 && lifetime < uploadLinkTTL {
		return lifetime
	}
	return uploadLinkTTL
}

func (c *Client) putFile(ctx context.Context, slot *uploadSlot, file *inputFile) (bool, error) {
	header := linkHeader()
	header.Set("Content-Type", file.contentType)
	req := &request{
		method:     http.MethodPut,
		url:        slot.url,
		header:     header,
		body:       &payload{size: file.size, open: file.open},
		maxBytes:   maxUploadAnswerBytes,
		policy:     policyUpload,
		validUntil: slot.validUntil,
		target:     "the upload server",
	}
	resp, err := c.send(ctx, req)
	if err != nil {
		return req.linkExpired, err
	}
	if err := checkStatus(resp, c.apiKey); err != nil {

		return req.linkExpired || resp.status == http.StatusForbidden, err
	}

	var answer struct {
		Bytes *int64 `json:"bytes"`
	}
	if json.Unmarshal(resp.body, &answer) == nil && answer.Bytes != nil && *answer.Bytes != file.size {
		return false, wrapf(ErrUnexpectedResponse, "the upload received %d bytes of %s, not %d", *answer.Bytes, file.label, file.size)
	}
	return false, nil
}
