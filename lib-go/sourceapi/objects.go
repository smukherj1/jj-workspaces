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

// errInvalidObject is returned, wrapped, for an object whose ID doesn't match its kind and content,
// or whose content looks like a SHA-1 collision attack.
var errInvalidObject = errors.New("invalid object")

// object is an object as the API carries it: its ID, kind and content, without the Git header.
type object struct {
	id      obj.ID
	kind    obj.Kind
	content []byte
}

// uploadHeader is the parsed header of an UploadObject stream.
type uploadHeader struct {
	id   obj.ID
	kind obj.Kind
	size uint64
}

// uploadServer is the server side of an UploadObject stream.
type uploadServer = grpc.ClientStreamingServer[sourceapiv1.UploadObjectRequest, sourceapiv1.UploadObjectResponse]

// objectsService serves the Objects service. Object content lives in objects and each object's row
// in meta; an object is stored once its row is.
type objectsService struct {
	sourceapiv1.UnimplementedObjectsServer
	meta    *metastore.Store
	objects *objstore.Store
}

func (s *objectsService) PutObjects(ctx context.Context, req *sourceapiv1.PutObjectsRequest) (
	*sourceapiv1.PutObjectsResponse, error,
) {
	objects := make([]object, len(req.GetObjects()))
	total := 0
	for i, o := range req.GetObjects() {
		id, err := parseID(o.GetId())
		if err != nil {
			return nil, wrapStatus(err, "parsing ID of object %d", i)
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
			return nil, wrapStatus(err, "parsing kind of object %d", i)
		}
		objects[i] = object{id: id, kind: kind, content: o.GetContent()}
	}
	if err := s.storeObjects(ctx, objects); err != nil {
		return nil, storageError(fmt.Errorf("storing object batch: %w", err))
	}
	return &sourceapiv1.PutObjectsResponse{}, nil
}

func (s *objectsService) UploadObject(stream uploadServer) error {
	header, err := recvUploadHeader(stream)
	if err != nil {
		return wrapStatus(err, "receiving upload header")
	}
	w, cleanup, err := s.objects.Create(header.kind, header.size)
	if err != nil {
		return storageError(fmt.Errorf("creating file for object %s: %w", header.id, err))
	}
	defer cleanup()
	digest, err := recvUploadContent(stream, header, w)
	if err != nil {
		return wrapStatus(err, "receiving upload content")
	}
	// As in storeObjects, the file comes first. An object already stored has its file replaced with
	// verified bytes, and InsertObjects keeps its row.
	if err := w.Commit(header.id); err != nil {
		return storageError(fmt.Errorf("committing object %s: %w", header.id, err))
	}
	err = s.meta.InsertObjects(stream.Context(), []metastore.ObjectRecord{
		{ID: header.id, Kind: header.kind, Size: header.size, SHA256: digest.SHA256},
	})
	if err != nil {
		return storageError(fmt.Errorf("inserting row for object %s: %w", header.id, err))
	}
	if err := stream.SendAndClose(&sourceapiv1.UploadObjectResponse{}); err != nil {
		return wrapStatus(err, "sending upload response for object %s", header.id)
	}
	return nil
}

func (s *objectsService) GetObjectsMeta(ctx context.Context, req *sourceapiv1.GetObjectsMetaRequest) (
	*sourceapiv1.GetObjectsMetaResponse, error,
) {
	ids, err := parseIDs(req.GetIds())
	if err != nil {
		return nil, wrapStatus(err, "parsing object IDs")
	}
	metas, err := s.meta.GetObjectsMeta(ctx, ids)
	if err != nil {
		return nil, storageError(fmt.Errorf("looking up object rows: %w", err))
	}
	resp := &sourceapiv1.GetObjectsMetaResponse{Objects: make([]*sourceapiv1.ObjectMeta, len(metas))}
	for i, m := range metas {
		resp.Objects[i] = &sourceapiv1.ObjectMeta{Id: m.ID[:], Present: m.Present, Size: m.Size}
	}
	return resp, nil
}

func (s *objectsService) GetObjects(ctx context.Context, req *sourceapiv1.GetObjectsRequest) (
	*sourceapiv1.GetObjectsResponse, error,
) {
	ids, err := parseIDs(req.GetIds())
	if err != nil {
		return nil, wrapStatus(err, "parsing object IDs")
	}
	metas, err := s.meta.GetObjectsMeta(ctx, ids)
	if err != nil {
		return nil, storageError(fmt.Errorf("looking up object rows: %w", err))
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
			return nil, storageError(fmt.Errorf("reading object %s: %w", m.ID, err))
		}
		o, err := s.readFile(m)
		if err != nil {
			return nil, storageError(fmt.Errorf("reading object batch: %w", err))
		}
		resp.Objects[i] = &sourceapiv1.Object{Id: o.id[:], Kind: o.kind.Proto(), Content: o.content}
	}
	return resp, nil
}

func (s *objectsService) DownloadObject(req *sourceapiv1.DownloadObjectRequest,
	stream grpc.ServerStreamingServer[sourceapiv1.DownloadObjectResponse],
) error {
	id, err := parseID(req.GetId())
	if err != nil {
		return wrapStatus(err, "parsing object ID")
	}
	metas, err := s.meta.GetObjectsMeta(stream.Context(), []obj.ID{id})
	if err != nil {
		return storageError(fmt.Errorf("looking up row for object %s: %w", id, err))
	}
	if len(metas) != 1 {
		return storageError(fmt.Errorf("object %s: meta.db returned %d rows, want 1", id, len(metas)))
	}
	m := metas[0]
	if !m.Present {
		return status.Errorf(codes.NotFound, "object %s: not found", id)
	}
	kind, size, r, err := s.objects.Open(id)
	if err != nil {
		return storageError(fmt.Errorf("stored object %s can't be read: %w", id, err))
	}
	defer r.Close()
	if err := checkRow(m, kind, size); err != nil {
		return storageError(fmt.Errorf("verifying metadata for object %s matches stored content: %w", id, err))
	}
	err = stream.Send(&sourceapiv1.DownloadObjectResponse{
		Msg: &sourceapiv1.DownloadObjectResponse_Header{Header: &sourceapiv1.DownloadObjectHeader{
			Kind: kind.Proto(), Size: size,
		}},
	})
	if err != nil {
		return wrapStatus(err, "sending download header for object %s", id)
	}
	// Send marshals the message before it returns, so the one buffer can be reused.
	buf := make([]byte, min(size, MaxMessageContent))
	for left := size; left > 0; {
		chunk := buf[:min(left, MaxMessageContent)]
		if _, err := io.ReadFull(r, chunk); err != nil {
			return storageError(fmt.Errorf("stored object %s can't be read: %w", id, err))
		}
		err := stream.Send(&sourceapiv1.DownloadObjectResponse{
			Msg: &sourceapiv1.DownloadObjectResponse_Chunk{Chunk: chunk},
		})
		if err != nil {
			return wrapStatus(err, "sending content chunk for object %s", id)
		}
		left -= uint64(len(chunk))
	}
	return nil
}

// recvUploadHeader receives and parses the header that must start an upload.
func recvUploadHeader(stream uploadServer) (uploadHeader, error) {
	first, err := stream.Recv()
	if err != nil {
		return uploadHeader{}, wrapStatus(err, "initiating stream")
	}
	header := first.GetHeader()
	if header == nil {
		return uploadHeader{}, status.Error(codes.InvalidArgument,
			"the first UploadObject message must be a header")
	}
	id, err := parseID(header.GetId())
	if err != nil {
		return uploadHeader{}, wrapStatus(err, "parsing object ID")
	}
	kind, err := parseKind(id, header.GetKind())
	if err != nil {
		return uploadHeader{}, wrapStatus(err, "parsing object kind")
	}
	return uploadHeader{id: id, kind: kind, size: header.GetSize()}, nil
}

// recvUploadContent writes the rest of an upload's stream to w, hashing it as it goes. It returns
// the object's digest once the content is the size the header declared and matches the header's ID.
// Errors are gRPC statuses.
func recvUploadContent(stream uploadServer, header uploadHeader, w *objstore.Writer) (obj.Digest, error) {
	id, size := header.id, header.size
	h, err := obj.NewHasher(header.kind, size)
	if err != nil {
		return obj.Digest{}, storageError(fmt.Errorf("creating hasher for object %s: %w", id, err))
	}
	var received uint64
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return obj.Digest{}, wrapStatus(err, "receiving chunk for object %s", id)
		}
		if msg.GetHeader() != nil {
			return obj.Digest{}, status.Errorf(codes.InvalidArgument,
				"object %s: a second UploadObject header", id)
		}
		chunk := msg.GetChunk()
		if len(chunk) > MaxMessageContent {
			return obj.Digest{}, status.Errorf(codes.InvalidArgument,
				"object %s: a %d-byte chunk is over the limit of %d", id, len(chunk), MaxMessageContent)
		}
		received += uint64(len(chunk))
		if received > size {
			return obj.Digest{}, status.Errorf(codes.InvalidArgument,
				"object %s: content is longer than the declared %d bytes", id, size)
		}
		if _, err := w.Write(chunk); err != nil {
			return obj.Digest{}, storageError(fmt.Errorf("writing chunk for object %s: %w", id, err))
		}
		h.Write(chunk)
	}
	if received != size {
		return obj.Digest{}, status.Errorf(codes.InvalidArgument,
			"object %s: content is %d bytes, but the header says %d", id, received, size)
	}
	d, err := checkDigest(id, h)
	if err != nil {
		return obj.Digest{}, storageError(fmt.Errorf("verifying uploaded content: %w", err))
	}
	return d, nil
}

// storeObjects verifies every object's ID and stores the batch. If any object is invalid, it stores
// none of them and returns errInvalidObject naming the first invalid ID. Objects already stored are
// left as they are.
func (s *objectsService) storeObjects(ctx context.Context, objects []object) error {
	ids := make([]obj.ID, len(objects))
	digests := make([]obj.Digest, len(objects))
	for i, o := range objects {
		h, err := obj.NewHasher(o.kind, uint64(len(o.content)))
		if err != nil {
			return fmt.Errorf("creating hasher for object %s: %w", o.id, err)
		}
		h.Write(o.content)
		if digests[i], err = checkDigest(o.id, h); err != nil {
			return fmt.Errorf("verifying object content: %w", err)
		}
		ids[i] = o.id
	}
	metas, err := s.meta.GetObjectsMeta(ctx, ids)
	if err != nil {
		return fmt.Errorf("looking up object rows: %w", err)
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
	for i, o := range objects {
		if !want[o.id] {
			continue
		}
		delete(want, o.id) // A batch may repeat an ID.
		if err := s.objects.Put(o.id, o.kind, o.content); err != nil {
			return fmt.Errorf("writing object %s: %w", o.id, err)
		}
		records = append(records, metastore.ObjectRecord{
			ID: o.id, Kind: o.kind, Size: uint64(len(o.content)), SHA256: digests[i].SHA256,
		})
	}
	if err := s.meta.InsertObjects(ctx, records); err != nil {
		return fmt.Errorf("inserting object rows: %w", err)
	}
	return nil
}

// checkDigest returns the digest of the object hashed by h, and errInvalidObject if it isn't id.
func checkDigest(id obj.ID, h *obj.Hasher) (obj.Digest, error) {
	d, err := h.Sum()
	if errors.Is(err, obj.ErrCollision) {
		return d, fmt.Errorf("%w: object %s: %v", errInvalidObject, id, err)
	}
	if err != nil {
		return d, fmt.Errorf("hashing object %s: %w", id, err)
	}
	if d.ID != id {
		return d, fmt.Errorf("%w: object %s doesn't match its content, which hashes to %s",
			errInvalidObject, id, d.ID)
	}
	return d, nil
}

// readFile reads a present object's file and checks it against its row. A row means the object is
// stored, so a missing file is corruption rather than a missing object.
func (s *objectsService) readFile(m metastore.ObjectMeta) (object, error) {
	kind, content, err := s.objects.Get(m.ID)
	if err != nil {
		return object{}, fmt.Errorf("stored object %s can't be read: %w", m.ID, err)
	}
	if err := checkRow(m, kind, uint64(len(content))); err != nil {
		return object{}, fmt.Errorf("verifying metadata for object %s matches stored content: %w", m.ID, err)
	}
	return object{id: m.ID, kind: kind, content: content}, nil
}

// checkRow checks an object's file, of the given kind and size, against its row.
func checkRow(m metastore.ObjectMeta, kind obj.Kind, size uint64) error {
	if kind != m.Kind || size != m.Size {
		return fmt.Errorf("stored object %s is a %d-byte %s, but meta.db says a %d-byte %s",
			m.ID, size, kind, m.Size, m.Kind)
	}
	return nil
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
			return nil, wrapStatus(err, "parsing object ID %d", i)
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

// wrapStatus adds context to err's message and keeps its gRPC code. Wrapping a status with
// fmt.Errorf keeps the code too, but puts the status's "rpc error: code = ..." text in the message
// clients see.
func wrapStatus(err error, format string, args ...any) error {
	s := status.Convert(err)
	return status.Errorf(s.Code(), "%s: %s", fmt.Sprintf(format, args...), s.Message())
}

// storageError turns an error from storing or reading objects into a gRPC status. Anything else,
// including objstore.ErrNotFound for a row whose file is gone, is logged and returned as Internal.
func storageError(err error) error {
	switch {
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
