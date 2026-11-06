// Package objstore is a content-addressed directory of objects on local disk, used by the service
// and the daemon. Each object is a file objects/<2 hex>/<38 hex> holding its Git header and content,
// uncompressed. The store doesn't hash: callers verify IDs.
package objstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/smukherj1/jj-workspaces/lib-go/obj"
)

// ErrNotFound is returned, wrapped, for an object that isn't in the store.
var ErrNotFound = errors.New("object not found")

// errWriterClosed is returned, wrapped, for using a Writer whose object has been committed or
// cleaned up.
var errWriterClosed = errors.New("object writer is closed")

// maxHeader bounds the Git header: "operation" plus a space, 20 digits and a NUL.
const maxHeader = 32

// Store is an object directory. It's safe for concurrent use within one process, and only one
// process may use a directory at a time.
type Store struct {
	dir string
}

// Open opens the store in dir, creating it if needed, and removes temp files a crash left behind.
func Open(dir string) (*Store, error) {
	s := &Store{dir: dir}
	if err := os.RemoveAll(s.tmpDir()); err != nil {
		return nil, fmt.Errorf("clean up object temp files: %w", err)
	}
	if err := os.MkdirAll(s.tmpDir(), 0o755); err != nil {
		return nil, fmt.Errorf("create object store: %w", err)
	}
	return s, nil
}

// Dir returns the store's directory.
func (s *Store) Dir() string {
	return s.dir
}

func (s *Store) tmpDir() string {
	return filepath.Join(s.dir, "tmp")
}

// path returns the file an object is stored in. It can't fail for an obj.ID today, whose hex is
// always 40 characters, but guards the split against a shorter one.
func (s *Store) path(id obj.ID) (string, error) {
	h := id.Hex()
	if len(h) < 3 {
		return "", fmt.Errorf("object ID %q is too short", h)
	}
	return filepath.Join(s.dir, h[:2], h[2:]), nil
}

// Put stores an object atomically: it writes a temp file, fsyncs it and renames it into place, so
// a crash leaves either the whole object or none of it. Putting an object that's already stored
// overwrites it with the same bytes, which also repairs a corrupt file.
func (s *Store) Put(id obj.ID, kind obj.Kind, content []byte) error {
	w, cleanup, err := s.Create(kind, uint64(len(content)))
	if err != nil {
		return fmt.Errorf("put object %s: %w", id, err)
	}
	defer cleanup()
	if _, err := w.Write(content); err != nil {
		return fmt.Errorf("put object %s: %w", id, err)
	}
	return w.Commit(id)
}

// Writer writes one object's content to a temp file, so that an object doesn't need to fit in
// memory. The object is stored only once Commit succeeds. After Commit, or the cleanup function
// from Create, the Writer is closed and Write and Commit return an error.
type Writer struct {
	store   *Store
	f       *os.File // nil once the temp file has been renamed or removed.
	size    uint64
	written uint64
}

// Create starts writing an object of kind with size bytes of content. It also returns a function
// that discards the object and removes its temp file unless Commit has stored it. Calling it again
// does nothing, so a caller defers it once Create succeeds. On error there's nothing to clean up
// and the function is nil.
func (s *Store) Create(kind obj.Kind, size uint64) (*Writer, func(), error) {
	if !kind.Valid() {
		return nil, nil, fmt.Errorf("create object: invalid kind %v", kind)
	}
	f, err := os.CreateTemp(s.tmpDir(), "obj-")
	if err != nil {
		return nil, nil, fmt.Errorf("create object: create temp file: %w", err)
	}
	w := &Writer{store: s, f: f, size: size}
	if _, err := f.Write(obj.Header(kind, size)); err != nil {
		w.cleanup()
		return nil, nil, fmt.Errorf("create object: write header: %w", err)
	}
	return w, w.cleanup, nil
}

// Write appends content to the object.
func (w *Writer) Write(p []byte) (int, error) {
	if w.f == nil {
		return 0, fmt.Errorf("write object content: %w", errWriterClosed)
	}
	n, err := w.f.Write(p)
	w.written += uint64(n)
	if err != nil {
		return n, fmt.Errorf("write object content: %w", err)
	}
	return n, nil
}

// Commit stores the object under id: it fsyncs the temp file and renames it into place, so a crash
// leaves either the whole object or none of it. If the object is already stored, it's overwritten.
// Commit fails if the content written isn't the size given to Create, and on failure it removes the
// temp file.
func (w *Writer) Commit(id obj.ID) error {
	if err := w.commit(id); err != nil {
		w.cleanup()
		return fmt.Errorf("commit object %s: %w", id, err)
	}
	return nil
}

func (w *Writer) commit(id obj.ID) error {
	if w.f == nil {
		return errWriterClosed
	}
	if w.written != w.size {
		return fmt.Errorf("%d bytes written, but the object is %d", w.written, w.size)
	}
	dst, err := w.store.path(id)
	if err != nil {
		return fmt.Errorf("determine storage path: %w", err)
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create shard directory: %w", err)
	}
	if err := os.Rename(w.f.Name(), dst); err != nil {
		return fmt.Errorf("rename temp file into place: %w", err)
	}
	w.f = nil
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("sync shard directory: %w", err)
	}
	return nil
}

// cleanup discards the object and removes its temp file, if it's still there.
func (w *Writer) cleanup() {
	if w.f == nil {
		return
	}
	w.f.Close()
	os.Remove(w.f.Name())
	w.f = nil
}

// Open returns an object's kind and content size, and a reader of its content that doesn't load it
// into memory. The caller closes the reader. If the object is overwritten or removed meanwhile, the
// reader keeps reading the bytes it opened: the open file holds the old inode until it's closed.
func (s *Store) Open(id obj.ID) (obj.Kind, uint64, io.ReadCloser, error) {
	p, err := s.path(id)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("open object: %w", err)
	}
	f, err := os.Open(p)
	if err != nil {
		return 0, 0, nil, s.wrap(id, err)
	}
	kind, size, err := readHeader(f)
	if err != nil {
		f.Close()
		return 0, 0, nil, fmt.Errorf("read object %s: %w", id, err)
	}
	return kind, size, f, nil
}

// Get returns an object's kind and content.
func (s *Store) Get(id obj.ID) (obj.Kind, []byte, error) {
	kind, size, r, err := s.Open(id)
	if err != nil {
		return 0, nil, err
	}
	defer r.Close()
	content, err := io.ReadAll(r)
	if err != nil {
		return 0, nil, fmt.Errorf("read object %s: %w", id, err)
	}
	if uint64(len(content)) != size {
		return 0, nil, fmt.Errorf("read object %s: header says %d bytes, read %d", id, size,
			len(content))
	}
	return kind, content, nil
}

// Has reports whether an object is stored.
func (s *Store) Has(id obj.ID) (bool, error) {
	p, err := s.path(id)
	if err != nil {
		return false, fmt.Errorf("stat object: %w", err)
	}
	_, err = os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat object %s: %w", id, err)
	}
	return true, nil
}

// Size returns an object's kind and content size, reading only its header.
func (s *Store) Size(id obj.ID) (obj.Kind, uint64, error) {
	kind, size, r, err := s.Open(id)
	if err != nil {
		return 0, 0, err
	}
	r.Close()
	return kind, size, nil
}

// Remove deletes an object. It returns ErrNotFound if the object isn't stored.
func (s *Store) Remove(id obj.ID) error {
	p, err := s.path(id)
	if err != nil {
		return fmt.Errorf("remove object: %w", err)
	}
	if err := os.Remove(p); err != nil {
		return s.wrap(id, err)
	}
	return nil
}

// MoveTo moves an object into another store by rename, so both stores must be on the same
// filesystem. An object already in other is replaced. It returns ErrNotFound if the object isn't
// in s. The daemon's uploader uses it to move an object from the primary store to the cache once
// the service confirms the upload.
func (s *Store) MoveTo(other *Store, id obj.ID) error {
	src, err := s.path(id)
	if err != nil {
		return fmt.Errorf("move object: %w", err)
	}
	dst, err := other.path(id)
	if err != nil {
		return fmt.Errorf("move object: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("move object %s: %w", id, err)
	}
	if err := os.Rename(src, dst); err != nil {
		return s.wrap(id, err)
	}
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("move object %s: %w", id, err)
	}
	if err := syncDir(filepath.Dir(src)); err != nil {
		return fmt.Errorf("move object %s: %w", id, err)
	}
	return nil
}

// List returns the IDs of all stored objects, in no particular order. The daemon's uploader uses it
// at startup to re-queue the objects left in the primary store.
func (s *Store) List() ([]obj.ID, error) {
	shards, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}
	var ids []obj.ID
	for _, shard := range shards {
		if !shard.IsDir() || len(shard.Name()) != 2 {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.dir, shard.Name()))
		if err != nil {
			return nil, fmt.Errorf("list objects: %w", err)
		}
		for _, e := range entries {
			id, err := obj.ParseHex(shard.Name() + e.Name())
			if err != nil || !e.Type().IsRegular() {
				continue
			}
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// wrap describes an error reading an object's file, as ErrNotFound if the file doesn't exist.
func (s *Store) wrap(id obj.ID, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("object %s: %w", id, ErrNotFound)
	}
	return fmt.Errorf("object %s: %w", id, err)
}

// readHeader reads the Git header of an object file, checks it against the file's size and leaves
// f at the start of the content.
func readHeader(f *os.File) (obj.Kind, uint64, error) {
	b := make([]byte, maxHeader)
	n, err := f.ReadAt(b, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, 0, fmt.Errorf("read header: %w", err)
	}
	kind, size, headerLen, err := parseHeader(b[:n])
	if err != nil {
		return 0, 0, fmt.Errorf("parse header: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		return 0, 0, fmt.Errorf("stat file: %w", err)
	}
	if got := uint64(info.Size() - int64(headerLen)); got != size {
		return 0, 0, fmt.Errorf("header says %d bytes, file has %d", size, got)
	}
	if _, err := f.Seek(int64(headerLen), io.SeekStart); err != nil {
		return 0, 0, fmt.Errorf("seek past header: %w", err)
	}
	return kind, size, nil
}

// parseHeader parses the Git header at the start of b and returns the kind, the content size and
// the header's length.
func parseHeader(b []byte) (obj.Kind, uint64, int, error) {
	end := bytes.IndexByte(b, 0)
	if end < 0 {
		return 0, 0, 0, errors.New("no object header")
	}
	word, num, ok := bytes.Cut(b[:end], []byte{' '})
	if !ok {
		return 0, 0, 0, fmt.Errorf("bad object header %q", b[:end])
	}
	kind, err := obj.ParseKind(string(word))
	if err != nil {
		return 0, 0, 0, err
	}
	size, err := strconv.ParseUint(string(num), 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("bad object header %q", b[:end])
	}
	return kind, size, end + 1, nil
}

// syncDir fsyncs a directory so a rename into it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}
