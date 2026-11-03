package pixlub

import (
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"sync"
)

type formField struct {
	name  string
	value string
}

type form struct {
	boundary string
	fields   []formField
	files    []*inputFile
	size     int64

	mu sync.Mutex

	writing chan struct{}
}

func newForm(fields []formField, files []*inputFile) (*form, error) {
	f := &form{boundary: multipart.NewWriter(io.Discard).Boundary(), fields: fields, files: files}
	var counter countingWriter
	writer := multipart.NewWriter(&counter)
	if err := writer.SetBoundary(f.boundary); err != nil {
		return nil, wrapf(ErrInvalidInput, "cannot encode the form")
	}
	if err := f.write(writer, &counter); err != nil {
		return nil, err
	}
	f.size = counter.n
	return f, nil
}

func (f *form) contentType() string {
	return mime.FormatMediaType("multipart/form-data", map[string]string{"boundary": f.boundary})
}

func (f *form) payload() *payload {
	return &payload{size: f.size, open: f.open}
}

func (f *form) open() (io.ReadCloser, error) {
	f.mu.Lock()
	previous := f.writing
	done := make(chan struct{})
	f.writing = done
	f.mu.Unlock()
	if previous != nil {
		<-previous
	}

	reader, pipe := io.Pipe()
	go func() {
		defer close(done)
		writer := multipart.NewWriter(pipe)
		err := writer.SetBoundary(f.boundary)
		if err == nil {
			err = f.write(writer, nil)
		}

		_ = pipe.CloseWithError(err)
	}()
	return reader, nil
}

func (f *form) wait() {
	f.mu.Lock()
	done := f.writing
	f.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (f *form) write(writer *multipart.Writer, counter *countingWriter) error {
	for _, field := range f.fields {
		if err := writer.WriteField(field.name, field.value); err != nil {
			return err
		}
	}
	for _, file := range f.files {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data",
			map[string]string{"name": file.field, "filename": file.filename}))
		header.Set("Content-Type", file.contentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return err
		}
		if counter != nil {
			counter.n += file.size
			continue
		}
		if err := copyInput(part, file); err != nil {
			return err
		}
	}
	return writer.Close()
}

func copyInput(w io.Writer, file *inputFile) error {
	source, err := file.open()
	if err != nil {
		return err
	}
	defer source.Close()
	_, err = io.Copy(w, source)
	return err
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}
