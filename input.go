package pixlub

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
)

const maxRequestFiles = 64

// Source is one input file. Build it with FromPath, FromBytes, FromReader,
// FromReaderSize or FromURL. The zero value means "not set".
type Source struct {
	kind   sourceKind
	name   string
	path   string
	data   []byte
	reader io.Reader
	size   int64
	url    string
}

type sourceKind int

const (
	sourceUnset sourceKind = iota
	sourcePath
	sourceBytes
	sourceReader
	sourceReaderSize
	sourceURL
)

// FromPath sends the local file at path.
func FromPath(path string) Source {
	return Source{kind: sourcePath, path: path}
}

// FromBytes sends a file already in memory. name is optional.
func FromBytes(name string, data []byte) Source {
	return Source{kind: sourceBytes, name: name, data: data}
}

// FromReader sends everything reader returns. name is optional. A reader that
// cannot seek is copied first; use FromPath for a file on disk.
func FromReader(name string, reader io.Reader) Source {
	return Source{kind: sourceReader, name: name, reader: reader}
}

// FromReaderSize sends exactly size bytes read from reader. name is optional.
func FromReaderSize(name string, reader io.Reader, size int64) Source {
	return Source{kind: sourceReaderSize, name: name, reader: reader, size: size}
}

// FromURL uses a public https URL as the main input of the tool, instead of a
// local file: port 443, no credentials, 2048 characters at most.
func FromURL(rawURL string) Source {
	return Source{kind: sourceURL, url: rawURL}
}

func (s Source) isSet() bool { return s.kind != sourceUnset }

var unsafeFilenameChars = regexp.MustCompile(`[\x00-\x1f"\\]`)

func detectType(head []byte, name string) string {
	if kind := sniffImageType(head); kind != "" {
		return kind
	}
	if kind := mimeFromName(name); kind != "application/octet-stream" {
		return kind
	}
	if len(head) > 0 {
		if media, _, err := mime.ParseMediaType(http.DetectContentType(head)); err == nil {
			return media
		}
	}
	return "application/octet-stream"
}

func sniffImageType(data []byte) string {
	head := data[:min(16, len(data))]
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(head, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "image/gif"
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return "image/webp"
	case bytes.HasPrefix(head, []byte("BM")):
		return "image/bmp"
	case bytes.HasPrefix(head, []byte("II*\x00")), bytes.HasPrefix(head, []byte("MM\x00*")):
		return "image/tiff"
	case len(head) >= 12 && string(head[4:8]) == "ftyp" && (string(head[8:12]) == "avif" || string(head[8:12]) == "avis"):
		return "image/avif"
	}
	return ""
}

func mimeFromName(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ext == "" {
		return "application/octet-stream"
	}
	if kind := mime.TypeByExtension(ext); kind != "" {
		if media, _, err := mime.ParseMediaType(kind); err == nil {
			return media
		}
	}
	return "application/octet-stream"
}

func baseName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		return name[i+1:]
	}
	return name
}

var extensions = map[string]string{
	"image/png":        ".png",
	"image/jpeg":       ".jpg",
	"image/webp":       ".webp",
	"image/gif":        ".gif",
	"image/bmp":        ".bmp",
	"image/tiff":       ".tiff",
	"image/avif":       ".avif",
	"application/json": ".json",
	"application/pdf":  ".pdf",
	"application/zip":  ".zip",
	"video/mp4":        ".mp4",
	"video/webm":       ".webm",
	"audio/mpeg":       ".mp3",
	"audio/wav":        ".wav",
	"text/html":        ".html",
	"text/plain":       ".txt",
}

func extensionFor(contentType string) string {
	if ext, ok := extensions[contentType]; ok {
		return ext
	}
	return ".img"
}

type plannedInput struct {
	name    string
	sources []Source
}

func planRun(id string, tool *Tool, req RunRequest) ([]plannedInput, error) {
	if tool != nil && !tool.Executable {
		return nil, wrapf(ErrInvalidInput, "'%s' cannot be run through the API", id)
	}
	named := req.Inputs != nil || req.InputLists != nil
	if req.Image.isSet() && named {
		return nil, wrapf(ErrInvalidInput, "set either Image or Inputs and InputLists, not both")
	}

	var inputs []plannedInput
	switch {
	case req.Image.isSet():
		if tool == nil {
			return nil, wrapf(ErrUnknownTool,
				"'%s' is not in the tool catalog, so the name of its input is unknown: use Inputs instead", id)
		}
		name, err := singleImageInput(tool)
		if err != nil {
			return nil, err
		}
		inputs = []plannedInput{{name: name, sources: []Source{req.Image}}}
	case named:
		planned, err := namedInputs(id, tool, req.Inputs, req.InputLists)
		if err != nil {
			return nil, err
		}
		inputs = planned
	default:
		if tool != nil {
			var required []string
			for _, in := range tool.Inputs {
				if in.Required {
					required = append(required, in.ID)
				}
			}
			if len(required) > 0 {
				return nil, wrapf(ErrInvalidInput,
					"'%s' needs a file for: %s: set Image or Inputs", id, strings.Join(required, ", "))
			}
		}
	}

	if err := checkFileCounts(id, tool, inputs); err != nil {
		return nil, err
	}
	if err := checkSourceURLs(id, tool, inputs); err != nil {
		return nil, err
	}
	return inputs, nil
}

func singleImageInput(tool *Tool) (string, error) {
	if len(tool.Inputs) == 0 {
		return "", wrapf(ErrInvalidInput, "'%s' takes no file: pass its settings in Params only", tool.ID)
	}
	var required []ToolInput
	for _, in := range tool.Inputs {
		if in.Required {
			required = append(required, in)
		}
	}
	candidates := required
	if len(candidates) == 0 {
		candidates = tool.Inputs
	}
	if len(candidates) == 1 {
		return candidates[0].ID, nil
	}
	return "", wrapf(ErrInvalidInput,
		"'%s' takes several inputs (%s): use Inputs with one key per input", tool.ID, strings.Join(inputIDs(tool), ", "))
}

var fieldPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func namedInputs(id string, tool *Tool, single map[string]Source, lists map[string][]Source) ([]plannedInput, error) {
	sources := make(map[string][]Source, len(single)+len(lists))
	for name, src := range single {
		sources[name] = []Source{src}
	}
	for name, list := range lists {
		if _, ok := sources[name]; ok {
			return nil, wrapf(ErrInvalidInput, "input '%s' is set in both Inputs and InputLists", truncate(name, 80))
		}
		if len(list) == 0 {
			return nil, wrapf(ErrInvalidInput, "InputLists[%q] is empty: leave the input out instead", truncate(name, 80))
		}
		sources[name] = list
	}
	if len(sources) == 0 {
		return nil, wrapf(ErrInvalidInput, "Inputs must not be empty, for example {\"image\": FromPath(...)}")
	}

	known := map[string]bool{}
	if tool != nil {
		for _, in := range tool.Inputs {
			known[in.ID] = true
		}
	}

	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)

	planned := make([]plannedInput, 0, len(names))
	for _, name := range names {
		if !fieldPattern.MatchString(name) {
			return nil, wrapf(ErrInvalidInput, "invalid input name %q", truncate(name, 80))
		}
		if tool != nil && !known[name] {
			return nil, wrapf(ErrInvalidInput, "'%s' has no input named '%s'; its inputs are: %s",
				id, name, strings.Join(inputIDs(tool), ", "))
		}
		planned = append(planned, plannedInput{name: name, sources: sources[name]})
	}

	if tool != nil {
		var missing []string
		for _, in := range tool.Inputs {
			if _, ok := sources[in.ID]; in.Required && !ok {
				missing = append(missing, in.ID)
			}
		}
		if len(missing) > 0 {
			return nil, wrapf(ErrInvalidInput, "'%s' is missing required input(s): %s", id, strings.Join(missing, ", "))
		}
	}
	return planned, nil
}

func checkFileCounts(id string, tool *Tool, inputs []plannedInput) error {
	total := 0
	for _, in := range inputs {
		count := len(in.sources)
		for _, src := range in.sources {
			if src.kind == sourceURL {
				count--
			}
		}
		total += count
		spec := findInput(tool, in.name)
		if spec == nil || len(in.sources) <= 1 {
			continue
		}
		if !spec.Multiple {
			return wrapf(ErrInvalidInput, "input '%s' of '%s' takes one file, not %d", in.name, id, len(in.sources))
		}
		if spec.MaxFiles > 0 && len(in.sources) > spec.MaxFiles {
			return wrapf(ErrInvalidInput, "input '%s' of '%s' takes at most %d files, not %d",
				in.name, id, spec.MaxFiles, len(in.sources))
		}
	}
	if total > maxRequestFiles {
		return wrapf(ErrInvalidInput, "a run accepts at most %d files, not %d", maxRequestFiles, total)
	}
	return nil
}

func checkSourceURLs(id string, tool *Tool, inputs []plannedInput) error {
	found := 0
	for _, in := range inputs {
		for _, src := range in.sources {
			if src.kind != sourceURL {
				continue
			}
			found++
			if err := checkSourceURL(src.url); err != nil {
				return err
			}
			if len(in.sources) > 1 {
				return wrapf(ErrInvalidInput, "a source URL must be the only source of input '%s'", in.name)
			}
			if main := primaryInput(tool); tool != nil && in.name != main {
				return wrapf(ErrInvalidInput, "a source URL can only fill the main input of '%s', '%s', not '%s'",
					id, main, in.name)
			}
		}
	}
	if found > 1 {
		return wrapf(ErrInvalidInput, "a run takes one source URL at most, not %d", found)
	}
	return nil
}

func primaryInput(tool *Tool) string {
	if tool == nil || len(tool.Inputs) == 0 {
		return ""
	}
	for _, in := range tool.Inputs {
		if in.Required {
			return in.ID
		}
	}
	return tool.Inputs[0].ID
}

func findInput(tool *Tool, name string) *ToolInput {
	if tool == nil {
		return nil
	}
	for i := range tool.Inputs {
		if tool.Inputs[i].ID == name {
			return &tool.Inputs[i]
		}
	}
	return nil
}

func inputIDs(tool *Tool) []string {
	ids := make([]string, 0, len(tool.Inputs))
	for _, in := range tool.Inputs {
		ids = append(ids, in.ID)
	}
	return ids
}

func truncate(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n]
}
