# pixlub-go

Official Go client for the [Pixlub](https://pixlub.com) API. Go 1.23+, standard
library only.

Documentation: [getting started with the API](https://pixlub.com/en/docs/api/getting-started)
and [every module, its inputs and settings](https://pixlub.com/en/docs/modules).

## Install

```bash
go get github.com/pixlub/pixlub-go
```

## Quick start

1. Create an API key in your Pixlub dashboard. API keys need a paid plan.
2. Put it in an environment variable, never in your source code:

   ```bash
   export PIXLUB_API_KEY="pix.xxxxxxxx.xxxxxxxx..."
   ```

3. Run a tool:

   ```go
   package main

   import (
       "context"
       "log"

       pixlub "github.com/pixlub/pixlub-go"
   )

   func main() {
       client, err := pixlub.NewClient()
       if err != nil {
           log.Fatal(err)
       }
       result, err := client.Tools.Run(context.Background(), "removeBgApi", pixlub.RunRequest{
           Image: pixlub.FromPath("photo.jpg"),
       })
       if err != nil {
           log.Fatal(err)
       }
       if err := result.Save("photo-nobg"+result.Extension(), false); err != nil {
           log.Fatal(err)
       }
   }
   ```

`result.Extension()` matches the produced file: `.webp` for `removeBgApi`, `.png`
for most tools. `Save` refuses to replace an existing file unless `overwrite` is
true.

## Results

```go
result.Image()          // bytes of the produced file (nil if the tool only returns data)
result.Data(&faces)     // the data output, decoded as JSON
result.Cost             // *int64: credits charged
result.CreditsBalance   // *int64: credits left
result.Output("out")    // an output by key, or by name ("image", "data", "file")
for _, out := range result.Outputs {
    fmt.Println(out.Key, out.Name, out.ContentType, out.Bytes)
}
```

In a batch, output keys get the rank of the file: `out#0`, `out#1`… Each
`Output.URL` is a download link valid for one hour.

## Tools and settings

```go
tools, err := client.Tools.List(ctx, pixlub.ListOptions{ExecutableOnly: true})
schema, err := client.Tools.Schema(ctx, "flip") // JSON Schema of the settings

client.Tools.Run(ctx, "flip", pixlub.RunRequest{
    Image:  pixlub.FromPath("photo.jpg"),
    Params: map[string]any{"axis": "v"},
})
```

Invalid settings fail with `ErrInvalidRequest`, with the reasons in `APIError.Details`.

## Several files

A tool with several inputs takes `Inputs`, keyed by input id. An input that
takes several files (`ToolInput.Multiple`) takes a list in `InputLists`:

```go
client.Tools.Run(ctx, "mask", pixlub.RunRequest{
    Inputs: map[string]pixlub.Source{
        "image": pixlub.FromPath("photo.png"),
        "mask":  pixlub.FromPath("mask.png"),
    },
})

client.Tools.Run(ctx, "merge", pixlub.RunRequest{
    Inputs:     map[string]pixlub.Source{"base": pixlub.FromPath("bg.png")},
    InputLists: map[string][]pixlub.Source{"overlay": {pixlub.FromPath("1.png"), pixlub.FromPath("2.png")}},
})
```

When `ToolInput.PerFile` is true, each file is a separate run, charged
separately. A palette is passed in `Params`, as a list of colors.

A file comes from `FromPath`, `FromBytes`, `FromReader` or `FromReaderSize`. To
use a file from the web, pass `FromURL("https://...")`; a path is always local.
Large files are handled automatically.

## Limits

| Limit | Value |
| --- | --- |
| File size, by plan | Free 10 MiB, Basic 50 MiB, Pro 3 GiB |
| File size, by tool | images 100 MiB, video and audio 3 GiB, Office 250 MiB |
| Image size | 40 megapixels |
| Files per run | 64 |
| Runs | 30 a minute per account, bursts of 10 |
| Download links | 1 hour |

## Timeouts

`Run` waits for long runs until they finish, up to `WithJobTimeout` (30 minutes
by default), within the deadline of the context. After that it fails with
`ErrJobTimeout`: the run may still finish and may have been charged, so check
it with `GET /v1/jobs/{job_id}` rather than running the tool again.

```go
client, _ := pixlub.NewClient(pixlub.WithTimeout(30*time.Second), pixlub.WithJobTimeout(time.Hour))
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
defer cancel()
```

## Errors

Test errors with `errors.Is`, and read the details with `errors.As` into
`*pixlub.APIError` (`HTTPStatus`, `Code`, `Message`, `MaxBytes`, `MaxPixels`,
`Limit`, `RequiredCredits`, `RetryAfter`, `JobID`) or `*pixlub.ConnectionError`
(`RequestSent`).

| Sentinel | When |
| --- | --- |
| `ErrConfiguration` | Missing or malformed key or base URL. |
| `ErrInvalidInput` | An argument was refused before anything was sent. |
| `ErrUnknownTool` | Unknown tool id. |
| `ErrFileTooLarge` | A file is too large for the tool or your plan. |
| `ErrInvalidRequest` | HTTP 4xx: invalid settings, `image_too_large`… |
| `ErrAuthentication` | HTTP 401: unknown or revoked key. |
| `ErrInsufficientCredits` | HTTP 402: not enough credits. |
| `ErrPermissionDenied` | HTTP 403: not included in your plan. |
| `ErrNotFound` | HTTP 404. |
| `ErrRateLimited` | HTTP 429: too many requests. Never charged. |
| `ErrServer` | HTTP 5xx. |
| `ErrConnection`, `ErrTimeout` | No answer. |
| `ErrJobTimeout` | A run outlived `WithJobTimeout`. |
| `ErrUnexpectedResponse` | The answer could not be read. |

```go
result, err := client.Tools.Run(ctx, "removeBgApi", req)
var apiErr *pixlub.APIError
switch {
case errors.Is(err, pixlub.ErrInsufficientCredits):
    log.Println("top up your credits")
case errors.As(err, &apiErr):
    log.Printf("refused (%d): %s", apiErr.HTTPStatus, apiErr.Message)
case err != nil:
    log.Println("run failed:", err)
}
```

## Retries and credits

The SDK retries a request only when it cannot cause a second charge, up to
`WithMaxRetries` times (2 by default, 0 to disable), and waits as long as the
API asks. Arguments are checked before anything is sent: a mistake costs
nothing. After a timeout or a server error on a run, check your balance before
running it again.

## Configuration

| Option | Environment variable | Default |
| --- | --- | --- |
| `WithAPIKey` | `PIXLUB_API_KEY` | required |
| `WithBaseURL` | `PIXLUB_BASE_URL` | `https://api.pixlub.com` (`http://` only for localhost) |
| `WithTimeout` | | 5 minutes per request |
| `WithJobTimeout` | | 30 minutes |
| `WithMaxRetries` | | 2 (0 to 5) |
| `WithMaxInputBytes` | | 3 GiB |
| `WithHTTPClient` | | a new `http.Client` (pass one for proxies or custom TLS) |

## Examples

[`examples/quickstart`](examples/quickstart/main.go) lists the tools, or removes
the background of an image given on the command line.

## Development

```bash
make check   # format, vet, race tests, example build
make live    # live tests: PIXLUB_API_KEY, PIXLUB_BASE_URL, and PIXLUB_LIVE_IMAGE to run a tool (costs credits)
```

## License

MIT: see [LICENSE](LICENSE).
