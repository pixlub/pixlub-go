package pixlub

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Result is what a run produced. Outputs lists every file, already downloaded.
type Result struct {
	// Module is the id of the tool that ran.
	Module string
	// ID identifies the run. Quote it when you contact support.
	ID string
	// JobID identifies a long run, or is empty.
	JobID string
	// Outputs lists every file of the run, in the order the API gave them.
	Outputs []Output
	// ExpiresAt is when the download links expire.
	ExpiresAt time.Time
	// Cost is the number of credits the run charged, when the API reports it.
	Cost *int64
	// CreditsBalance is what is left on the account after the run, when reported.
	CreditsBalance *int64
	// Message is an optional note from the tool, such as a warning about the result.
	Message string
}

// Output is one file of a run, already downloaded.
type Output struct {
	// Key is the tool output (see Tool.Outputs), such as "out". In a batch it
	// ends with the rank of the file: "out#0", "out#1"...
	Key string
	// Name is "image" (the produced image), "data" (JSON) or "file" (another format, such as a PDF).
	Name string
	// URL is the download link, valid for one hour.
	URL string
	// ContentType is the media type of Content, such as "image/png".
	ContentType string
	// Bytes is the size of the file, len(Content).
	Bytes int64
	// Content holds the bytes of the file.
	Content []byte
}

func newResult(r *receipt, jobID string, outputs []Output) *Result {
	return &Result{
		Module:         r.module,
		ID:             r.id,
		JobID:          jobID,
		Outputs:        outputs,
		ExpiresAt:      r.expiresAt,
		Cost:           r.cost,
		CreditsBalance: r.balance,
		Message:        r.message,
	}
}

// Output returns the output with this key, such as "out" or "out#1", or else
// the first output with this name ("image", "data" or "file"). It returns nil
// when there is neither.
func (r *Result) Output(keyOrName string) *Output {
	for i := range r.Outputs {
		if r.Outputs[i].Key != "" && r.Outputs[i].Key == keyOrName {
			return &r.Outputs[i]
		}
	}
	return r.byName(keyOrName)
}

func (r *Result) byName(name string) *Output {
	for i := range r.Outputs {
		if r.Outputs[i].Name == name {
			return &r.Outputs[i]
		}
	}
	return nil
}

// File returns the produced file: the image, or else the first output that is
// not data. It returns nil when the run produced neither.
func (r *Result) File() *Output {
	if image := r.byName("image"); image != nil {
		return image
	}
	for i := range r.Outputs {
		if r.Outputs[i].Name != "data" {
			return &r.Outputs[i]
		}
	}
	return nil
}

// Extension returns the file extension of the produced file, such as ".png" or
// ".webp" (removeBgApi), or ".json" when the run produced only data.
func (r *Result) Extension() string {
	if file := r.File(); file != nil {
		return file.Extension()
	}
	return ".json"
}

// Image returns the bytes of the produced file, or nil when the tool only returns data.
func (r *Result) Image() []byte {
	if file := r.File(); file != nil {
		return file.Content
	}
	return nil
}

// Data decodes the "data" output as JSON into v. It fails with ErrNoData when
// the tool returned no data.
func (r *Result) Data(v any) error {
	data := r.byName("data")
	if data == nil {
		return ErrNoData
	}
	if err := json.Unmarshal(data.Content, v); err != nil {
		return wrapf(ErrUnexpectedResponse, "the data output is not valid JSON")
	}
	return nil
}

// Save writes the produced file to path. It refuses to replace a file unless
// overwrite is true.
func (r *Result) Save(path string, overwrite bool) error {
	file := r.File()
	if file == nil {
		return wrapf(ErrNoData, "this run produced no file, only data: read it with Data")
	}
	return file.Save(path, overwrite)
}

// Save writes the content to path, readable by its owner only. It refuses to
// replace a file unless overwrite is true.
func (o *Output) Save(path string, overwrite bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if overwrite {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("pixlub: %s is a symbolic link, and the SDK does not write through links", path)
		}
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(o.Content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// Extension returns the file extension that matches the content type, such as ".png".
func (o *Output) Extension() string {
	return extensionFor(o.ContentType)
}
