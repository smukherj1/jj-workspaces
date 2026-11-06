package obj

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/pjbgf/sha1cd"

	sourceapiv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/sourceapi/v1"
)

// sampleCommit is a commit in jj's Git encoding. Its ID is from `git hash-object -t commit`.
const sampleCommit = "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
	"author Test User <test@example.com> 1700000000 +0000\n" +
	"committer Test User <test@example.com> 1700000000 +0000\n" +
	"change-id zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz\n" +
	"\n" +
	"A sample commit\n"

func TestHashMatchesGit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    Kind
		content string
		want    string
	}{
		{"empty blob", Blob, "", "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"},
		{"empty tree", Tree, "", "4b825dc642cb6eb9a060e54bf8d69288fbee4904"},
		{"sample commit", Commit, sampleCommit, "a30012bb56a3542d332d9c2bf4a53e207bde619a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := Hash(tc.kind, []byte(tc.content))
			if err != nil {
				t.Fatal(err)
			}
			if got := id.Hex(); got != tc.want {
				t.Errorf("Hash = %s, want %s", got, tc.want)
			}
		})
	}
}

// shatteredPrefix is the first 1 KiB of shattered-1.pdf from https://shattered.io, which holds the
// collision blocks. Copied from github.com/pjbgf/sha1cd's test data.
func shatteredPrefix(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/shattered-1-prefix.bin")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The attack needs its blocks at their original offsets, which the Git header of a real object
// moves, so this Hasher has no header.
func TestHasherDetectsSHAttered(t *testing.T) {
	h := &Hasher{sha1: sha1cd.New().(collisionResistantHash)}
	prefix := shatteredPrefix(t)
	h.Write(prefix[:100])
	h.Write(prefix[100:])
	if _, err := h.Sum(); !errors.Is(err, ErrCollision) {
		t.Errorf("Sum of shattered-1 prefix error = %v, want ErrCollision", err)
	}
}

// Streaming a blob to the Hasher gives the same digest as hashing it in one shot, whatever the chunk
// size. The content spans many SHA-1 blocks, so chunks land on and across block boundaries.
func TestHasherStreamingSupport(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789"), 10_000)
	wantID, err := Hash(Blob, content)
	if err != nil {
		t.Fatal(err)
	}
	wantSHA256 := sha256.Sum256(content)
	for _, chunkSize := range []int{10, 100} {
		h, err := NewHasher(Blob, uint64(len(content)))
		if err != nil {
			t.Fatal(err)
		}
		for chunk := range slices.Chunk(content, chunkSize) {
			h.Write(chunk)
		}
		d, err := h.Sum()
		if err != nil {
			t.Fatal(err)
		}
		if d.ID != wantID {
			t.Errorf("chunk size %d: ID = %s, want %s", chunkSize, d.ID, wantID)
		}
		if !bytes.Equal(d.SHA256, wantSHA256[:]) {
			t.Errorf("chunk size %d: SHA-256 = %x, want %x", chunkSize, d.SHA256, wantSHA256)
		}
	}
}

func TestDigestHasSHA256OnlyForBlobs(t *testing.T) {
	content := []byte("content")
	for _, kind := range []Kind{Commit, Tree, Blob, Operation, View} {
		want, err := Hash(kind, content)
		if err != nil {
			t.Fatal(err)
		}
		h, err := NewHasher(kind, uint64(len(content)))
		if err != nil {
			t.Fatal(err)
		}
		h.Write(content)
		d, err := h.Sum()
		if err != nil {
			t.Fatal(err)
		}
		if d.ID != want {
			t.Errorf("%v: ID = %s, want %s", kind, d.ID, want)
		}
		var wantSHA256 []byte
		if kind == Blob {
			sum := sha256.Sum256(content)
			wantSHA256 = sum[:]
		}
		if !bytes.Equal(d.SHA256, wantSHA256) {
			t.Errorf("%v: SHA-256 = %x, want %x", kind, d.SHA256, wantSHA256)
		}
	}
}

func TestNewHasherRejectsInvalidKind(t *testing.T) {
	if _, err := NewHasher(0, 0); err == nil {
		t.Error("NewHasher(0) succeeded")
	}
}

// The Git header shifts the SHAttered blocks off their block boundary and changes the chaining
// value before them, so stored as a blob the PDF is no attack on Git's IDs. Git's own sha1dc gives
// the same ID.
func TestHashOfSHAtteredBlob(t *testing.T) {
	id, err := Hash(Blob, shatteredPrefix(t))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := id.Hex(), "84e514d8dd26d9e550697ff49f7d816ca7b655a2"; got != want {
		t.Errorf("Hash = %s, want %s", got, want)
	}
}

func TestHashRejectsInvalidKind(t *testing.T) {
	if _, err := Hash(0, nil); err == nil {
		t.Error("Hash(0) succeeded")
	}
}

func TestKind(t *testing.T) {
	for _, tc := range []struct {
		kind  Kind
		word  string
		proto sourceapiv1.ObjectKind
	}{
		{Commit, "commit", sourceapiv1.ObjectKind_OBJECT_KIND_COMMIT},
		{Tree, "tree", sourceapiv1.ObjectKind_OBJECT_KIND_TREE},
		{Blob, "blob", sourceapiv1.ObjectKind_OBJECT_KIND_BLOB},
		{Operation, "operation", sourceapiv1.ObjectKind_OBJECT_KIND_OPERATION},
		{View, "view", sourceapiv1.ObjectKind_OBJECT_KIND_VIEW},
	} {
		if got := tc.kind.String(); got != tc.word {
			t.Errorf("%d.String() = %q, want %q", tc.kind, got, tc.word)
		}
		if got, err := ParseKind(tc.word); err != nil || got != tc.kind {
			t.Errorf("ParseKind(%q) = %v, %v, want %v", tc.word, got, err, tc.kind)
		}
		if got := tc.kind.Proto(); got != tc.proto {
			t.Errorf("%v.Proto() = %v, want %v", tc.kind, got, tc.proto)
		}
		if got, err := KindFromProto(tc.proto); err != nil || got != tc.kind {
			t.Errorf("KindFromProto(%v) = %v, %v, want %v", tc.proto, got, err, tc.kind)
		}
	}
	if _, err := ParseKind("tag"); err == nil {
		t.Error(`ParseKind("tag") succeeded`)
	}
	if _, err := KindFromProto(sourceapiv1.ObjectKind_OBJECT_KIND_UNSPECIFIED); err == nil {
		t.Error("KindFromProto(UNSPECIFIED) succeeded")
	}
	if _, err := KindFromProto(99); err == nil {
		t.Error("KindFromProto(99) succeeded")
	}
	if got := Kind(0).Proto(); got != sourceapiv1.ObjectKind_OBJECT_KIND_UNSPECIFIED {
		t.Errorf("Kind(0).Proto() = %v", got)
	}
	if got := Kind(9).String(); got != "Kind(9)" {
		t.Errorf("Kind(9).String() = %q", got)
	}
}

func TestID(t *testing.T) {
	const h = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	id, err := ParseHex(h)
	if err != nil {
		t.Fatal(err)
	}
	if id.Hex() != h || id.String() != h {
		t.Errorf("round trip gave %s", id.Hex())
	}
	if upper, err := ParseHex("4B825DC642CB6EB9A060E54BF8D69288FBEE4904"); err != nil || upper != id {
		t.Errorf("ParseHex(upper) = %v, %v", upper, err)
	}
	for _, bad := range []string{"", "4b825d", h + "00", "zz825dc642cb6eb9a060e54bf8d69288fbee4904"} {
		if _, err := ParseHex(bad); err == nil {
			t.Errorf("ParseHex(%q) succeeded", bad)
		}
	}
	if got, err := IDFromBytes(id[:]); err != nil || got != id {
		t.Errorf("IDFromBytes = %v, %v", got, err)
	}
	if _, err := IDFromBytes(id[:19]); err == nil {
		t.Error("IDFromBytes(19 bytes) succeeded")
	}
	if RootID.Hex() != "0000000000000000000000000000000000000000" {
		t.Errorf("RootID = %s", RootID.Hex())
	}
}
