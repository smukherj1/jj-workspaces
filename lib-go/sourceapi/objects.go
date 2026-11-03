package sourceapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	sourceapiv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/sourceapi/v1"
	"github.com/smukherj1/jj-workspaces/lib-go/obj"
	"github.com/smukherj1/jj-workspaces/lib-go/objstore"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi/metastore"
)

// errNotFound is returned, wrapped, for an object with no row.
var errNotFound = errors.New("not found")

// errInvalidObject is returned, wrapped, for an object whose ID doesn't match its kind and content,
// or whose content looks like a SHA-1 collision attack.
var errInvalidObject = errors.New("invalid object")

// object is an object as the API carries it: its ID, kind and content, without the Git header.
type object struct {
	id      obj.ID
	kind    obj.Kind
	content []byte
}

// objectsService serves the Objects service. Object content lives in objects and each object's row
// in meta; an object is stored once its row is.
type objectsService struct {
	sourceapiv1.UnimplementedObjectsServer
	meta    *metastore.Store
	objects *objstore.Store
}

func (s *objectsService) PutObjects(ctx context.Context, req *sourceapiv1.PutObjectsRequest) (
	*sourceapiv1.PutObjectsResponse, error) {
	objects := make([]object, len(req.GetObjects()))
	total := 0
	for i, o := range req.GetObjects() {
		id, err := parseID(o.GetId())
		if err != nil {
			return nil, err
		}
		if len(o.GetContent()) > MaxMessageContent {
			return nil, status.Errorf(codes.InvalidArgument,
				"object %s: %d bytes is over the batch limit of %d bytes: send it with UploadObject",
				id, len(o.GetContent()), MaxMessageContent)
		}
		total += len(o.GetContent())
		if total > MaxMessageContent {
			return nil, status.Errorf(codes.InvalidArgument,
				"batch content is over the limit of %d bytes", MaxMessageContent)
		}
		kind, err := parseKind(id, o.GetKind())
		if err != nil {
			return nil, err
		}
		objects[i] = object{id: id, kind: kind, content: o.GetContent()}
	}
	if err := s.storeObjects(ctx, objects); err != nil {
		return nil, storageError(err)
	}
	return &sourceapiv1.PutObjectsResponse{}, nil
}

func (s *objectsService) UploadObject(
	stream grpc.ClientStreamingServer[sourceapiv1.UploadObjectRequest, sourceapiv1.UploadObjectResponse],
) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	header := first.GetHeader()
	if header == nil {
		return status.Error(codes.InvalidArgument, "the first UploadObject message must be a header")
	}
	id, err := parseID(header.GetId())
	if err != nil {
		return err
	}
	kind, err := parseKind(id, header.GetKind())
	if err != nil {
		return err
	}
	size := header.GetSize()
	// Grows with the chunks rather than trusting the declared size up front.
	var content []byte
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if msg.GetHeader() != nil {
			return status.Errorf(codes.InvalidArgument, "object %s: a second UploadObject header", id)
		}
		if len(msg.GetChunk()) > MaxMessageContent {
			return status.Errorf(codes.InvalidArgument, "object %s: a %d-byte chunk is over the limit of %d",
				id, len(msg.GetChunk()), MaxMessageContent)
		}
		content = append(content, msg.GetChunk()...)
		if uint64(len(content)) > size {
			return status.Errorf(codes.InvalidArgument,
				"object %s: content is longer than the declared %d bytes", id, size)
		}
	}
	if uint64(len(content)) != size {
		return status.Errorf(codes.InvalidArgument,
			"object %s: content is %d bytes, but the header says %d", id, len(content), size)
	}
	err = s.storeObjects(stream.Context(), []object{{id: id, kind: kind, content: content}})
	if err != nil {
		return storageError(err)
	}
	return stream.SendAndClose(&sourceapiv1.UploadObjectResponse{})
}

func (s *objectsService) GetObjectsMeta(ctx context.Context, req *sourceapiv1.GetObjectsMetaRequest) (
	*sourceapiv1.GetObjectsMetaResponse, error) {
	ids, err := parseIDs(req.GetIds())
	if err != nil {
		return nil, err
	}
	metas, err := s.meta.GetObjectsMeta(ctx, ids)
	if err != nil {
		return nil, storageError(err)
	}
	resp := &sourceapiv1.GetObjectsMetaResponse{Objects: make([]*sourceapiv1.ObjectMeta, len(metas))}
	for i, m := range metas {
		resp.Objects[i] = &sourceapiv1.ObjectMeta{Id: m.ID[:], Present: m.Present, Size: m.Size}
	}
	return resp, nil
}

func (s *objectsService) GetObjects(ctx context.Context, req *sourceapiv1.GetObjectsRequest) (
	*sourceapiv1.GetObjectsResponse, error) {
	ids, err := parseIDs(req.GetIds())
	if err != nil {
		return nil, err
	}
	metas, err := s.meta.GetObjectsMeta(ctx, ids)
	if err != nil {
		return nil, storageError(err)
	}
	// Checked before reading any content, so a rejected request costs no object reads.
	var total uint64
	for _, m := range metas {
		if !m.Present {
			return nil, status.Errorf(codes.NotFound, "object %s: not found", m.ID)
		}
		if m.Size > MaxMessageContent {
			return nil, status.Errorf(codes.InvalidArgument,
				"object %s: %d bytes is over the batch limit of %d bytes: fetch it with DownloadObject",
				m.ID, m.Size, MaxMessageContent)
		}
		total += m.Size
		if total > MaxMessageContent {
			return nil, status.Errorf(codes.InvalidArgument,
				"batch content is over the limit of %d bytes", MaxMessageContent)
		}
	}
	resp := &sourceapiv1.GetObjectsResponse{Objects: make([]*sourceapiv1.Object, len(metas))}
	for i, m := range metas {
		if err := ctx.Err(); err != nil {
			return nil, storageError(err)
		}
		o, err := s.readFile(m)
		if err != nil {
			return nil, storageError(err)
		}
		resp.Objects[i] = &sourceapiv1.Object{Id: o.id[:], Kind: o.kind.Proto(), Content: o.content}
	}
	return resp, nil
}

func (s *objectsService) DownloadObject(req *sourceapiv1.DownloadObjectRequest,
	stream grpc.ServerStreamingServer[sourceapiv1.DownloadObjectResponse]) error {
	id, err := parseID(req.GetId())
	if err != nil {
		return err
	}
	o, err := s.readObject(stream.Context(), id)
	if err != nil {
		return storageError(err)
	}
	err = stream.Send(&sourceapiv1.DownloadObjectResponse{
		Msg: &sourceapiv1.DownloadObjectResponse_Header{Header: &sourceapiv1.DownloadObjectHeader{
			Kind: o.kind.Proto(), Size: uint64(len(o.content)),
		}},
	})
	if err != nil {
		return err
	}
	for start := 0; start < len(o.content); start += MaxMessageContent {
		chunk := o.content[start:min(start+MaxMessageContent, len(o.content))]
		err := stream.Send(&sourceapiv1.DownloadObjectResponse{
			Msg: &sourceapiv1.DownloadObjectResponse_Chunk{Chunk: chunk},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// storeObjects verifies every object's ID and stores the batch. If any object is invalid, it stores
// none of them and returns errInvalidObject naming the first invalid ID. Objects already stored are
// left as they are.
func (s *objectsService) storeObjects(ctx context.Context, objects []object) error {
	ids := make([]obj.ID, len(objects))
	for i, o := range objects {
		if err := verify(o); err != nil {
			return err
		}
		ids[i] = o.id
	}
	metas, err := s.meta.GetObjectsMeta(ctx, ids)
	if err != nil {
		return err
	}
	want := make(map[obj.ID]bool)
	for _, m := range metas {
		if !m.Present {
			want[m.ID] = true
		}
	}
	if len(want) == 0 {
		return nil
	}

	// Files first, then rows: a row is what makes an object stored, so a crash in between leaves
	// only files that count as missing and are overwritten by the next upload.
	records := make([]metastore.ObjectRecord, 0, len(want))
	for _, o := range objects {
		if !want[o.id] {
			continue
		}
		delete(want, o.id) // A batch may repeat an ID.
		if err := s.objects.Put(o.id, o.kind, o.content); err != nil {
			return err
		}
		r := metastore.ObjectRecord{ID: o.id, Kind: o.kind, Size: uint64(len(o.content))}
		if o.kind == obj.Blob {
			_, sum := obj.BlobMeta(o.content)
			r.SHA256 = sum[:]
		}
		records = append(records, r)
	}
	return s.meta.InsertObjects(ctx, records)
}

// verify checks that an object's ID is the hash of its kind and content.
func verify(o object) error {
	if !o.kind.Valid() {
		return fmt.Errorf("%w: object %s has invalid kind %v", errInvalidObject, o.id, o.kind)
	}
	got, err := obj.Hash(o.kind, o.content)
	if errors.Is(err, obj.ErrCollision) {
		return fmt.Errorf("%w: object %s: %v", errInvalidObject, o.id, err)
	}
	if err != nil {
		return err
	}
	if got != o.id {
		return fmt.Errorf("%w: object %s doesn't match its content, which hashes to %s",
			errInvalidObject, o.id, got)
	}
	return nil
}

// readObject returns a stored object, or errNotFound if it has no row.
func (s *objectsService) readObject(ctx context.Context, id obj.ID) (object, error) {
	metas, err := s.meta.GetObjectsMeta(ctx, []obj.ID{id})
	if err != nil {
		return object{}, err
	}
	if !metas[0].Present {
		return object{}, fmt.Errorf("object %s: %w", id, errNotFound)
	}
	return s.readFile(metas[0])
}

// readFile reads a present object's file and checks it against its row. A row means the object is
// stored, so a missing file is corruption rather than errNotFound.
func (s *objectsService) readFile(m metastore.ObjectMeta) (object, error) {
	kind, content, err := s.objects.Get(m.ID)
	if err != nil {
		return object{}, fmt.Errorf("stored object %s can't be read: %w", m.ID, err)
	}
	if kind != m.Kind || uint64(len(content)) != m.Size {
		return object{}, fmt.Errorf("stored object %s is a %d-byte %s, but meta.db says a %d-byte %s",
			m.ID, len(content), kind, m.Size, m.Kind)
	}
	return object{id: m.ID, kind: kind, content: content}, nil
}

// parseID parses an object ID from the wire, InvalidArgument if it isn't 20 bytes.
func parseID(b []byte) (obj.ID, error) {
	id, err := obj.IDFromBytes(b)
	if err != nil {
		return id, status.Error(codes.InvalidArgument, err.Error())
	}
	return id, nil
}

// parseIDs parses object IDs from the wire.
func parseIDs(bs [][]byte) ([]obj.ID, error) {
	ids := make([]obj.ID, len(bs))
	for i, b := range bs {
		id, err := parseID(b)
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	return ids, nil
}

// parseKind parses an object's kind from the wire, InvalidArgument if it isn't a known kind.
func parseKind(id obj.ID, p sourceapiv1.ObjectKind) (obj.Kind, error) {
	kind, err := obj.KindFromProto(p)
	if err != nil {
		return 0, status.Errorf(codes.InvalidArgument, "object %s: %v", id, err)
	}
	return kind, nil
}

// storageError turns an error from storing or reading objects into a gRPC status. Anything else,
// including objstore.ErrNotFound for a row whose file is gone, is logged and returned as Internal.
func storageError(err error) error {
	switch {
	case errors.Is(err, errNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, errInvalidObject):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	slog.Error("storage failed", "err", err)
	return status.Error(codes.Internal, fmt.Sprintf("storage: %v", err))
}
