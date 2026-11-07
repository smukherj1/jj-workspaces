package sourceapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	sourceapiv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/sourceapi/v1"
	"github.com/smukherj1/jj-workspaces/lib-go/obj"
	"github.com/smukherj1/jj-workspaces/lib-go/objstore"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi/sourceapitest"
)

// sampleObjects returns one object of each kind.
func sampleObjects(t *testing.T) []*sourceapiv1.Object {
	return []*sourceapiv1.Object{
		newObject(t, obj.Commit, []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n\nmsg\n")),
		newObject(t, obj.Tree, nil),
		newObject(t, obj.Blob, []byte("hello\n")),
		newObject(t, obj.Operation, []byte("operation bytes")),
		newObject(t, obj.View, []byte("view bytes")),
	}
}

// objectsMeta looks up idList with GetObjectsMeta and checks there's one entry for each.
func objectsMeta(t *testing.T, h *sourceapitest.Harness, idList [][]byte) []*sourceapiv1.ObjectMeta {
	t.Helper()
	resp, err := h.Objects().GetObjectsMeta(testCtx(t), &sourceapiv1.GetObjectsMetaRequest{Ids: idList})
	if err != nil {
		t.Fatalf("GetObjectsMeta: %v", err)
	}
	if len(resp.GetObjects()) != len(idList) {
		t.Fatalf("GetObjectsMeta returned %d entries for %d IDs", len(resp.GetObjects()), len(idList))
	}
	return resp.GetObjects()
}

// upload streams an object to UploadObject: a header declaring size, then content in chunks.
func upload(t *testing.T, h *sourceapitest.Harness, o *sourceapiv1.Object, size uint64, chunk int) error {
	t.Helper()
	stream, err := h.Objects().UploadObject(testCtx(t))
	if err != nil {
		t.Fatalf("start UploadObject: %v", err)
	}
	err = stream.Send(&sourceapiv1.UploadObjectRequest{
		Msg: &sourceapiv1.UploadObjectRequest_Header{Header: &sourceapiv1.UploadObjectHeader{
			Id: o.GetId(), Kind: o.GetKind(), Size: size,
		}},
	})
	if err != nil {
		t.Fatalf("send the UploadObject header: %v", err)
	}
	content := o.GetContent()
	for start := 0; start < len(content); start += chunk {
		err := stream.Send(&sourceapiv1.UploadObjectRequest{
			Msg: &sourceapiv1.UploadObjectRequest_Chunk{Chunk: content[start:min(start+chunk, len(content))]},
		})
		if errors.Is(err, io.EOF) {
			break // The server rejected the upload early; CloseAndRecv returns why.
		}
		if err != nil {
			t.Fatalf("send an UploadObject chunk: %v", err)
		}
	}
	_, err = stream.CloseAndRecv()
	return err
}

// download reads an object through DownloadObject and returns its header and content, checking
// that no chunk is over sourceapi.MaxMessageContent.
func download(t *testing.T, h *sourceapitest.Harness, id []byte) (*sourceapiv1.DownloadObjectHeader, []byte, error) {
	t.Helper()
	stream, err := h.Objects().DownloadObject(testCtx(t), &sourceapiv1.DownloadObjectRequest{Id: id})
	if err != nil {
		t.Fatalf("start DownloadObject: %v", err)
	}
	first, err := stream.Recv()
	if err != nil {
		return nil, nil, err
	}
	var content []byte
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return first.GetHeader(), content, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if len(msg.GetChunk()) > sourceapi.MaxMessageContent {
			t.Errorf("DownloadObject sent a %d-byte chunk", len(msg.GetChunk()))
		}
		content = append(content, msg.GetChunk()...)
	}
}

// blobOfSize returns a blob of size bytes, with different content for each seed.
func blobOfSize(t *testing.T, size int, seed byte) *sourceapiv1.Object {
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i*7) + seed
	}
	return newObject(t, obj.Blob, content)
}

// largeBlobSize takes three chunks, and is over the largest gRPC message.
const largeBlobSize = 9 << 20

func largeBlob(t *testing.T) *sourceapiv1.Object {
	return blobOfSize(t, largeBlobSize, 0)
}

// wantNotStored checks that none of objects with a well-formed ID is stored. after says what
// happened that should have left them out, for the failure message.
func wantNotStored(t *testing.T, h *sourceapitest.Harness, after string, objects ...*sourceapiv1.Object) {
	t.Helper()
	var idList [][]byte
	for _, o := range objects {
		if len(o.GetId()) == obj.IDSize {
			idList = append(idList, o.GetId())
		}
	}
	for _, m := range objectsMeta(t, h, idList) {
		if m.GetPresent() {
			t.Errorf("after %s, object %x is stored, want none of %d objects stored", after, m.GetId(),
				len(idList))
		}
	}
}

// checkBlobSHA256 checks that the metadata store records the SHA-256 of each blob's content, and
// none for other kinds.
func checkBlobSHA256(t *testing.T, h *sourceapitest.Harness, objects []*sourceapiv1.Object) {
	t.Helper()
	idList := make([]obj.ID, len(objects))
	for i, o := range objects {
		idList[i] = obj.ID(o.GetId())
	}
	metas, err := h.MetaStore().GetObjectsMeta(testCtx(t), idList)
	if err != nil {
		t.Fatalf("look up rows in the metadata store: %v", err)
	}
	for i, o := range objects {
		var want []byte
		if o.GetKind() == obj.Blob.Proto() {
			sum := sha256.Sum256(o.GetContent())
			want = sum[:]
		}
		if !bytes.Equal(metas[i].SHA256, want) {
			t.Errorf("object %x has SHA-256 %x, want %x", o.GetId(), metas[i].SHA256, want)
		}
	}
}

func hexID(o *sourceapiv1.Object) string {
	return obj.ID(o.GetId()).Hex()
}

// uploadStream starts an UploadObject stream for o and sends its header.
func uploadStream(ctx context.Context, t *testing.T, h *sourceapitest.Harness,
	o *sourceapiv1.Object) grpc.ClientStreamingClient[sourceapiv1.UploadObjectRequest, sourceapiv1.UploadObjectResponse] {
	t.Helper()
	stream, err := h.Objects().UploadObject(ctx)
	if err != nil {
		t.Fatalf("start UploadObject: %v", err)
	}
	err = stream.Send(&sourceapiv1.UploadObjectRequest{
		Msg: &sourceapiv1.UploadObjectRequest_Header{Header: &sourceapiv1.UploadObjectHeader{
			Id: o.GetId(), Kind: o.GetKind(), Size: uint64(len(o.GetContent())),
		}},
	})
	if err != nil {
		t.Fatalf("send the UploadObject header: %v", err)
	}
	return stream
}

// sendChunk sends chunk number i of content, in chunks of sourceapi.MaxMessageContent.
func sendChunk(t *testing.T,
	stream grpc.ClientStreamingClient[sourceapiv1.UploadObjectRequest, sourceapiv1.UploadObjectResponse],
	content []byte, i int) {
	t.Helper()
	start := i * sourceapi.MaxMessageContent
	err := stream.Send(&sourceapiv1.UploadObjectRequest{
		Msg: &sourceapiv1.UploadObjectRequest_Chunk{Chunk: content[start:min(start+sourceapi.MaxMessageContent, len(content))]},
	})
	if err != nil {
		t.Fatalf("send UploadObject chunk %d: %v", i, err)
	}
}

// waitFor polls cond until it's true, and fails the test if it doesn't become true in time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// tmpFiles returns the temp files in the object store.
func tmpFiles(t *testing.T, h *sourceapitest.Harness) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(h.ObjStore().TempDir())
	if err != nil {
		t.Fatalf("read the object store's temp dir: %v", err)
	}
	return entries
}

// wantNoTrace checks that a failed upload of o left no temp file, no row and no object file.
func wantNoTrace(t *testing.T, h *sourceapitest.Harness, o *sourceapiv1.Object) {
	t.Helper()
	wantNoTempFiles(t, h)
	wantNotStored(t, h, "a failed UploadObject", o)
	if has, err := h.ObjStore().Has(obj.ID(o.GetId())); err != nil || has {
		t.Errorf("object file: Has = %v, %v", has, err)
	}
}

// tamperFunc changes an object's file behind the server's back.
type tamperFunc func(objStore *objstore.Store, o *sourceapiv1.Object) error

// rewriteFile returns a tamperFunc that makes an object's file hold kind and content, leaving its
// row, if it has one, as it was.
func rewriteFile(kind obj.Kind, content []byte) tamperFunc {
	return func(objStore *objstore.Store, o *sourceapiv1.Object) error {
		return objStore.Put(obj.ID(o.GetId()), kind, content)
	}
}

// removeFile is a tamperFunc that deletes an object's file and leaves its row.
func removeFile(objStore *objstore.Store, o *sourceapiv1.Object) error {
	return objStore.Remove(obj.ID(o.GetId()))
}

// storeAll stores objects as a client would: with PutObjects if they fit in a message and
// UploadObject if they don't.
func storeAll(t *testing.T, h *sourceapitest.Harness, objects ...*sourceapiv1.Object) {
	t.Helper()
	for _, o := range objects {
		if len(o.GetContent()) <= sourceapi.MaxMessageContent {
			put(t, h, o)
			continue
		}
		if err := upload(t, h, o, uint64(len(o.GetContent())), sourceapi.MaxMessageContent); err != nil {
			t.Fatalf("UploadObject of valid object %x: %v", o.GetId(), err)
		}
	}
}

// tamper applies f, if there is one, to o in the server's object store.
func tamper(t *testing.T, h *sourceapitest.Harness, f tamperFunc, o *sourceapiv1.Object) {
	t.Helper()
	if f == nil {
		return
	}
	if err := f(h.ObjStore(), o); err != nil {
		t.Fatalf("tamper with the file of object %x: %v", o.GetId(), err)
	}
}

// wantNoTempFiles checks that nothing is left in the object store's temp dir.
func wantNoTempFiles(t *testing.T, h *sourceapitest.Harness) {
	t.Helper()
	if entries := tmpFiles(t, h); len(entries) != 0 {
		t.Errorf("%d temp files left", len(entries))
	}
}

// checkDownload checks that DownloadObject returns o's kind, size and content.
func checkDownload(t *testing.T, h *sourceapitest.Harness, o *sourceapiv1.Object) {
	t.Helper()
	header, content, err := download(t, h, o.GetId())
	if err != nil {
		t.Fatalf("DownloadObject of stored object %x: %v", o.GetId(), err)
	}
	if header.GetKind() != o.GetKind() || header.GetSize() != uint64(len(o.GetContent())) {
		t.Errorf("header = %v, want kind %v and size %d", header, o.GetKind(), len(o.GetContent()))
	}
	if !bytes.Equal(content, o.GetContent()) {
		t.Errorf("content is %d bytes that differ from the %d stored", len(content),
			len(o.GetContent()))
	}
}

// TestPutObjects sends one batch to PutObjects. A valid batch is stored whole: GetObjects returns
// every object and meta.db has each blob's SHA-256. An invalid one fails with InvalidArgument and
// stores nothing, not even its valid objects.
func TestPutObjects(t *testing.T) {
	blob := newObject(t, obj.Blob, []byte("hello\n"))
	fileOnly := newObject(t, obj.Blob, []byte("file only"))
	good := newObject(t, obj.Blob, []byte("good"))
	tampered := newObject(t, obj.Blob, []byte("original"))
	tampered.Content = []byte("tampered")
	// The right content but the wrong kind doesn't match either.
	wrongKind := newObject(t, obj.Blob, []byte("kind"))
	wrongKind.Kind = obj.Tree.Proto()
	noKind := newObject(t, obj.Blob, []byte("x"))
	noKind.Kind = sourceapiv1.ObjectKind_OBJECT_KIND_UNSPECIFIED
	shortID := &sourceapiv1.Object{Id: []byte{1, 2, 3}, Kind: obj.Blob.Proto()}
	first, second := blobOfSize(t, 3<<20, 1), blobOfSize(t, 3<<20, 2)
	samples := sampleObjects(t)

	for _, tc := range []struct {
		name   string
		stored []*sourceapiv1.Object // Stored before the batch is sent.
		tamper tamperFunc            // Applied to the first object of the batch before it's sent.
		batch  []*sourceapiv1.Object
		// wantMsg is part of the expected error, or empty if the batch should be stored.
		wantMsg string
	}{
		{name: "one of each kind", batch: samples},
		{name: "repeated ID", batch: []*sourceapiv1.Object{blob, blob}},
		{name: "batch at the content limit",
			batch: []*sourceapiv1.Object{blobOfSize(t, sourceapi.MaxMessageContent, 3)}},
		{name: "already stored", stored: samples, batch: samples},
		// A crash between writing an object's file and its row leaves a file that counts as missing.
		{name: "file without a row", tamper: rewriteFile(obj.Blob, fileOnly.GetContent()),
			batch: []*sourceapiv1.Object{fileOnly}},
		{name: "content doesn't match ID", batch: []*sourceapiv1.Object{good, tampered},
			wantMsg: hexID(tampered)},
		{name: "kind doesn't match ID", batch: []*sourceapiv1.Object{good, wrongKind},
			wantMsg: hexID(wrongKind)},
		{name: "unspecified kind", batch: []*sourceapiv1.Object{good, noKind},
			wantMsg: "invalid object kind"},
		{name: "3-byte ID", batch: []*sourceapiv1.Object{good, shortID},
			wantMsg: "object ID is 3 bytes"},
		{name: "batch over the content limit", batch: []*sourceapiv1.Object{first, second},
			wantMsg: "batch content is over the limit"},
		{name: "object over the content limit",
			batch:   []*sourceapiv1.Object{blobOfSize(t, sourceapi.MaxMessageContent+1, 0)},
			wantMsg: "UploadObject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Setup: a new server holding tc.stored, with its files tampered with if the case says so.
			h := newHarness(t)
			storeAll(t, h, tc.stored...)
			tamper(t, h, tc.tamper, tc.batch[0])

			// Test: send the batch.
			_, err := h.Objects().PutObjects(testCtx(t), &sourceapiv1.PutObjectsRequest{Objects: tc.batch})

			// Expect: a rejected batch names its problem and stores nothing.
			if tc.wantMsg != "" {
				wantStatus(t, "PutObjects", err, codes.InvalidArgument, tc.wantMsg)
				wantNotStored(t, h, "a rejected PutObjects", tc.batch...)
				return
			}

			// Expect: a valid batch can be read back, with the blobs' SHA-256 recorded.
			if err != nil {
				t.Fatalf("PutObjects of a valid batch: %v", err)
			}
			checkGot(t, h, tc.batch)
			checkBlobSHA256(t, h, tc.batch)
		})
	}
}

// TestUploadObject sends a whole upload to UploadObject: a header declaring a size, then content
// in chunks. A valid upload can be downloaded and has its SHA-256 in meta.db. An invalid one fails
// with InvalidArgument and leaves no row, no object file and no temp file.
func TestUploadObject(t *testing.T) {
	blob := largeBlob(t)
	n := uint64(len(blob.GetContent()))
	tampered := newObject(t, obj.Blob, blob.GetContent())
	tampered.Content = append([]byte{}, blob.GetContent()...)
	tampered.Content[0]++
	justOverChunk := blobOfSize(t, sourceapi.MaxMessageContent+1, 0)

	for _, tc := range []struct {
		name   string
		stored []*sourceapiv1.Object // Stored before the upload.
		object *sourceapiv1.Object   // The object to upload.
		size   uint64                // The size the header declares.
		chunk  int                   // The size of each chunk sent.
		// wantMsg is part of the expected error, or empty if the upload should succeed.
		wantMsg string
	}{
		{name: "new object", object: blob, size: n, chunk: sourceapi.MaxMessageContent},
		// An object already stored goes through the same path as a new one, and keeps its row.
		{name: "already stored", stored: []*sourceapiv1.Object{blob}, object: blob, size: n,
			chunk: sourceapi.MaxMessageContent},
		{name: "header size too large", object: blob, size: n + 1, chunk: sourceapi.MaxMessageContent,
			wantMsg: "header says"},
		{name: "header size too small", object: blob, size: n - 1, chunk: sourceapi.MaxMessageContent,
			wantMsg: "longer than"},
		{name: "content doesn't match ID", object: tampered, size: n, chunk: sourceapi.MaxMessageContent,
			wantMsg: "doesn't match"},
		{name: "chunk over the limit", object: justOverChunk, size: sourceapi.MaxMessageContent + 1,
			chunk: sourceapi.MaxMessageContent + 1, wantMsg: "over the limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Setup: a new server holding tc.stored.
			h := newHarness(t)
			storeAll(t, h, tc.stored...)

			// Test: send the header and every chunk.
			err := upload(t, h, tc.object, tc.size, tc.chunk)

			// Expect: a rejected upload names its problem and leaves nothing behind.
			if tc.wantMsg != "" {
				wantStatus(t, "UploadObject", err, codes.InvalidArgument, tc.wantMsg)
				wantNoTrace(t, h, tc.object)
				return
			}

			// Expect: a valid upload can be read back, and its temp file has become the object.
			if err != nil {
				t.Fatalf("UploadObject of a valid object: %v", err)
			}
			checkDownload(t, h, tc.object)
			checkBlobSHA256(t, h, []*sourceapiv1.Object{tc.object})
			wantNoTempFiles(t, h)
		})
	}
}

// TestUploadObject_midStream sends an upload one chunk at a time and watches the server's temp
// file. Each chunk must reach the file before the next is sent, so the server isn't holding the
// object in memory. If the client then finishes, the object is stored; if it cancels, the handler
// must remove the temp file and store nothing.
func TestUploadObject_midStream(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int // The size of the blob to upload.
		// cancelAfter is the number of chunks to send before canceling, or 0 to finish the upload.
		cancelAfter int
	}{
		{name: "client finishes", size: 64 << 20},
		{name: "client cancels after the first chunk", size: largeBlobSize, cancelAfter: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Setup: an empty server and a stream that has sent its header.
			h := newHarness(t)
			blob := blobOfSize(t, tc.size, 0)
			header := len(obj.Header(obj.Blob, uint64(tc.size)))
			ctx, cancel := context.WithCancel(testCtx(t))
			defer cancel()
			stream := uploadStream(ctx, t, h, blob)

			// Test: send chunks one at a time.
			chunks := (tc.size + sourceapi.MaxMessageContent - 1) / sourceapi.MaxMessageContent
			if tc.cancelAfter > 0 {
				chunks = tc.cancelAfter
			}
			for i := range chunks {
				sendChunk(t, stream, blob.GetContent(), i)

				// Expect: the server's one temp file grows by that chunk before the next is sent.
				want := int64(header + min((i+1)*sourceapi.MaxMessageContent, tc.size))
				waitFor(t, fmt.Sprintf("a %d-byte temp file", want), func() bool {
					entries := tmpFiles(t, h)
					if len(entries) > 1 {
						t.Fatalf("%d temp files during the upload, want 1", len(entries))
					}
					if len(entries) == 0 {
						return false // The server hasn't processed the header yet.
					}
					info, err := entries[0].Info()
					return err == nil && info.Size() == want
				})
			}

			// Expect: after a cancel, the handler, which runs on after the client has gone, removes the
			// temp file and stores nothing.
			if tc.cancelAfter > 0 {
				cancel()
				waitFor(t, "the temp file to go", func() bool { return len(tmpFiles(t, h)) == 0 })
				wantNoTrace(t, h, blob)
				return
			}

			// Expect: a finished upload can be read back, and its temp file has become the object.
			if _, err := stream.CloseAndRecv(); err != nil {
				t.Fatalf("UploadObject of a valid object: %v", err)
			}
			checkDownload(t, h, blob)
			checkBlobSHA256(t, h, []*sourceapiv1.Object{blob})
			wantNoTempFiles(t, h)
		})
	}
}

// TestGetObjectsMeta looks up objects in several states on one server and expects one entry per
// requested ID, in order, with the size of those stored and present unset for the rest.
func TestGetObjectsMeta(t *testing.T) {
	// Setup: one server holding small, empty and large objects, and a file that has no row.
	stored := newObject(t, obj.Blob, []byte("hello\n"))
	empty := newObject(t, obj.Tree, nil)
	unknown := newObject(t, obj.Blob, []byte("never stored"))
	fileOnly := newObject(t, obj.Blob, []byte("file only"))
	large := largeBlob(t)
	h := newHarness(t)
	storeAll(t, h, stored, empty, large)
	tamper(t, h, rewriteFile(obj.Blob, fileOnly.GetContent()), fileOnly)

	type meta struct {
		present bool
		size    uint64
	}
	absent, storedMeta, emptyMeta := meta{}, meta{true, 6}, meta{true, 0}
	for _, tc := range []struct {
		name    string
		request []*sourceapiv1.Object
		want    []meta
	}{
		{"stored", []*sourceapiv1.Object{stored}, []meta{storedMeta}},
		{"empty", []*sourceapiv1.Object{empty}, []meta{emptyMeta}},
		{"unknown", []*sourceapiv1.Object{unknown}, []meta{absent}},
		// A row is what makes an object stored, so a file alone doesn't count.
		{"file without a row", []*sourceapiv1.Object{fileOnly}, []meta{absent}},
		{"uploaded over the message limit", []*sourceapiv1.Object{large},
			[]meta{{true, largeBlobSize}}},
		{"request order, repeats included", []*sourceapiv1.Object{unknown, stored, empty, stored},
			[]meta{absent, storedMeta, emptyMeta, storedMeta}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Test: look up the requested IDs. objectsMeta checks that there's one entry for each.
			got := objectsMeta(t, h, ids(tc.request))

			// Expect: each entry is for the ID requested at its position, with the state expected.
			for i, m := range got {
				want := tc.want[i]
				if !bytes.Equal(m.GetId(), tc.request[i].GetId()) || m.GetPresent() != want.present ||
					m.GetSize() != want.size {
					t.Errorf("entry %d = %v, want %+v for ID %x", i, m, want, tc.request[i].GetId())
				}
			}
		})
	}
}

// TestGetObjects requests objects from GetObjects. It returns them in order, or fails the whole
// request: NotFound for an object with no row, InvalidArgument for a batch over the content limit
// and Internal for a row and file that disagree.
func TestGetObjects(t *testing.T) {
	samples := sampleObjects(t)
	first, second := blobOfSize(t, 3<<20, 1), blobOfSize(t, 3<<20, 2)
	rest := blobOfSize(t, sourceapi.MaxMessageContent-3<<20, 3)
	large := largeBlob(t)
	unknown := newObject(t, obj.Blob, []byte("never stored"))
	fileOnly := newObject(t, obj.Blob, []byte("file only"))
	rowContent := []byte("in the row")
	withRow := newObject(t, obj.Blob, rowContent)

	for _, tc := range []struct {
		name    string
		stored  []*sourceapiv1.Object // Stored before the request.
		restart bool                  // Restart the server before the request.
		tamper  tamperFunc            // Applied to the first object requested.
		request []*sourceapiv1.Object
		// wantCode and wantMsg describe the expected error. codes.OK means the request should succeed.
		wantCode codes.Code
		wantMsg  string
	}{
		{name: "one of each kind", stored: samples, request: samples},
		{name: "exactly at the content limit", stored: []*sourceapiv1.Object{first, rest},
			request: []*sourceapiv1.Object{first, rest}},
		{name: "after a restart", stored: samples, restart: true, request: samples},
		{name: "unknown ID", stored: []*sourceapiv1.Object{first},
			request:  []*sourceapiv1.Object{first, unknown},
			wantCode: codes.NotFound, wantMsg: hexID(unknown)},
		// A crash between writing an object's file and its row leaves a file that counts as missing.
		{name: "file without a row", tamper: rewriteFile(obj.Blob, fileOnly.GetContent()),
			request:  []*sourceapiv1.Object{fileOnly},
			wantCode: codes.NotFound, wantMsg: "not found"},
		{name: "batch over the content limit", stored: []*sourceapiv1.Object{first, second},
			request:  []*sourceapiv1.Object{first, second},
			wantCode: codes.InvalidArgument, wantMsg: "batch content is over the limit"},
		{name: "repeated ID over the content limit", stored: []*sourceapiv1.Object{first, rest},
			request:  []*sourceapiv1.Object{first, rest, rest},
			wantCode: codes.InvalidArgument, wantMsg: "batch content is over the limit"},
		{name: "object over the content limit", stored: []*sourceapiv1.Object{large},
			request:  []*sourceapiv1.Object{large},
			wantCode: codes.InvalidArgument, wantMsg: "DownloadObject"},
		{name: "file has another kind", stored: []*sourceapiv1.Object{withRow},
			tamper: rewriteFile(obj.Tree, rowContent), request: []*sourceapiv1.Object{withRow},
			wantCode: codes.Internal, wantMsg: "meta.db says"},
		{name: "file has another size", stored: []*sourceapiv1.Object{withRow},
			tamper:   rewriteFile(obj.Blob, []byte("not what the row says")),
			request:  []*sourceapiv1.Object{withRow},
			wantCode: codes.Internal, wantMsg: "meta.db says"},
		{name: "file is gone", stored: []*sourceapiv1.Object{withRow}, tamper: removeFile,
			request:  []*sourceapiv1.Object{withRow},
			wantCode: codes.Internal, wantMsg: "can't be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Setup: a new server holding tc.stored, restarted and tampered with if the case says so.
			h := newHarness(t)
			storeAll(t, h, tc.stored...)
			if tc.restart {
				restart(t, h)
			}
			tamper(t, h, tc.tamper, tc.request[0])

			// Test and expect: a valid request returns each object in order.
			if tc.wantCode == codes.OK {
				checkGot(t, h, tc.request)
				return
			}

			// Test and expect: an invalid request fails with the expected status.
			_, err := h.Objects().GetObjects(testCtx(t),
				&sourceapiv1.GetObjectsRequest{Ids: ids(tc.request)})
			wantStatus(t, "GetObjects", err, tc.wantCode, tc.wantMsg)
		})
	}
}

// TestDownloadObject reads one object through DownloadObject. It streams the object's header and
// content however the object was stored, or fails: NotFound for an object with no row and Internal
// for a row and file that disagree.
func TestDownloadObject(t *testing.T) {
	large := largeBlob(t)
	small := newObject(t, obj.Blob, []byte("small"))
	empty := newObject(t, obj.Tree, nil)
	unknown := newObject(t, obj.Blob, []byte("nope"))
	fileOnly := newObject(t, obj.Blob, []byte("file only"))
	rowContent := []byte("in the row")
	withRow := newObject(t, obj.Blob, rowContent)

	for _, tc := range []struct {
		name    string
		stored  []*sourceapiv1.Object // Stored before the request.
		restart bool                  // Restart the server before the request.
		tamper  tamperFunc            // Applied to object.
		object  *sourceapiv1.Object   // The object to download.
		// wantCode and wantMsg describe the expected error. codes.OK means the download should succeed.
		wantCode codes.Code
		wantMsg  string
	}{
		{name: "uploaded in chunks", stored: []*sourceapiv1.Object{large}, object: large},
		{name: "put as one message", stored: []*sourceapiv1.Object{small}, object: small},
		{name: "empty", stored: []*sourceapiv1.Object{empty}, object: empty},
		{name: "after a restart", stored: []*sourceapiv1.Object{large}, restart: true, object: large},
		{name: "unknown ID", object: unknown, wantCode: codes.NotFound, wantMsg: "not found"},
		// A crash between writing an object's file and its row leaves a file that counts as missing.
		{name: "file without a row", tamper: rewriteFile(obj.Blob, fileOnly.GetContent()),
			object: fileOnly, wantCode: codes.NotFound, wantMsg: "not found"},
		{name: "file has another kind", stored: []*sourceapiv1.Object{withRow},
			tamper: rewriteFile(obj.Tree, rowContent), object: withRow,
			wantCode: codes.Internal, wantMsg: "meta.db says"},
		{name: "file has another size", stored: []*sourceapiv1.Object{withRow},
			tamper: rewriteFile(obj.Blob, []byte("not what the row says")), object: withRow,
			wantCode: codes.Internal, wantMsg: "meta.db says"},
		{name: "file is gone", stored: []*sourceapiv1.Object{withRow}, tamper: removeFile,
			object: withRow, wantCode: codes.Internal, wantMsg: "can't be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Setup: a new server holding tc.stored, restarted and tampered with if the case says so.
			h := newHarness(t)
			storeAll(t, h, tc.stored...)
			if tc.restart {
				restart(t, h)
			}
			tamper(t, h, tc.tamper, tc.object)

			// Test and expect: a valid download returns the object's header and content.
			if tc.wantCode == codes.OK {
				checkDownload(t, h, tc.object)
				return
			}

			// Test and expect: an invalid download fails with the expected status.
			_, _, err := download(t, h, tc.object.GetId())
			wantStatus(t, "DownloadObject", err, tc.wantCode, tc.wantMsg)
		})
	}
}
