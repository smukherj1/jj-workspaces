# workspace-service

Go gRPC server that stores each repo's objects, operation log and op heads. See [DESIGN.md](DESIGN.md) for the overall design.

The service reads Git commit and tree objects only to walk parents and trees, and treats operations and views as opaque objects.

## Storage

- **Objects:** commits, trees, blobs, operations and views, keyed by ID.
- **Blob metadata:** `(size, SHA-256)` for every blob, stored next to its Git ID.
- **Upload time** for every object, so garbage collection can be added later without a migration.
- **Op heads:** a mutable set of operation IDs per repo, the only mutable state.

## API

Served to the daemon over gRPC.

| RPC                                                    | Behavior                                                                                                                                                                           |
| ------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GetObjects(ids[])`                                    | Batch fetch small objects (commits, trees, small blobs, operations, views) in one response; returns a `too_large(size)` marker instead of content for blobs over the message limit |
| `DownloadObject(id) → chunks` (server-streaming)       | Download one large blob that doesn't fit in a single message                                                                                                                       |
| `GetCommitGraph(heads[], known_heads[]) → commits` (server-streaming) | Stream every commit object reachable from `heads` but not from `known_heads`, whole                                                                 |
| `GetTreeRecursive(tree_id, path, depth)`               | Fetch a subtree's tree objects without blobs, with each blob's `(size, SHA-256)`                                                                                                  |
| `HasObjects(ids[]) → missing[]`                        | Dedup before upload                                                                                                                                                                |
| `PutObjects(objects[])`                                | Upload a batch of small objects (commits, trees, small blobs, operations, views) in one request                                                                                    |
| `UploadObject(chunks)` (client-streaming)              | Upload one large blob that doesn't fit in a single message                                                                                                                         |
| `GetOpHeads(repo)`                                     | Return the current op heads                                                                                                                                                        |
| `UpdateOpHeads(repo, old[], new)`                      | Atomically remove `old` and add `new`; never fails because an `old` head is already gone                                                                                           |

- **Upload verification:** `PutObjects` and `UploadObject` re-hash every object with collision-detecting SHA-1 (sha1dc) and reject any whose ID doesn't match, so a bad client can't corrupt the shared store. Blob metadata is computed at the same time.
- **`GetCommitGraph`** sends commits whole rather than an index. Commits are a few hundred bytes each, so tens of MB at most for 100k commits, and the service stays independent of jj-lib's index format.

## Not in v1

- Authentication, authorization and replication (PRD future work).

## Future work

- **Garbage collection.** A periodic mark-and-sweep from all op heads that deletes unreachable objects older than a grace period (e.g. 7 days), so it never races with in-flight publishes. Objects left by failed publishes are small; the bigger growth comes from the op log never being pruned, so GC only matters once there is a policy for pruning old operations.
