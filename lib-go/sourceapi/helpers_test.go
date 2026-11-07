package sourceapi_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	sourceapiv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/sourceapi/v1"
	"github.com/smukherj1/jj-workspaces/lib-go/obj"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi/sourceapitest"
)

// newHarness starts a source-api server on a new temp data dir, failing the test if it can't, and
// closes it at the end of the test.
func newHarness(t *testing.T) *sourceapitest.Harness {
	t.Helper()
	h, err := sourceapitest.Start(t.TempDir())
	if err != nil {
		t.Fatalf("start source-api: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("close source-api: %v", err)
		}
	})
	return h
}

// restart restarts h's server on the same data dir, failing the test if it can't.
func restart(t *testing.T, h *sourceapitest.Harness) {
	t.Helper()
	if err := h.Restart(); err != nil {
		t.Fatalf("restart source-api: %v", err)
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
		t.Fatalf("hash a %s of %d bytes: %v", kind, len(content), err)
	}
	return &sourceapiv1.Object{Id: id[:], Kind: kind.Proto(), Content: content}
}

func ids(objects []*sourceapiv1.Object) [][]byte {
	out := make([][]byte, len(objects))
	for i, o := range objects {
		out[i] = o.GetId()
	}
	return out
}

// wantStatus checks that an rpc failed with code and a message containing msgPart, without another
// status's text nested in the message.
func wantStatus(t *testing.T, rpc string, err error, code codes.Code, msgPart string) {
	t.Helper()
	msg := status.Convert(err).Message()
	if status.Code(err) != code || !strings.Contains(msg, msgPart) || strings.Contains(msg, "rpc error:") {
		t.Errorf("%s error = %v, want %v containing %q", rpc, err, code, msgPart)
	}
}

// put stores objects in one PutObjects batch.
func put(t *testing.T, h *sourceapitest.Harness, objects ...*sourceapiv1.Object) {
	t.Helper()
	_, err := h.Objects().PutObjects(testCtx(t), &sourceapiv1.PutObjectsRequest{Objects: objects})
	if err != nil {
		t.Fatalf("PutObjects: %v", err)
	}
}

// checkGot checks that GetObjects returns each of objects, in order.
func checkGot(t *testing.T, h *sourceapitest.Harness, objects []*sourceapiv1.Object) {
	t.Helper()
	resp, err := h.Objects().GetObjects(testCtx(t), &sourceapiv1.GetObjectsRequest{Ids: ids(objects)})
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
