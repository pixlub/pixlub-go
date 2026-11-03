package pixlub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// Tools lists, describes and runs tools. Access it as client.Tools.
type Tools struct {
	client *Client
}

// ListOptions filters the result of Tools.List.
type ListOptions struct {
	// ExecutableOnly leaves out tools that cannot be run through the API.
	ExecutableOnly bool
}

// RunRequest describes one run. Set Image for a tool with a single input, or
// Inputs and InputLists for inputs keyed by name (see Tool.Inputs). Params
// holds the tool's settings, as JSON values (see Tools.Schema).
type RunRequest struct {
	// Image is the file of a tool that takes one input.
	Image Source
	// Inputs gives one file per input name.
	Inputs map[string]Source
	// InputLists gives several files per input name, for inputs that take
	// several (ToolInput.Multiple), up to ToolInput.MaxFiles each, and 64 files
	// per run in all. A name may be in Inputs or in InputLists, not in both.
	InputLists map[string][]Source
	Params     map[string]any
}

var toolIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func checkToolID(id string) error {
	if !toolIDPattern.MatchString(id) {
		return wrapf(ErrInvalidInput,
			"invalid tool id %q: ids use letters, digits, '-' and '_', such as 'removeBgApi'", truncate(id, 80))
	}
	return nil
}

// List returns the tools.
func (t *Tools) List(ctx context.Context, opts ListOptions) ([]Tool, error) {
	tools, err := t.client.fetchCatalog(ctx)
	if err != nil {
		return nil, err
	}
	listed := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if opts.ExecutableOnly && !tool.Executable {
			continue
		}

		listed = append(listed, cloneTool(tool))
	}
	return listed, nil
}

// Retrieve returns one tool, or ErrUnknownTool.
func (t *Tools) Retrieve(ctx context.Context, id string) (*Tool, error) {
	if err := checkToolID(id); err != nil {
		return nil, err
	}
	tool, err := t.client.findTool(ctx, id)
	if err != nil {
		return nil, err
	}
	if tool == nil {
		return nil, wrapf(ErrUnknownTool,
			"'%s' is not in the tool catalog: call Tools.List to see the ids", id)
	}
	return tool, nil
}

// Schema returns the JSON Schema of a tool's Params.
func (t *Tools) Schema(ctx context.Context, id string) (map[string]any, error) {
	if err := checkToolID(id); err != nil {
		return nil, err
	}
	body, err := t.client.getJSON(ctx, "/v1/modules/"+id+"/schema", maxSchemaBytes)
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(body, &schema); err != nil || schema == nil {
		return nil, wrapf(ErrUnexpectedResponse, "the parameter schema is not a JSON object")
	}
	return schema, nil
}

// Run runs a tool and downloads every file it produced. Large files are
// uploaded automatically, and a long run is awaited until WithJobTimeout. Set
// a deadline on ctx to bound the whole call.
func (t *Tools) Run(ctx context.Context, id string, req RunRequest) (*Result, error) {
	if err := checkToolID(id); err != nil {
		return nil, err
	}
	c := t.client
	tool, err := c.findTool(ctx, id)
	if err != nil {
		return nil, err
	}
	if tool == nil {

		return nil, wrapf(ErrUnknownTool,
			"'%s' is not in the tool catalog: call Tools.List to see the ids", id)
	}
	planned, err := planRun(id, tool, req)
	if err != nil {
		return nil, err
	}
	paramsJSON, err := encodeParams(req.Params)
	if err != nil {
		return nil, err
	}
	inputs, err := prepareInputs(planned, c.maxInputBytes)
	if err != nil {
		return nil, err
	}
	defer inputs.close()
	route, err := routeRun(tool, inputs, directUploadLimit)
	if err != nil {
		return nil, err
	}

	resp, err := c.submit(ctx, id, tool, paramsJSON, inputs, route)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(resp, c.apiKey); err != nil {
		return nil, err
	}

	var ref runRef
	if resp.status == http.StatusAccepted {

		final, jobID, err := c.followJob(ctx, resp)
		if err != nil {
			return nil, err
		}
		resp, ref.jobID = final, jobID
	}
	rcpt, err := parseReceipt(resp.body, c.baseURL, c.apiKey)
	if err != nil {
		return nil, chargedError(ref, "its answer could not be read", err)
	}
	ref.runID = rcpt.id
	outputs, err := c.downloadOutputs(ctx, rcpt, ref)
	if err != nil {
		return nil, err
	}
	return newResult(rcpt, ref.jobID, outputs), nil
}

func (c *Client) submit(ctx context.Context, id string, tool *Tool, paramsJSON string, inputs *inputSet, route runRoute) (*response, error) {
	if route.upload == nil {
		fields := []formField{{name: "params", value: paramsJSON}}
		if inputs.sourceURL != "" {
			fields = append(fields, formField{name: "source_url", value: inputs.sourceURL})
		}
		resp, err := c.postRun(ctx, id, fields, route.direct, policyCreate)
		if err != nil || resp.status != http.StatusRequestEntityTooLarge || errorCode(resp.body) != "use_uploads" {
			return resp, err
		}

		limit := directUploadLimit
		if maxBytes := newAPIError(resp, c.apiKey).MaxBytes; maxBytes != nil && *maxBytes > 0 && *maxBytes < limit {
			limit = *maxBytes
		}
		fallback, routeErr := routeUpload(tool, inputs, limit)
		if routeErr != nil {

			return resp, nil
		}
		route = fallback
	}

	uploadID, err := c.deposit(ctx, id, route.upload)
	if err != nil {
		return nil, err
	}
	fields := []formField{{name: "params", value: paramsJSON}, {name: "upload_id", value: uploadID}}
	return c.postRun(ctx, id, fields, route.direct, policyClaim)
}

func (c *Client) postRun(ctx context.Context, id string, fields []formField, files []*inputFile, policy retryPolicy) (*response, error) {
	body, err := newForm(fields, files)
	if err != nil {
		return nil, err
	}

	defer body.wait()
	header := c.apiHeader("application/json")
	header.Set("Content-Type", body.contentType())
	return c.send(ctx, &request{
		method:    http.MethodPost,
		url:       c.baseURL + "/v1/modules/" + id,
		header:    header,
		body:      body.payload(),
		maxBytes:  maxJSONBytes,
		policy:    policy,
		readFloor: runReadFloor,
		charges:   true,
	})
}

func (c *Client) downloadOutputs(ctx context.Context, rcpt *receipt, ref runRef) ([]Output, error) {
	if len(rcpt.outputs) > maxRunOutputs {
		return nil, chargedError(ref, fmt.Sprintf("it announced %d files, more than the %d the SDK accepts",
			len(rcpt.outputs), maxRunOutputs), nil)
	}
	var announced int64
	for _, link := range rcpt.outputs {
		if link.bytes > maxOutputBytes {
			return nil, chargedError(ref, fmt.Sprintf("one of its files is %d bytes, more than the %d bytes the SDK downloads: "+
				"get it from its link instead", link.bytes, maxOutputBytes), nil)
		}
		announced += link.bytes
	}
	if announced > maxRunOutputBytes {
		return nil, chargedError(ref, "its results are larger than the SDK accepts", nil)
	}

	outputs := make([]Output, 0, len(rcpt.outputs))
	var downloaded int64
	for _, link := range rcpt.outputs {
		download, err := c.send(ctx, &request{
			method:   http.MethodGet,
			url:      link.url,
			header:   linkHeader(),
			maxBytes: maxOutputBytes,
			policy:   policyRead,
			target:   "the result host",
		})
		if err != nil {
			return nil, chargedError(ref, "a result could not be downloaded", err)
		}
		downloaded += int64(len(download.body))
		if downloaded > maxRunOutputBytes {
			return nil, chargedError(ref, "its results are larger than the SDK accepts", nil)
		}
		output, err := outputFromResponse(link, download, c.apiKey)
		if err != nil {
			return nil, chargedError(ref, "a result could not be read", err)
		}
		outputs = append(outputs, output)
	}
	return outputs, nil
}

func encodeParams(params map[string]any) (string, error) {
	if params == nil {
		return "{}", nil
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return "", wrapf(ErrInvalidInput, "Params must contain only JSON values: %v", err)
	}
	return string(encoded), nil
}

type catalogCache struct {
	mu        sync.Mutex
	tools     []Tool
	fetchedAt time.Time
	inflight  *catalogFetch
}

type catalogFetch struct {
	done  chan struct{}
	tools []Tool
	err   error
}

func (c *catalogCache) snapshot() ([]Tool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tools, c.tools != nil && time.Since(c.fetchedAt) < catalogTTL
}

func (c *Client) fetchCatalog(ctx context.Context) ([]Tool, error) {
	cache := c.catalog
	cache.mu.Lock()
	call := cache.inflight
	if call == nil {
		call = &catalogFetch{done: make(chan struct{})}
		cache.inflight = call

		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		go c.runCatalogFetch(fetchCtx, cancel, call)
	}
	cache.mu.Unlock()

	select {
	case <-call.done:
		return call.tools, call.err
	case <-ctx.Done():
		return nil, fmt.Errorf("pixlub: request cancelled: %w", ctx.Err())
	}
}

func (c *Client) runCatalogFetch(ctx context.Context, cancel context.CancelFunc, call *catalogFetch) {
	defer cancel()
	call.tools, call.err = c.loadCatalog(ctx)

	cache := c.catalog
	cache.mu.Lock()
	if call.err == nil {
		cache.tools = call.tools
		cache.fetchedAt = time.Now()
	}
	cache.inflight = nil
	cache.mu.Unlock()
	close(call.done)
}

func (c *Client) loadCatalog(ctx context.Context) ([]Tool, error) {
	body, err := c.getJSON(ctx, "/v1/modules", maxJSONBytes)
	if err != nil {
		return nil, err
	}
	return parseCatalog(body)
}

func (c *Client) findTool(ctx context.Context, id string) (*Tool, error) {
	tools, fresh := c.catalog.snapshot()
	if !fresh || indexOfTool(tools, id) < 0 {
		refreshed, err := c.fetchCatalog(ctx)
		if err != nil {
			return nil, err
		}
		tools = refreshed
	}
	i := indexOfTool(tools, id)
	if i < 0 {
		return nil, nil
	}
	tool := cloneTool(tools[i])
	return &tool, nil
}

func indexOfTool(tools []Tool, id string) int {
	for i := range tools {
		if tools[i].ID == id {
			return i
		}
	}
	return -1
}

func cloneTool(tool Tool) Tool {
	tool.Inputs = append([]ToolInput(nil), tool.Inputs...)
	for i := range tool.Inputs {
		tool.Inputs[i].Accept = append([]string(nil), tool.Inputs[i].Accept...)
	}
	tool.Outputs = append([]ToolOutput(nil), tool.Outputs...)
	for i := range tool.Outputs {
		tool.Outputs[i].ContentTypes = append([]string(nil), tool.Outputs[i].ContentTypes...)
	}
	return tool
}
