// Package objstore is a content-addressed directory of objects on local disk, used by the service
// and the daemon. Each object is a file objects/<2 hex>/<38 hex> holding its Git header and content,
// uncompressed. The store doesn't hash: callers verify IDs.
package objstore

import (
	"bufio"
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
	if !kind.Valid() {
		return fmt.Errorf("put object %s: invalid kind %v", id, kind)
	}
	if err := s.put(id, kind, content); err != nil {
		return fmt.Errorf("put object %s: %w", id, err)
	}
	return nil
}

func (s *Store) put(id obj.ID, kind obj.Kind, content []byte) (err error) {
	dst, err := s.path(id)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.tmpDir(), "obj-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(obj.Header(kind, len(content))); err != nil {
		return err
	}
	if _, err = f.Write(content); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err = os.Rename(tmp, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

// Get returns an object's kind and content.
func (s *Store) Get(id obj.ID) (obj.Kind, []byte, error) {
	p, err := s.path(id)
	if err != nil {
		return 0, nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, nil, s.wrap(id, err)
	}
	kind, size, n, err := parseHeader(b)
	if err != nil {
		return 0, nil, fmt.Errorf("read object %s: %w", id, err)
	}
	content := b[n:]
	if uint64(len(content)) != size {
		return 0, nil, fmt.Errorf("read object %s: header says %d bytes, file has %d", id, size,
			len(content))
	}
	return kind, content, nil
}

// Has reports whether an object is stored.
func (s *Store) Has(id obj.ID) (bool, error) {
	p, err := s.path(id)
	if err != nil {
		return false, err
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
	p, err := s.path(id)
	if err != nil {
		return 0, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		return 0, 0, s.wrap(id, err)
	}
	defer f.Close()
	b, err := bufio.NewReaderSize(f, maxHeader).Peek(maxHeader)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, 0, fmt.Errorf("read object %s: %w", id, err)
	}
	kind, size, _, err := parseHeader(b)
	if err != nil {
		return 0, 0, fmt.Errorf("read object %s: %w", id, err)
	}
	return kind, size, nil
}

// Remove deletes an object. It returns ErrNotFound if the object isn't stored.
func (s *Store) Remove(id obj.ID) error {
	p, err := s.path(id)
	if err != nil {
		return err
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
		return err
	}
	dst, err := other.path(id)
	if err != nil {
		return err
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
		return err
	}
	defer d.Close()
	return d.Sync()
}
