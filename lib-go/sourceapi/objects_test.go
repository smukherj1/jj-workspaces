package sourceapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	sourceapiv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/sourceapi/v1"
	"github.com/smukherj1/jj-workspaces/lib-go/obj"
	"github.com/smukherj1/jj-workspaces/lib-go/objstore"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi/metastore"
)

// objectsServer is a running source-api on a data dir, with an Objects client for it and the
// stores it serves from.
type objectsServer struct {
	client sourceapiv1.ObjectsClient
	meta   *metastore.Store
	files  *objstore.Store
	stop   func()
}

// startObjects serves source-api on dataDir and returns an Objects client for it. stop shuts the
// server down and closes its metadata store, so another can start on the same dir; it's also run at the
// end of the test if it hasn't been.
func startObjects(t *testing.T, dataDir string) *objectsServer {
	t.Helper()
	objects, err := objstore.Open(filepath.Join(dataDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := metastore.Open(filepath.Join(dataDir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(meta, objects)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(lis) }()
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(MaxMessageSize), grpc.MaxCallSendMsgSize(MaxMessageSize)))
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		conn.Close()
		srv.Shutdown(time.Second)
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
		meta.Close()
	}
	t.Cleanup(stop)
	return &objectsServer{
		client: sourceapiv1.NewObjectsClient(conn), meta: meta, files: objects, stop: stop,
	}
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// newObject returns a wire object with the right ID for kind and content.
func newObject(t *testing.T, kind obj.Kind, content []byte) *sourceapiv1.Object {
	t.Helper()
	id, err := obj.Hash(kind, content)
	if err != nil {
		t.Fatal(err)
	}
	return &sourceapiv1.Object{Id: id[:], Kind: kind.Proto(), Content: content}
}

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

func ids(objects []*sourceapiv1.Object) [][]byte {
	out := make([][]byte, len(objects))
	for i, o := range objects {
		out[i] = o.GetId()
	}
	return out
}

func put(t *testing.T, c sourceapiv1.ObjectsClient, objects ...*sourceapiv1.Object) {
	t.Helper()
	if _, err := c.PutObjects(testCtx(t), &sourceapiv1.PutObjectsRequest{Objects: objects}); err != nil {
		t.Fatalf("PutObjects: %v", err)
	}
}

func objectsMeta(t *testing.T, c sourceapiv1.ObjectsClient, idList [][]byte) []*sourceapiv1.ObjectMeta {
	t.Helper()
	resp, err := c.GetObjectsMeta(testCtx(t), &sourceapiv1.GetObjectsMetaRequest{Ids: idList})
	if err != nil {
		t.Fatalf("GetObjectsMeta: %v", err)
	}
	if len(resp.GetObjects()) != len(idList) {
		t.Fatalf("GetObjectsMeta returned %d entries for %d IDs", len(resp.GetObjects()), len(idList))
	}
	return resp.GetObjects()
}

// checkGot checks that GetObjects returns each of objects, in order.
func checkGot(t *testing.T, c sourceapiv1.ObjectsClient, objects []*sourceapiv1.Object) {
	t.Helper()
	resp, err := c.GetObjects(testCtx(t), &sourceapiv1.GetObjectsRequest{Ids: ids(objects)})
	if err != nil {
		t.Fatalf("GetObjects: %v", err)
	}
	if len(resp.GetObjects()) != len(objects) {
		t.Fatalf("GetObjects returned %d objects for %d IDs", len(resp.GetObjects()), len(objects))
	}
	for i, got := range resp.GetObjects() {
		want := objects[i]
		if !bytes.Equal(got.GetId(), want.GetId()) || got.GetKind() != want.GetKind() ||
			!bytes.Equal(got.GetContent(), want.GetContent()) {
			t.Errorf("GetObjects object %d has ID %x and %d bytes, want ID %x and %d bytes", i,
				got.GetId(), len(got.GetContent()), want.GetId(), len(want.GetContent()))
		}
	}
}

// upload streams an object to UploadObject: a header declaring size, then content in chunks.
func upload(t *testing.T, c sourceapiv1.ObjectsClient, o *sourceapiv1.Object, size uint64,
	chunk int) error {
	t.Helper()
	stream, err := c.UploadObject(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	err = stream.Send(&sourceapiv1.UploadObjectRequest{
		Msg: &sourceapiv1.UploadObjectRequest_Header{Header: &sourceapiv1.UploadObjectHeader{
			Id: o.GetId(), Kind: o.GetKind(), Size: size,
		}},
	})
	if err != nil {
		t.Fatal(err)
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
			t.Fatal(err)
		}
	}
	_, err = stream.CloseAndRecv()
	return err
}

// download reads an object through DownloadObject and returns its header and content, checking
// that no chunk is over MaxMessageContent.
func download(t *testing.T, c sourceapiv1.ObjectsClient, id []byte) (*sourceapiv1.DownloadObjectHeader,
	[]byte, error) {
	t.Helper()
	stream, err := c.DownloadObject(testCtx(t), &sourceapiv1.DownloadObjectRequest{Id: id})
	if err != nil {
		t.Fatal(err)
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
		if len(msg.GetChunk()) > MaxMessageContent {
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

// wantStatus checks that an rpc failed with code and a message containing msgPart.
func wantStatus(t *testing.T, rpc string, err error, code codes.Code, msgPart string) {
	t.Helper()
	if status.Code(err) != code || !strings.Contains(status.Convert(err).Message(), msgPart) {
		t.Errorf("%s error = %v, want %v containing %q", rpc, err, code, msgPart)
	}
}

// wantNotStored checks that none of objects with a well-formed ID is stored. after says what
// happened that should have left them out, for the failure message.
func wantNotStored(t *testing.T, c sourceapiv1.ObjectsClient, after string,
	objects ...*sourceapiv1.Object) {
	t.Helper()
	var idList [][]byte
	for _, o := range objects {
		if len(o.GetId()) == obj.IDSize {
			idList = append(idList, o.GetId())
		}
	}
	for _, m := range objectsMeta(t, c, idList) {
		if m.GetPresent() {
			t.Errorf("after %s, object %x is stored, want none of %d objects stored", after, m.GetId(),
				len(idList))
		}
	}
}

// checkBlobSHA256 checks that meta records the SHA-256 of each blob's content, and none for other
// kinds.
func checkBlobSHA256(t *testing.T, meta *metastore.Store, objects []*sourceapiv1.Object) {
	t.Helper()
	idList := make([]obj.ID, len(objects))
	for i, o := range objects {
		idList[i] = obj.ID(o.GetId())
	}
	metas, err := meta.GetObjectsMeta(testCtx(t), idList)
	if err != nil {
		t.Fatal(err)
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

func TestPutAndGet(t *testing.T) {
	blob := newObject(t, obj.Blob, []byte("hello\n"))
	for _, tc := range []struct {
		name    string
		objects []*sourceapiv1.Object
	}{
		{"one of each kind", sampleObjects(t)},
		{"repeated ID", []*sourceapiv1.Object{blob, blob}},
		{"batch at the content limit", []*sourceapiv1.Object{blobOfSize(t, MaxMessageContent, 3)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startObjects(t, t.TempDir())
			wantNotStored(t, srv.client, "starting empty", tc.objects...)
			put(t, srv.client, tc.objects...)
			put(t, srv.client, tc.objects...) // Idempotent.
			checkGot(t, srv.client, tc.objects)
			checkBlobSHA256(t, srv.meta, tc.objects)
		})
	}
}

func TestPutObjects_errors(t *testing.T) {
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

	for _, tc := range []struct {
		name    string
		batch   []*sourceapiv1.Object
		wantMsg string
	}{
		{"content doesn't match ID", []*sourceapiv1.Object{good, tampered}, hexID(tampered)},
		{"kind doesn't match ID", []*sourceapiv1.Object{good, wrongKind}, hexID(wrongKind)},
		{"unspecified kind", []*sourceapiv1.Object{good, noKind}, "invalid object kind"},
		{"3-byte ID", []*sourceapiv1.Object{good, shortID}, "object ID is 3 bytes"},
		{"batch over the content limit", []*sourceapiv1.Object{first, second},
			"batch content is over the limit"},
		{"object over the content limit",
			[]*sourceapiv1.Object{blobOfSize(t, MaxMessageContent+1, 0)}, "UploadObject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := startObjects(t, t.TempDir()).client
			_, err := c.PutObjects(testCtx(t), &sourceapiv1.PutObjectsRequest{Objects: tc.batch})
			wantStatus(t, "PutObjects", err, codes.InvalidArgument, tc.wantMsg)
			wantNotStored(t, c, "a rejected PutObjects", tc.batch...)
		})
	}
}

func TestUploadObject_errors(t *testing.T) {
	blob := largeBlob(t)
	n := uint64(len(blob.GetContent()))
	tampered := newObject(t, obj.Blob, blob.GetContent())
	tampered.Content = append([]byte{}, blob.GetContent()...)
	tampered.Content[0]++
	justOverChunk := blobOfSize(t, MaxMessageContent+1, 0)

	for _, tc := range []struct {
		name    string
		object  *sourceapiv1.Object
		size    uint64
		chunk   int
		wantMsg string
	}{
		{"header size too large", blob, n + 1, MaxMessageContent, "header says"},
		{"header size too small", blob, n - 1, MaxMessageContent, "longer than"},
		{"content doesn't match ID", tampered, n, MaxMessageContent, "doesn't match"},
		{"chunk over the limit", justOverChunk, MaxMessageContent + 1, MaxMessageContent + 1,
			"over the limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := startObjects(t, t.TempDir()).client
			err := upload(t, c, tc.object, tc.size, tc.chunk)
			wantStatus(t, "UploadObject", err, codes.InvalidArgument, tc.wantMsg)
			wantNotStored(t, c, "a rejected UploadObject", tc.object)
		})
	}
}

func TestGetObjectsMeta(t *testing.T) {
	c := startObjects(t, t.TempDir()).client
	stored := newObject(t, obj.Blob, []byte("hello\n"))
	empty := newObject(t, obj.Tree, nil)
	unknown := newObject(t, obj.Blob, []byte("never stored"))
	large := largeBlob(t)
	put(t, c, stored, empty)
	if err := upload(t, c, large, largeBlobSize, MaxMessageContent); err != nil {
		t.Fatalf("UploadObject: %v", err)
	}

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
		{"uploaded over the message limit", []*sourceapiv1.Object{large},
			[]meta{{true, largeBlobSize}}},
		{"request order, repeats included", []*sourceapiv1.Object{unknown, stored, empty, stored},
			[]meta{absent, storedMeta, emptyMeta, storedMeta}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, m := range objectsMeta(t, c, ids(tc.request)) {
				want := tc.want[i]
				if !bytes.Equal(m.GetId(), tc.request[i].GetId()) || m.GetPresent() != want.present ||
					m.GetSize() != want.size {
					t.Errorf("entry %d = %v, want %+v for ID %x", i, m, want, tc.request[i].GetId())
				}
			}
		})
	}
}

func TestGetObjects(t *testing.T) {
	c := startObjects(t, t.TempDir()).client
	first, second := blobOfSize(t, 3<<20, 1), blobOfSize(t, 3<<20, 2)
	rest := blobOfSize(t, MaxMessageContent-3<<20, 3)
	large := largeBlob(t)
	unknown := newObject(t, obj.Blob, []byte("never stored"))
	put(t, c, first)
	put(t, c, second)
	put(t, c, rest)
	if err := upload(t, c, large, largeBlobSize, MaxMessageContent); err != nil {
		t.Fatalf("UploadObject: %v", err)
	}

	for _, tc := range []struct {
		name     string
		request  []*sourceapiv1.Object
		wantCode codes.Code
		wantMsg  string
	}{
		{name: "exactly at the content limit", request: []*sourceapiv1.Object{first, rest}},
		{"unknown ID", []*sourceapiv1.Object{first, unknown}, codes.NotFound, hexID(unknown)},
		{"batch over the content limit", []*sourceapiv1.Object{first, second},
			codes.InvalidArgument, "batch content is over the limit"},
		{"repeated ID over the content limit", []*sourceapiv1.Object{first, rest, rest},
			codes.InvalidArgument, "batch content is over the limit"},
		{"object over the content limit", []*sourceapiv1.Object{large},
			codes.InvalidArgument, "DownloadObject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantCode == codes.OK {
				checkGot(t, c, tc.request)
				return
			}
			_, err := c.GetObjects(testCtx(t), &sourceapiv1.GetObjectsRequest{Ids: ids(tc.request)})
			wantStatus(t, "GetObjects", err, tc.wantCode, tc.wantMsg)
		})
	}
}

func TestDownloadObject(t *testing.T) {
	c := startObjects(t, t.TempDir()).client
	large := largeBlob(t)
	if err := upload(t, c, large, largeBlobSize, MaxMessageContent); err != nil {
		t.Fatalf("UploadObject: %v", err)
	}
	small := newObject(t, obj.Blob, []byte("small"))
	put(t, c, small)

	for _, tc := range []struct {
		name   string
		object *sourceapiv1.Object
	}{
		{"uploaded in chunks", large},
		{"put as one message", small},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header, content, err := download(t, c, tc.object.GetId())
			if err != nil {
				t.Fatalf("DownloadObject: %v", err)
			}
			if header.GetKind() != tc.object.GetKind() ||
				header.GetSize() != uint64(len(tc.object.GetContent())) {
				t.Errorf("header = %v, want kind %v and size %d", header, tc.object.GetKind(),
					len(tc.object.GetContent()))
			}
			if !bytes.Equal(content, tc.object.GetContent()) {
				t.Errorf("content is %d bytes that differ from the %d stored", len(content),
					len(tc.object.GetContent()))
			}
		})
	}

	t.Run("unknown ID", func(t *testing.T) {
		_, _, err := download(t, c, newObject(t, obj.Blob, []byte("nope")).GetId())
		wantStatus(t, "DownloadObject", err, codes.NotFound, "not found")
	})
}

func TestObjectsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	srv := startObjects(t, dir)
	objects := sampleObjects(t)
	put(t, srv.client, objects...)
	blob := largeBlob(t)
	if err := upload(t, srv.client, blob, uint64(len(blob.GetContent())), MaxMessageContent); err != nil {
		t.Fatal(err)
	}
	srv.stop()

	c := startObjects(t, dir).client
	checkGot(t, c, objects)
	if _, content, err := download(t, c, blob.GetId()); err != nil ||
		!bytes.Equal(content, blob.GetContent()) {
		t.Errorf("after restart, DownloadObject = %d bytes, %v", len(content), err)
	}
}

func TestFileWithoutRowIsMissing(t *testing.T) {
	srv := startObjects(t, t.TempDir())
	c := srv.client
	// A crash between writing an object's file and its row leaves the file alone.
	o := newObject(t, obj.Blob, []byte("file only"))
	if err := srv.files.Put(obj.ID(o.GetId()), obj.Blob, o.GetContent()); err != nil {
		t.Fatal(err)
	}

	wantNotStored(t, c, "writing only an object's file", o)
	_, err := c.GetObjects(testCtx(t), &sourceapiv1.GetObjectsRequest{Ids: [][]byte{o.GetId()}})
	wantStatus(t, "GetObjects", err, codes.NotFound, "not found")
	put(t, c, o)
	checkGot(t, c, []*sourceapiv1.Object{o})
}

func TestGetAndDownload_corruptStorage(t *testing.T) {
	content := []byte("in the row")
	for _, tc := range []struct {
		name    string
		corrupt func(files *objstore.Store, id obj.ID) error
		wantMsg string
	}{
		{"file has another kind", func(files *objstore.Store, id obj.ID) error {
			return files.Put(id, obj.Tree, content)
		}, "meta.db says"},
		{"file has another size", func(files *objstore.Store, id obj.ID) error {
			return files.Put(id, obj.Blob, []byte("not what the row says"))
		}, "meta.db says"},
		{"file is gone", func(files *objstore.Store, id obj.ID) error {
			return files.Remove(id)
		}, "can't be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startObjects(t, t.TempDir())
			o := newObject(t, obj.Blob, content)
			put(t, srv.client, o)
			if err := tc.corrupt(srv.files, obj.ID(o.GetId())); err != nil {
				t.Fatal(err)
			}

			_, err := srv.client.GetObjects(testCtx(t),
				&sourceapiv1.GetObjectsRequest{Ids: [][]byte{o.GetId()}})
			wantStatus(t, "GetObjects", err, codes.Internal, tc.wantMsg)
			_, _, err = download(t, srv.client, o.GetId())
			wantStatus(t, "DownloadObject", err, codes.Internal, tc.wantMsg)
		})
	}
}
