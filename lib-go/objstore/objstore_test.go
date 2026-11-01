package objstore

import (
	"bytes"
	"errors"
	"fmt"
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
		t.Fatal(err)
	}
	return s
}

// objectID hashes content as kind and returns its ID.
func objectID(t *testing.T, kind obj.Kind, content string) obj.ID {
	t.Helper()
	id, err := obj.Hash(kind, []byte(content))
	if err != nil {
		t.Fatal(err)
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
				t.Fatal(err)
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
				t.Fatal(err)
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
				t.Fatal(err)
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
			t.Fatal(err)
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
		t.Fatal(err)
	}
	if err := primary.MoveTo(cache, id); err != nil {
		t.Fatal(err)
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
			t.Fatal(err)
		}
		want = append(want, id)
	}
	// Stray files aren't objects.
	h := want[0].Hex()
	if err := os.WriteFile(filepath.Join(dir, h[:2], "junk"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.List()
	if err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
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
		t.Fatal(err)
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

func TestOpenRemovesTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := os.WriteFile(filepath.Join(s.tmpDir(), "obj-crashed"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
	}
	h := id.Hex()
	path := filepath.Join(dir, h[:2], h[2:])
	for _, tc := range []struct {
		name string
		raw  string
	}{
		// The header says 5 bytes but only 3 follow, e.g. a write cut short.
		{"truncated content", "blob 5\x00hel"},
		// No NUL ends the header, so it can't be told apart from the content.
		{"unterminated header", "blob 5"},
		// The header word isn't one of the object kinds.
		{"unknown kind", "nope 5\x00hello"},
		// The size in the header isn't a number.
		{"bad size", "blob x\x00hello"},
	} {
		if err := os.WriteFile(path, []byte(tc.raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Get(id); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: Get of %q error = %v, want a corruption error", tc.name, tc.raw, err)
		}
	}
	// Put repairs it.
	if err := s.Put(id, obj.Blob, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, got, err := s.Get(id); err != nil || string(got) != "hello" {
		t.Errorf("Get after repair = %q, %v", got, err)
	}
}
