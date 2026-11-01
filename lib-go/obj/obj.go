// Package obj defines object kinds and IDs, and hashes objects as Git does. The service and the
// daemon share it. See docs/plan/m2-implementation.md#objects-and-ids.
package obj

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strconv"

	"github.com/pjbgf/sha1cd"

	sourceapiv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/sourceapi/v1"
)

// Kind is the kind of an object. The zero value is invalid.
type Kind uint8

// The object kinds.
const (
	Commit Kind = iota + 1
	Tree
	Blob
	Operation
	View
)

// kindWords are the header words of the kinds, indexed by Kind.
var kindWords = [...]string{
	Commit:    "commit",
	Tree:      "tree",
	Blob:      "blob",
	Operation: "operation",
	View:      "view",
}

// Valid reports whether k is one of the defined kinds.
func (k Kind) Valid() bool {
	return k >= Commit && k <= View
}

// String returns the header word of k, e.g. "commit", or "Kind(<n>)" for an invalid kind.
func (k Kind) String() string {
	if !k.Valid() {
		return "Kind(" + strconv.Itoa(int(k)) + ")"
	}
	return kindWords[k]
}

// ParseKind returns the kind whose header word is word.
func ParseKind(word string) (Kind, error) {
	for k := Commit; k <= View; k++ {
		if kindWords[k] == word {
			return k, nil
		}
	}
	return 0, fmt.Errorf("unknown object kind %q", word)
}

// Proto returns k as a proto ObjectKind, OBJECT_KIND_UNSPECIFIED for an invalid kind.
func (k Kind) Proto() sourceapiv1.ObjectKind {
	switch k {
	case Commit:
		return sourceapiv1.ObjectKind_OBJECT_KIND_COMMIT
	case Tree:
		return sourceapiv1.ObjectKind_OBJECT_KIND_TREE
	case Blob:
		return sourceapiv1.ObjectKind_OBJECT_KIND_BLOB
	case Operation:
		return sourceapiv1.ObjectKind_OBJECT_KIND_OPERATION
	case View:
		return sourceapiv1.ObjectKind_OBJECT_KIND_VIEW
	}
	return sourceapiv1.ObjectKind_OBJECT_KIND_UNSPECIFIED
}

// KindFromProto returns the kind of a proto ObjectKind. It fails for OBJECT_KIND_UNSPECIFIED and
// unknown values.
func KindFromProto(p sourceapiv1.ObjectKind) (Kind, error) {
	switch p {
	case sourceapiv1.ObjectKind_OBJECT_KIND_COMMIT:
		return Commit, nil
	case sourceapiv1.ObjectKind_OBJECT_KIND_TREE:
		return Tree, nil
	case sourceapiv1.ObjectKind_OBJECT_KIND_BLOB:
		return Blob, nil
	case sourceapiv1.ObjectKind_OBJECT_KIND_OPERATION:
		return Operation, nil
	case sourceapiv1.ObjectKind_OBJECT_KIND_VIEW:
		return View, nil
	}
	return 0, fmt.Errorf("invalid object kind %v", p)
}

// IDSize is the length of an ID in bytes.
const IDSize = 20

// ID is an object ID: the SHA-1 of the object's Git header and content.
type ID [IDSize]byte

// RootID is the ID of the root commit and the root operation: all zeros. Root objects are never
// stored.
var RootID ID

// Hex returns id as 40 lowercase hex digits.
func (id ID) Hex() string {
	return hex.EncodeToString(id[:])
}

// String returns id.Hex().
func (id ID) String() string {
	return id.Hex()
}

// ParseHex parses an ID from 40 hex digits, in either case.
func ParseHex(s string) (ID, error) {
	var id ID
	if len(s) != 2*IDSize {
		return id, fmt.Errorf("object ID %q isn't %d hex digits", s, 2*IDSize)
	}
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return id, fmt.Errorf("object ID %q isn't hex: %w", s, err)
	}
	return id, nil
}

// IDFromBytes returns the ID in b, which must be IDSize bytes long, as IDs are on the wire.
func IDFromBytes(b []byte) (ID, error) {
	var id ID
	if len(b) != IDSize {
		return id, fmt.Errorf("object ID is %d bytes, not %d", len(b), IDSize)
	}
	copy(id[:], b)
	return id, nil
}

// Header returns the Git header of an object: "<kind> <len>\x00".
func Header(kind Kind, size int) []byte {
	return fmt.Appendf(nil, "%s %d\x00", kind, size)
}

// ErrCollision is returned by Hash when the content looks like part of a SHA-1 collision attack.
var ErrCollision = errors.New("SHA-1 collision attack detected")

// Hash returns the ID of an object: the SHA-1 of its Git header and content, as Git computes it.
// It uses SHA-1 with collision detection and returns ErrCollision when it detects an attack.
func Hash(kind Kind, content []byte) (ID, error) {
	if !kind.Valid() {
		return ID{}, fmt.Errorf("can't hash an object of invalid kind %v", kind)
	}
	return sum(Header(kind, len(content)), content)
}

// collisionResistantHash is the interface of sha1cd's hash.
type collisionResistantHash interface {
	hash.Hash
	CollisionResistantSum(in []byte) ([]byte, bool)
}

// sum returns the SHA-1 of the concatenated parts, or ErrCollision if sha1cd detects an attack.
func sum(parts ...[]byte) (ID, error) {
	var id ID
	h := sha1cd.New().(collisionResistantHash)
	for _, p := range parts {
		h.Write(p)
	}
	b, collision := h.CollisionResistantSum(nil)
	if collision {
		return id, ErrCollision
	}
	copy(id[:], b)
	return id, nil
}

// BlobMeta returns the size and SHA-256 of a blob's content (the file bytes, without the header).
func BlobMeta(content []byte) (size uint64, sha [sha256.Size]byte) {
	return uint64(len(content)), sha256.Sum256(content)
}
