package objstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smukherj1/jj-workspaces/lib-go/obj"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	return s
}

// objectID hashes content as kind and returns its ID.
func objectID(t *testing.T, kind obj.Kind, content string) obj.ID {
	t.Helper()
	id, err := obj.Hash(kind, []byte(content))
	if err != nil {
		t.Fatalf("hash %s object %q: %v", kind, content, err)
	}
	return id
}

// TestObjectLifecycle takes one object of each kind through the store's lifecycle:
//
//   - An empty store doesn't have the object: Has is false, and Get and Size return ErrNotFound.
//   - Put succeeds, and Get, Has and Size then return the object. Its file is the Git header plus
//     content at <2 hex>/<38 hex>.
//   - The object can still be read after the store is reopened.
//   - Remove deletes it: Has is false and Get returns ErrNotFound. Removing it again returns
//     ErrNotFound.
func TestObjectLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    obj.Kind
		content string
	}{
		{"commit", obj.Commit, "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n"},
		{"tree", obj.Tree, "100644 f\x00" + strings.Repeat("\xab", obj.IDSize)},
		{"blob", obj.Blob, "hello\n"},
		{"operation", obj.Operation, "op"},
		{"view", obj.View, "view"},
		{"empty tree", obj.Tree, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := open(t, dir)
			id := objectID(t, tc.kind, tc.content)

			if has, err := s.Has(id); err != nil || has {
				t.Fatalf("Has before Put = %v, %v", has, err)
			}
			if _, _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get before Put error = %v, want ErrNotFound", err)
			}
			if _, _, err := s.Size(id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Size before Put error = %v, want ErrNotFound", err)
			}

			if err := s.Put(id, tc.kind, []byte(tc.content)); err != nil {
				t.Fatalf("Put of a new object: %v", err)
			}
			if kind, got, err := s.Get(id); err != nil || kind != tc.kind || string(got) != tc.content {
				t.Fatalf("Get = %v, %q, %v", kind, got, err)
			}
			if has, err := s.Has(id); err != nil || !has {
				t.Fatalf("Has = %v, %v", has, err)
			}
			if kind, size, err := s.Size(id); err != nil || kind != tc.kind ||
				size != uint64(len(tc.content)) {
				t.Fatalf("Size = %v, %d, %v", kind, size, err)
			}
			h := id.Hex()
			raw, err := os.ReadFile(filepath.Join(dir, h[:2], h[2:]))
			if err != nil {
				t.Fatalf("read the object's file after Put: %v", err)
			}
			want := fmt.Sprintf("%s %d\x00%s", tc.kind, len(tc.content), tc.content)
			if string(raw) != want {
				t.Errorf("file = %q, want %q", raw, want)
			}

			if kind, got, err := open(t, dir).Get(id); err != nil || kind != tc.kind ||
				string(got) != tc.content {
				t.Fatalf("Get after reopen = %v, %q, %v", kind, got, err)
			}

			if err := s.Remove(id); err != nil {
				t.Fatalf("Remove of a stored object: %v", err)
			}
			if has, err := s.Has(id); err != nil || has {
				t.Errorf("Has after Remove = %v, %v", has, err)
			}
			if _, _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get after Remove error = %v, want ErrNotFound", err)
			}
			if err := s.Remove(id); !errors.Is(err, ErrNotFound) {
				t.Errorf("second Remove error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestPutIsIdempotent(t *testing.T) {
	s := open(t, t.TempDir())
	id := objectID(t, obj.Commit, "c")
	for range 2 {
		if err := s.Put(id, obj.Commit, []byte("c")); err != nil {
			t.Fatalf("Put of the same object, stored or not: %v", err)
		}
	}
	if kind, got, err := s.Get(id); err != nil || kind != obj.Commit || string(got) != "c" {
		t.Fatalf("Get = %v, %q, %v", kind, got, err)
	}
	if ids, err := s.List(); err != nil || len(ids) != 1 {
		t.Fatalf("List = %v, %v", ids, err)
	}
}

func TestPutRejectsInvalidKind(t *testing.T) {
	s := open(t, t.TempDir())
	if err := s.Put(obj.ID{1}, 0, nil); err == nil {
		t.Error("Put with kind 0 succeeded")
	}
}

func TestMoveTo(t *testing.T) {
	root := t.TempDir()
	primary := open(t, filepath.Join(root, "primary"))
	cache := open(t, filepath.Join(root, "cache"))
	id := objectID(t, obj.Operation, "op")
	if err := primary.Put(id, obj.Operation, []byte("op")); err != nil {
		t.Fatalf("Put into the primary store: %v", err)
	}
	if err := primary.MoveTo(cache, id); err != nil {
		t.Fatalf("MoveTo of an object in the primary store: %v", err)
	}
	if has, _ := primary.Has(id); has {
		t.Error("object still in primary after MoveTo")
	}
	if kind, got, err := cache.Get(id); err != nil || kind != obj.Operation || string(got) != "op" {
		t.Fatalf("cache.Get = %v, %q, %v", kind, got, err)
	}
	if err := primary.MoveTo(cache, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("MoveTo of a missing object error = %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if ids, err := s.List(); err != nil || len(ids) != 0 {
		t.Fatalf("List of empty store = %v, %v", ids, err)
	}
	var want []obj.ID
	for _, c := range []string{"a", "b", "c", "d"} {
		id := objectID(t, obj.View, c)
		if err := s.Put(id, obj.View, []byte(c)); err != nil {
			t.Fatalf("Put of object %q: %v", c, err)
		}
		want = append(want, id)
	}
	// Stray files aren't objects.
	h := want[0].Hex()
	if err := os.WriteFile(filepath.Join(dir, h[:2], "junk"), nil, 0o644); err != nil {
		t.Fatalf("write a stray file in a shard: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), nil, 0o644); err != nil {
		t.Fatalf("write a stray file at the top level: %v", err)
	}

	got, err := s.List()
	if err != nil {
		t.Fatalf("List of a store with stray files: %v", err)
	}
	cmp := func(a, b obj.ID) int { return bytes.Compare(a[:], b[:]) }
	slices.SortFunc(got, cmp)
	slices.SortFunc(want, cmp)
	if !slices.Equal(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
}

// tmpFiles returns the names of the temp files in the store.
func tmpFiles(t *testing.T, s *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(s.tmpDir())
	if err != nil {
		t.Fatalf("read the temp dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestFailedPutLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	id := objectID(t, obj.Blob, "x")
	// A non-empty directory where the object belongs makes the rename fail.
	h := id.Hex()
	if err := os.MkdirAll(filepath.Join(dir, h[:2], h[2:], "blocker"), 0o755); err != nil {
		t.Fatalf("create a directory where the object belongs: %v", err)
	}
	if err := s.Put(id, obj.Blob, []byte("x")); err == nil {
		t.Fatal("Put succeeded")
	}
	if names := tmpFiles(t, s); len(names) != 0 {
		t.Errorf("temp files left after a failed Put: %v", names)
	}
	if ids, err := s.List(); err != nil || len(ids) != 0 {
		t.Errorf("List after a failed Put = %v, %v", ids, err)
	}
}

// rawFile returns the file of an object in the store in dir.
func rawFile(t *testing.T, dir string, id obj.ID) []byte {
	t.Helper()
	h := id.Hex()
	raw, err := os.ReadFile(filepath.Join(dir, h[:2], h[2:]))
	if err != nil {
		t.Fatalf("read the file of object %s: %v", id, err)
	}
	return raw
}

// TestWriterMatchesPut stores the same object with Writer in one store and with Put in another. A
// Writer's file must be byte for byte what Put writes, and the object must appear only at Commit.
func TestWriterMatchesPut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    obj.Kind
		content string
	}{
		{"blob", obj.Blob, "hello, world\n"},
		{"empty tree", obj.Tree, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Setup: Put the object into its own store, for the file to compare against.
			putDir, writerDir := t.TempDir(), t.TempDir()
			id := objectID(t, tc.kind, tc.content)
			if err := open(t, putDir).Put(id, tc.kind, []byte(tc.content)); err != nil {
				t.Fatalf("Put of a new object into an empty store: %v", err)
			}

			// Test: write the same content through a Writer in two parts.
			s := open(t, writerDir)
			w, cleanup, err := s.Create(tc.kind, uint64(len(tc.content)))
			if err != nil {
				t.Fatalf("Create of a %s object: %v", tc.kind, err)
			}
			defer cleanup()
			half := len(tc.content) / 2
			for _, part := range []string{tc.content[:half], tc.content[half:]} {
				if _, err := w.Write([]byte(part)); err != nil {
					t.Fatalf("Write of %q to an open Writer: %v", part, err)
				}
			}

			// Expect: nothing is stored until Commit.
			if has, err := s.Has(id); err != nil || has {
				t.Errorf("Has before Commit = %v, %v", has, err)
			}

			// Test: Commit the full content, then clean up as a deferred cleanup would.
			if err := w.Commit(id); err != nil {
				t.Fatalf("Commit of content of the declared size: %v", err)
			}
			cleanup()

			// Expect: cleanup left the object alone, its file is what Put wrote, and the temp file has
			// been renamed away.
			if got, want := rawFile(t, writerDir, id), rawFile(t, putDir, id); !bytes.Equal(got, want) {
				t.Errorf("file = %q, want %q as Put writes", got, want)
			}
			if names := tmpFiles(t, s); len(names) != 0 {
				t.Errorf("temp files left after Commit: %v", names)
			}
		})
	}
}

func TestCommitOverwrites(t *testing.T) {
	s := open(t, t.TempDir())
	id := objectID(t, obj.Blob, "x")
	for range 2 {
		w, cleanup, err := s.Create(obj.Blob, 1)
		if err != nil {
			t.Fatalf("Create of a blob: %v", err)
		}
		defer cleanup()
		w.Write([]byte("x"))
		if err := w.Commit(id); err != nil {
			t.Fatalf("Commit of an object, stored or not: %v", err)
		}
	}
	if _, got, err := s.Get(id); err != nil || string(got) != "x" {
		t.Errorf("Get = %q, %v", got, err)
	}
}

// TestAbandonedWriterLeavesNothing finishes a Writer without storing its object: by cleaning up, or
// by a Commit that fails because the wrong number of bytes was written. No object and no temp file
// may remain. TestWriterMatchesPut covers the other outcome, a Commit that succeeds.
func TestAbandonedWriterLeavesNothing(t *testing.T) {
	id := objectID(t, obj.Blob, "hello")
	for _, tc := range []struct {
		name   string
		finish func(t *testing.T, w *Writer, cleanup func())
	}{
		{"cleanup", func(t *testing.T, w *Writer, cleanup func()) {
			cleanup()
			cleanup()
		}},
		{"too few bytes", func(t *testing.T, w *Writer, cleanup func()) {
			w.Write([]byte("hell"))
			if err := w.Commit(id); err == nil {
				t.Error("Commit of too few bytes succeeded")
			}
		}},
		{"too many bytes", func(t *testing.T, w *Writer, cleanup func()) {
			w.Write([]byte("hello!"))
			if err := w.Commit(id); err == nil {
				t.Error("Commit of too many bytes succeeded")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Setup: start writing a 5-byte blob.
			s := open(t, t.TempDir())
			w, cleanup, err := s.Create(obj.Blob, 5)
			if err != nil {
				t.Fatalf("Create of a 5-byte blob: %v", err)
			}

			// Test: abandon the Writer as the case does.
			tc.finish(t, w, cleanup)

			// Expect: the store holds neither the object nor its temp file.
			if ids, err := s.List(); err != nil || len(ids) != 0 {
				t.Errorf("List = %v, %v", ids, err)
			}
			if names := tmpFiles(t, s); len(names) != 0 {
				t.Errorf("temp files left: %v", names)
			}
		})
	}
}

// A Writer that has finished rejects further use, however it finished, and a later Commit must not
// disturb what the first one stored. The empty object means the size check can't catch this.
func TestClosedWriter(t *testing.T) {
	id := objectID(t, obj.Blob, "")
	for _, tc := range []struct {
		name   string
		finish func(t *testing.T, w *Writer, cleanup func())
		stored bool
	}{
		{"cleanup", func(t *testing.T, w *Writer, cleanup func()) { cleanup() }, false},
		{"commit", func(t *testing.T, w *Writer, cleanup func()) {
			if err := w.Commit(id); err != nil {
				t.Fatalf("Commit of an empty blob: %v", err)
			}
		}, true},
		{"failed commit", func(t *testing.T, w *Writer, cleanup func()) {
			w.Write([]byte("too long"))
			if err := w.Commit(id); err == nil {
				t.Fatal("Commit of the wrong size succeeded")
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t, t.TempDir())
			w, cleanup, err := s.Create(obj.Blob, 0)
			if err != nil {
				t.Fatalf("Create of an empty blob: %v", err)
			}
			defer cleanup()
			tc.finish(t, w, cleanup)

			if n, err := w.Write([]byte("x")); n != 0 || !errors.Is(err, errWriterClosed) {
				t.Errorf("Write = %d, %v, want errWriterClosed", n, err)
			}
			if err := w.Commit(id); !errors.Is(err, errWriterClosed) {
				t.Errorf("Commit error = %v, want errWriterClosed", err)
			}
			if has, err := s.Has(id); err != nil || has != tc.stored {
				t.Errorf("Has = %v, %v, want %v", has, err, tc.stored)
			}
		})
	}
}

func TestCreateRejectsInvalidKind(t *testing.T) {
	s := open(t, t.TempDir())
	if _, _, err := s.Create(0, 0); err == nil {
		t.Error("Create with kind 0 succeeded")
	}
	if names := tmpFiles(t, s); len(names) != 0 {
		t.Errorf("temp files left: %v", names)
	}
}

func TestOpen(t *testing.T) {
	s := open(t, t.TempDir())
	id := objectID(t, obj.Commit, "content")
	if _, _, _, err := s.Open(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open before Put error = %v, want ErrNotFound", err)
	}
	if err := s.Put(id, obj.Commit, []byte("content")); err != nil {
		t.Fatalf("Put of a new object: %v", err)
	}
	kind, size, r, err := s.Open(id)
	if err != nil {
		t.Fatalf("Open of a stored object: %v", err)
	}
	defer r.Close()
	if kind != obj.Commit || size != 7 {
		t.Errorf("Open = %v, %d", kind, size)
	}
	if got, err := io.ReadAll(r); err != nil || string(got) != "content" {
		t.Errorf("read %q, %v", got, err)
	}
}

// An open reader holds the inode it opened, so it reads the old bytes to the end whatever happens to
// the object's name.
func TestReaderOutlivesObject(t *testing.T) {
	const content = "original content"
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, s *Store, id obj.ID)
	}{
		{"removed", func(t *testing.T, s *Store, id obj.ID) {
			if err := s.Remove(id); err != nil {
				t.Fatalf("Remove of an object with an open reader: %v", err)
			}
		}},
		// Different bytes, which a content-addressed store never writes in practice, show that the
		// reader isn't following the name.
		{"overwritten", func(t *testing.T, s *Store, id obj.ID) {
			if err := s.Put(id, obj.Blob, []byte("replacement")); err != nil {
				t.Fatalf("Put over an object with an open reader: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t, t.TempDir())
			id := objectID(t, obj.Blob, content)
			if err := s.Put(id, obj.Blob, []byte(content)); err != nil {
				t.Fatalf("Put of a new object: %v", err)
			}
			_, _, r, err := s.Open(id)
			if err != nil {
				t.Fatalf("Open of a stored object: %v", err)
			}
			defer r.Close()
			tc.change(t, s, id)
			if got, err := io.ReadAll(r); err != nil || string(got) != content {
				t.Errorf("read %q, %v, want %q", got, err, content)
			}
		})
	}
}

func TestOpenRemovesTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := os.WriteFile(filepath.Join(s.tmpDir(), "obj-crashed"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("write a temp file as a crash would leave it: %v", err)
	}
	if names := tmpFiles(t, open(t, dir)); len(names) != 0 {
		t.Errorf("temp files after reopen: %v", names)
	}
}

func TestCorruptFile(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	id := objectID(t, obj.Blob, "hello")
	if err := s.Put(id, obj.Blob, []byte("hello")); err != nil {
		t.Fatalf("Put of a new object: %v", err)
	}
	h := id.Hex()
	path := filepath.Join(dir, h[:2], h[2:])
	for _, tc := range []struct {
		name string
		raw  string
	}{
		// The header says 5 bytes but only 3 follow, e.g. a write cut short.
		{"truncated content", "blob 5\x00hel"},
		// More follows than the header says.
		{"trailing content", "blob 5\x00hello!"},
		// No NUL ends the header, so it can't be told apart from the content.
		{"unterminated header", "blob 5"},
		// The header word isn't one of the object kinds.
		{"unknown kind", "nope 5\x00hello"},
		// The size in the header isn't a number.
		{"bad size", "blob x\x00hello"},
	} {
		if err := os.WriteFile(path, []byte(tc.raw), 0o644); err != nil {
			t.Fatalf("%s: overwrite the object's file: %v", tc.name, err)
		}
		if _, _, err := s.Get(id); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: Get of %q error = %v, want a corruption error", tc.name, tc.raw, err)
		}
		if _, _, _, err := s.Open(id); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: Open of %q error = %v, want a corruption error", tc.name, tc.raw, err)
		}
	}
	// Put repairs it.
	if err := s.Put(id, obj.Blob, []byte("hello")); err != nil {
		t.Fatalf("Put to repair a corrupt file: %v", err)
	}
	if _, got, err := s.Get(id); err != nil || string(got) != "hello" {
		t.Errorf("Get after repair = %q, %v", got, err)
	}
}
