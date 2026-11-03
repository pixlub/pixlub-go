package pixlub

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	sniffBytes = 512

	spoolMemoryMax = 8 << 20
)

type inputFile struct {
	field       string
	label       string
	filename    string
	contentType string
	size        int64

	open func() (io.ReadCloser, error)

	cleanup func()
}

type inputSet struct {
	files []*inputFile

	sourceURL string
}

func (s *inputSet) total() int64 {
	var sum int64
	for _, file := range s.files {
		sum += file.size
	}
	return sum
}

func (s *inputSet) close() {
	for _, file := range s.files {
		if file.cleanup != nil {
			file.cleanup()
		}
	}
}

type inputReadError struct{ message string }

func (e *inputReadError) Error() string { return "pixlub: invalid input: " + e.message }
func (e *inputReadError) Unwrap() error { return ErrInvalidInput }

func prepareInputs(planned []plannedInput, maxBytes int64) (*inputSet, error) {
	set := &inputSet{}
	for _, in := range planned {
		for i, src := range in.sources {
			if src.kind == sourceURL {
				set.sourceURL = src.url
				continue
			}
			label := "input '" + in.name + "'"
			if len(in.sources) > 1 {
				label = fmt.Sprintf("input '%s' #%d", in.name, i+1)
			}
			file, err := prepareFile(src, in.name, label, maxBytes)
			if err != nil {
				set.close()
				return nil, err
			}
			set.files = append(set.files, file)
		}
	}
	return set, nil
}

func prepareFile(src Source, field, label string, maxBytes int64) (*inputFile, error) {
	var (
		file *inputFile
		head []byte
		name string
		err  error
	)
	switch src.kind {
	case sourcePath:
		file, head, err = pathFile(src.path, label, maxBytes)
		name = filepath.Base(src.path)
	case sourceBytes:
		file, head, err = bytesFile(src.data, label, maxBytes)
		name = src.name
	case sourceReader:
		file, head, err = readerFile(src.reader, -1, label, maxBytes)
		name = src.name
	case sourceReaderSize:
		if src.size <= 0 {
			return nil, wrapf(ErrInvalidInput, "%s: FromReaderSize needs a positive size", label)
		}
		file, head, err = readerFile(src.reader, src.size, label, maxBytes)
		name = src.name
	default:
		return nil, wrapf(ErrInvalidInput, "%s is not set", label)
	}
	if err != nil {
		return nil, err
	}

	file.field = field
	file.label = label
	file.contentType = detectType(head, name)
	filename := unsafeFilenameChars.ReplaceAllString(baseName(name), "_")
	filename = strings.TrimSpace(filename)
	if filename == "" {
		filename = field + extensionFor(file.contentType)
	}
	file.filename = filename
	return file, nil
}

func checkSize(label string, size, maxBytes int64) error {
	if size == 0 {
		return wrapf(ErrInvalidInput, "%s is empty", label)
	}
	if size > maxBytes {
		return tooLargef("%s is %d bytes; the limit is %d bytes (see WithMaxInputBytes)", label, size, maxBytes)
	}
	return nil
}

func pathFile(name, label string, maxBytes int64) (*inputFile, []byte, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, nil, wrapf(ErrInvalidInput, "cannot read %s", name)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, wrapf(ErrInvalidInput, "%s is not a regular file", name)
	}
	if err := checkSize(label, info.Size(), maxBytes); err != nil {
		return nil, nil, err
	}
	size := info.Size()

	opened, err := os.Open(name)
	if err != nil {
		return nil, nil, wrapf(ErrInvalidInput, "cannot read %s", name)
	}
	head := make([]byte, min(size, sniffBytes))
	n, err := io.ReadFull(opened, head)
	_ = opened.Close()
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, nil, wrapf(ErrInvalidInput, "cannot read %s", name)
	}

	open := func() (io.ReadCloser, error) {
		f, err := os.Open(name)
		if err != nil {
			return nil, &inputReadError{message: "cannot read " + name + " any more"}
		}

		if info, err := f.Stat(); err != nil || info.Size() != size {
			_ = f.Close()
			return nil, &inputReadError{message: name + " changed while it was being sent"}
		}
		return &readCloser{Reader: &exactReader{source: f, left: size, label: label}, closer: f}, nil
	}
	return &inputFile{size: size, open: open}, head[:n], nil
}

func bytesFile(data []byte, label string, maxBytes int64) (*inputFile, []byte, error) {
	if err := checkSize(label, int64(len(data)), maxBytes); err != nil {
		return nil, nil, err
	}
	open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	return &inputFile{size: int64(len(data)), open: open}, data[:min(len(data), sniffBytes)], nil
}

func readerFile(reader io.Reader, size int64, label string, maxBytes int64) (*inputFile, []byte, error) {
	if reader == nil {
		return nil, nil, wrapf(ErrInvalidInput, "%s: the reader is nil", label)
	}
	if size > 0 {
		if err := checkSize(label, size, maxBytes); err != nil {
			return nil, nil, err
		}
	}
	if seeker, ok := reader.(io.ReadSeeker); ok {
		return seekerFile(seeker, size, label, maxBytes)
	}
	return spoolFile(reader, size, label, maxBytes)
}

func seekerFile(seeker io.ReadSeeker, size int64, label string, maxBytes int64) (*inputFile, []byte, error) {
	start, err := seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, nil, wrapf(ErrInvalidInput, "%s: cannot seek in the reader", label)
	}
	end, err := seeker.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, nil, wrapf(ErrInvalidInput, "%s: cannot seek in the reader", label)
	}
	left := end - start
	if size > 0 && left != size {
		return nil, nil, wrapf(ErrInvalidInput, "%s holds %d bytes, not the %d bytes given", label, left, size)
	}
	if err := checkSize(label, left, maxBytes); err != nil {
		return nil, nil, err
	}
	if _, err := seeker.Seek(start, io.SeekStart); err != nil {
		return nil, nil, wrapf(ErrInvalidInput, "%s: cannot seek in the reader", label)
	}
	head := make([]byte, min(left, sniffBytes))
	n, err := io.ReadFull(seeker, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, nil, wrapf(ErrInvalidInput, "cannot read %s", label)
	}

	open := func() (io.ReadCloser, error) {
		if _, err := seeker.Seek(start, io.SeekStart); err != nil {
			return nil, &inputReadError{message: "cannot rewind " + label}
		}
		return &guardedReader{source: &exactReader{source: seeker, left: left, label: label}}, nil
	}
	return &inputFile{size: left, open: open}, head[:n], nil
}

func spoolFile(reader io.Reader, size int64, label string, maxBytes int64) (*inputFile, []byte, error) {
	var prefix []byte
	switch {
	case size > 0 && size <= spoolMemoryMax:
		data := make([]byte, size)
		if n, err := io.ReadFull(reader, data); err != nil {
			return nil, nil, endedEarly(label, int64(n), size, err)
		}
		return bytesFile(data, label, maxBytes)
	case size < 0:

		window := min(int64(spoolMemoryMax), maxBytes)
		data, err := io.ReadAll(io.LimitReader(reader, window+1))
		if err != nil {
			return nil, nil, wrapf(ErrInvalidInput, "cannot read %s", label)
		}
		if int64(len(data)) <= window {
			return bytesFile(data, label, maxBytes)
		}
		if window == maxBytes {
			return nil, nil, tooLargef("%s is larger than the limit of %d bytes (see WithMaxInputBytes)", label, maxBytes)
		}
		prefix = data
	}

	temp, err := os.CreateTemp("", "pixlub-input-*")
	if err != nil {
		return nil, nil, wrapf(ErrInvalidInput, "cannot copy %s into a temporary file", label)
	}
	tempName := temp.Name()
	cleanup := func() { _ = os.Remove(tempName) }
	fail := func(err error) (*inputFile, []byte, error) {
		_ = temp.Close()
		cleanup()
		return nil, nil, err
	}

	want := size
	if size < 0 {
		want = maxBytes + 1
	}
	if _, err := temp.Write(prefix); err != nil {
		return fail(wrapf(ErrInvalidInput, "cannot copy %s into a temporary file", label))
	}
	copied, err := io.Copy(temp, io.LimitReader(reader, want-int64(len(prefix))))
	if err != nil {
		return fail(wrapf(ErrInvalidInput, "cannot copy %s into a temporary file", label))
	}
	total := int64(len(prefix)) + copied
	switch {
	case size > 0 && total < size:
		return fail(endedEarly(label, total, size, nil))
	case size < 0 && total > maxBytes:
		return fail(tooLargef("%s is larger than the limit of %d bytes (see WithMaxInputBytes)", label, maxBytes))
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return nil, nil, wrapf(ErrInvalidInput, "cannot copy %s into a temporary file", label)
	}

	file, head, err := pathFile(tempName, label, maxBytes)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	file.cleanup = cleanup
	return file, head, nil
}

func endedEarly(label string, got, size int64, err error) error {
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return wrapf(ErrInvalidInput, "cannot read %s", label)
	}
	return wrapf(ErrInvalidInput, "%s ended after %d bytes, not the %d bytes given", label, got, size)
}

type exactReader struct {
	source io.Reader
	left   int64
	label  string
}

func (r *exactReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.source.Read(p)
	r.left -= int64(n)
	switch {
	case err == io.EOF && r.left > 0:
		return n, &inputReadError{message: r.label + " ended before its announced size"}
	case err != nil && err != io.EOF:
		return n, &inputReadError{message: "cannot read " + r.label}
	case r.left == 0:
		return n, io.EOF
	}
	return n, nil
}

type readCloser struct {
	io.Reader
	closer io.Closer
}

func (r *readCloser) Close() error { return r.closer.Close() }

type guardedReader struct {
	mu     sync.Mutex
	source io.Reader
	closed bool
}

func (r *guardedReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	return r.source.Read(p)
}

func (r *guardedReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}
