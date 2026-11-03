package pixlub

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)

func TestSniffImageType(t *testing.T) {
	cases := map[string][]byte{
		"image/png":  pngBytes,
		"image/jpeg": []byte("\xff\xd8\xff\xe0 rest"),
		"image/gif":  []byte("GIF89a......"),
		"image/webp": []byte("RIFF\x00\x00\x00\x00WEBPVP8 "),
		"image/bmp":  []byte("BM......"),
		"image/tiff": []byte("II*\x00......"),
		"image/avif": []byte("\x00\x00\x00\x18ftypavif....."),
		"":           []byte("plain text"),
	}
	for want, data := range cases {
		if got := sniffImageType(data); got != want {
			t.Errorf("sniffImageType(%q) = %q, want %q", data[:min(8, len(data))], got, want)
		}
	}
}

func TestSanitizeFilenameAndExtensions(t *testing.T) {
	up, err := prepareFile(FromBytes(`evil"name.png`, pngBytes), "image", "input 'image'", 1<<20)
	if err != nil {
		t.Fatalf("prepareUpload: %v", err)
	}
	if up.filename != "evil_name.png" {
		t.Errorf("filename = %q", up.filename)
	}

	nested, err := prepareFile(FromBytes(`../x/y.png`, pngBytes), "image", "input 'image'", 1<<20)
	if err != nil || nested.filename != "y.png" {
		t.Errorf("nested filename = %q, %v", nested.filename, err)
	}
	if up.contentType != "image/png" {
		t.Errorf("content type = %q", up.contentType)
	}

	anonymous, err := prepareFile(FromBytes("", pngBytes), "base", "input 'base'", 1<<20)
	if err != nil {
		t.Fatalf("prepareUpload: %v", err)
	}
	if anonymous.filename != "base.png" {
		t.Errorf("anonymous filename = %q, want base.png", anonymous.filename)
	}
}

func TestPrepareFileRefusesBadInputs(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.png")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.png")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]Source{
		"too large":   FromPath(big),
		"empty file":  FromPath(empty),
		"directory":   FromPath(dir),
		"missing":     FromPath(filepath.Join(dir, "nope.png")),
		"empty bytes": FromBytes("a.png", nil),
		"unset":       {},
	}
	for name, src := range cases {
		limit := int64(1024)
		if _, err := prepareFile(src, "image", "input 'image'", limit); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: error = %v, want ErrInvalidInput", name, err)
		}
	}
}

func TestPlanRunSingleImage(t *testing.T) {
	tool := &Tool{ID: "flip", Executable: true, Inputs: []ToolInput{{ID: "image", Required: true}}}
	planned, err := planRun("flip", tool, RunRequest{Image: FromBytes("a.png", pngBytes)})
	if err != nil || len(planned) != 1 || planned[0].name != "image" {
		t.Fatalf("planRun = %+v, %v", planned, err)
	}
}

func TestPlanRunRefusals(t *testing.T) {
	mask := &Tool{ID: "mask", Executable: true, Inputs: []ToolInput{
		{ID: "image", Required: true}, {ID: "mask", Required: true},
	}}
	noInput := &Tool{ID: "checker", Executable: true}
	browserOnly := &Tool{ID: "editor", Executable: false}

	cases := []struct {
		name string
		id   string
		tool *Tool
		req  RunRequest
		want error
	}{
		{"not executable", "editor", browserOnly, RunRequest{Image: FromBytes("", pngBytes)}, ErrInvalidInput},
		{"image and inputs", "mask", mask, RunRequest{Image: FromBytes("", pngBytes), Inputs: map[string]Source{"image": FromBytes("", pngBytes)}}, ErrInvalidInput},
		{"several images without names", "mask", mask, RunRequest{Image: FromBytes("", pngBytes)}, ErrInvalidInput},
		{"unknown tool with image", "custom", nil, RunRequest{Image: FromBytes("", pngBytes)}, ErrUnknownTool},
		{"unknown input name", "mask", mask, RunRequest{Inputs: map[string]Source{"base": FromBytes("", pngBytes)}}, ErrInvalidInput},
		{"missing required input", "mask", mask, RunRequest{Inputs: map[string]Source{"image": FromBytes("", pngBytes)}}, ErrInvalidInput},
		{"no input needed", "checker", noInput, RunRequest{Image: FromBytes("", pngBytes)}, ErrInvalidInput},
		{"empty inputs map", "mask", mask, RunRequest{Inputs: map[string]Source{}}, ErrInvalidInput},
		{"invalid field name", "mask", mask, RunRequest{Inputs: map[string]Source{"bad-name": FromBytes("", pngBytes)}}, ErrInvalidInput},
		{"no image at all", "mask", mask, RunRequest{}, ErrInvalidInput},
	}
	for _, tc := range cases {
		if _, err := planRun(tc.id, tc.tool, tc.req); !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestPlanRunNamedInputsAreSorted(t *testing.T) {
	mask := &Tool{ID: "mask", Executable: true, Inputs: []ToolInput{
		{ID: "image", Required: true}, {ID: "mask", Required: true},
	}}
	req := RunRequest{Inputs: map[string]Source{"mask": FromBytes("", pngBytes), "image": FromBytes("", pngBytes)}}
	planned, err := planRun("mask", mask, req)
	if err != nil || len(planned) != 2 || planned[0].name != "image" || planned[1].name != "mask" {
		t.Fatalf("planRun order = %+v, %v", planned, err)
	}
}

func TestEncodeParams(t *testing.T) {
	if got, _ := encodeParams(nil); got != "{}" {
		t.Errorf("nil params = %q", got)
	}
	if got, _ := encodeParams(map[string]any{"axis": "v", "width": 512}); got != `{"axis":"v","width":512}` {
		t.Errorf("params = %q", got)
	}
	if _, err := encodeParams(map[string]any{"bad": func() {}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("non-JSON params error = %v, want ErrInvalidInput", err)
	}
}

func TestCheckToolID(t *testing.T) {
	for _, id := range []string{"removeBgApi", "flip", "a-b_c"} {
		if err := checkToolID(id); err != nil {
			t.Errorf("checkToolID(%q) = %v", id, err)
		}
	}
	for _, id := range []string{"", "../internal", "a/b", "x?y=1", "a.b", "-lead"} {
		if err := checkToolID(id); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("checkToolID(%q) = %v, want ErrInvalidInput", id, err)
		}
	}
}

func TestPlanRunInputLists(t *testing.T) {
	collage := &Tool{ID: "collage", Executable: true, Inputs: []ToolInput{
		{ID: "images", Required: true, Multiple: true, MaxFiles: 3},
		{ID: "background"},
	}}
	batch := &Tool{ID: "batchFlip", Executable: true, Inputs: []ToolInput{
		{ID: "image", Required: true, Multiple: true, PerFile: true},
	}}
	files := func(n int) []Source {
		list := make([]Source, n)
		for i := range list {
			list[i] = FromBytes("", pngBytes)
		}
		return list
	}

	planned, err := planRun("collage", collage, RunRequest{
		Inputs:     map[string]Source{"background": FromBytes("", pngBytes)},
		InputLists: map[string][]Source{"images": files(3)},
	})
	if err != nil || len(planned) != 2 || planned[1].name != "images" || len(planned[1].sources) != 3 {
		t.Fatalf("planRun = %+v, %v", planned, err)
	}

	refused := map[string]struct {
		tool *Tool
		req  RunRequest
	}{
		"over max_files":      {collage, RunRequest{InputLists: map[string][]Source{"images": files(4)}}},
		"list on single":      {collage, RunRequest{InputLists: map[string][]Source{"images": files(1), "background": files(2)}}},
		"in both maps":        {collage, RunRequest{Inputs: map[string]Source{"images": FromBytes("", pngBytes)}, InputLists: map[string][]Source{"images": files(2)}}},
		"empty list":          {collage, RunRequest{InputLists: map[string][]Source{"images": {}}}},
		"over 64 per request": {batch, RunRequest{InputLists: map[string][]Source{"image": files(65)}}},
		"image and lists":     {batch, RunRequest{Image: FromBytes("", pngBytes), InputLists: map[string][]Source{"image": files(2)}}},
	}
	for name, tc := range refused {
		if _, err := planRun(tc.tool.ID, tc.tool, tc.req); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: error = %v, want ErrInvalidInput", name, err)
		}
	}
	if _, err := planRun("batchFlip", batch, RunRequest{InputLists: map[string][]Source{"image": files(64)}}); err != nil {
		t.Errorf("64 files: %v", err)
	}
}

func TestPrimaryInput(t *testing.T) {
	cases := map[string]*Tool{
		"mask":  {Inputs: []ToolInput{{ID: "base"}, {ID: "mask", Required: true}, {ID: "x", Required: true}}},
		"first": {Inputs: []ToolInput{{ID: "first"}, {ID: "second"}}},
		"":      {},
	}
	for want, tool := range cases {
		if got := primaryInput(tool); got != want {
			t.Errorf("primaryInput = %q, want %q", got, want)
		}
	}
}

func TestDetectTypeBeyondImages(t *testing.T) {
	if got := detectType([]byte("%PDF-1.7\n"), "doc"); got != "application/pdf" {
		t.Errorf("PDF = %q", got)
	}
	if got := detectType([]byte("\x00\x00\x00\x00"), "clip.json"); got != "application/json" {
		t.Errorf("by extension = %q", got)
	}
	if got := detectType(nil, ""); got != "application/octet-stream" {
		t.Errorf("unknown = %q", got)
	}
}

func TestLocalSizeLimitMatchesFileTooLarge(t *testing.T) {
	_, err := prepareFile(FromBytes("a.png", pngBytes), "image", "input 'image'", 10)
	if !errors.Is(err, ErrFileTooLarge) || !errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrAPI) {
		t.Fatalf("error = %v, want ErrFileTooLarge and ErrInvalidInput, not ErrAPI", err)
	}
}
