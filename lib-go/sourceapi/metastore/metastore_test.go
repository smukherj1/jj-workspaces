package metastore

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/smukherj1/jj-workspaces/lib-go/obj"
)

var (
	blobID = obj.ID{1}
	treeID = obj.ID{2}
)

// openStore opens an empty store whose clock reads the returned time, which a test may move.
func openStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.UnixMilli(1_000_000)
	s.now = func() time.Time { return now }
	return s, &now
}

// checkMeta checks one looked-up entry against what's expected.
func checkMeta(t *testing.T, got, want ObjectMeta) {
	t.Helper()
	if got.ID != want.ID || got.Present != want.Present || got.Kind != want.Kind ||
		got.Size != want.Size || !bytes.Equal(got.SHA256, want.SHA256) ||
		(got.SHA256 == nil) != (want.SHA256 == nil) || !got.UploadedAt.Equal(want.UploadedAt) {
		t.Errorf("GetObjectsMeta entry = %+v, want %+v", got, want)
	}
}

func TestInsertAndGetObjectsMeta(t *testing.T) {
	s, now := openStore(t)
	sum := bytes.Repeat([]byte{7}, 32)
	err := s.InsertObjects(t.Context(), []ObjectRecord{
		{ID: blobID, Kind: obj.Blob, Size: 13, SHA256: sum},
		{ID: treeID, Kind: obj.Tree},
	})
	if err != nil {
		t.Fatalf("InsertObjects: %v", err)
	}

	blob := ObjectMeta{ID: blobID, Present: true, Kind: obj.Blob, Size: 13, SHA256: sum, UploadedAt: *now}
	tree := ObjectMeta{ID: treeID, Present: true, Kind: obj.Tree, UploadedAt: *now}
	absent := ObjectMeta{ID: obj.ID{3}}
	for _, tc := range []struct {
		name string
		want []ObjectMeta
	}{
		{"blob with its SHA-256", []ObjectMeta{blob}},
		{"tree without a SHA-256", []ObjectMeta{tree}},
		{"absent", []ObjectMeta{absent}},
		{"request order, repeats included", []ObjectMeta{absent, tree, blob, tree}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idList := make([]obj.ID, len(tc.want))
			for i, m := range tc.want {
				idList[i] = m.ID
			}
			got, err := s.GetObjectsMeta(t.Context(), idList)
			if err != nil {
				t.Fatalf("GetObjectsMeta: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("GetObjectsMeta returned %d entries, want %d", len(got), len(tc.want))
			}
			for i := range got {
				checkMeta(t, got[i], tc.want[i])
			}
		})
	}
}

func TestInsertObjects_keepsExistingRow(t *testing.T) {
	s, now := openStore(t)
	first := *now
	records := []ObjectRecord{{ID: treeID, Kind: obj.Tree}}
	if err := s.InsertObjects(t.Context(), records); err != nil {
		t.Fatalf("InsertObjects: %v", err)
	}
	*now = now.Add(time.Hour)
	if err := s.InsertObjects(t.Context(), records); err != nil {
		t.Fatalf("second InsertObjects: %v", err)
	}

	got, err := s.GetObjectsMeta(t.Context(), []obj.ID{treeID})
	if err != nil {
		t.Fatalf("GetObjectsMeta: %v", err)
	}
	checkMeta(t, got[0], ObjectMeta{ID: treeID, Present: true, Kind: obj.Tree, UploadedAt: first})
}
