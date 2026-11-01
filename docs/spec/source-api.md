# source-api

Go gRPC server implementing the source API: it stores each cloud repo's objects, its workspaces with
each one's operation log and op heads, and the repo's trunk. See [DESIGN.md](DESIGN.md) for the
overall design.

The service reads Git commit and tree objects only to walk parents and trees, and treats operations
and views as opaque objects.

## Running

- **Flags:** `--listen <host:port>` (default `localhost:7470`; port `0` picks a free port) and
  `--version`, which prints `source-api <version>`.
- **Startup:** once the listener binds, the service prints `listening on <host:port>` to stdout with
  the resolved port. Test harnesses rely on this line. Logs go to stderr.
- **Health:** `grpc.health.v1` reports `SERVING` for `""` and for each service:
  `jjc.sourceapi.v1.Repo`, `jjc.sourceapi.v1.Workspace`, `jjc.sourceapi.v1.Objects`,
  `jjc.sourceapi.v1.OpHeads` and `jjc.sourceapi.v1.Trunk`. Server reflection is enabled.
- **Shutdown:** on SIGINT or SIGTERM, health switches to `NOT_SERVING`, then the server stops
  gracefully, waiting at most 5 s for in-flight RPCs before closing the remaining connections.
- **Transport:** plaintext gRPC over HTTP/2, with no TLS or auth in v1.

## Storage

- **Objects:** commits, trees, blobs, operations and views, keyed by ID.
- **Blob metadata:** `(size, SHA-256)` for every blob, stored next to its Git ID.
- **Upload time** for every object, so garbage collection can be added later without a migration.
- **Workspaces:** a registry entry per `(repo, user, name)`, with its creation time and, from M4,
  its holder: the machine it's open on and a holder token. The user is their email, used as an
  opaque key.
- **Op heads:** a mutable set of operation IDs per workspace. A new workspace has op heads
  `{root op ID}` until its first publish.
- **Trunk:** one commit ID per repo, starting at the root commit, that only moves forward.
- **Push log:** an append-only `(commit, user, workspace, time)` record per repo, one entry per
  push. Nothing reads it in v1.

## API

Served to the daemon over gRPC as five services in `jjc.sourceapi.v1`. The services are declared up
front; each RPC is added in the milestone that first needs it.

### Repo

| RPC                | Behavior                                                            |
| ------------------ | ------------------------------------------------------------------- |
| `CreateRepo(repo)` | Record the repo; `AlreadyExists` if it exists. Creates no workspace |

### Workspace

| RPC                                                       | Behavior                                                                                                                          |
| --------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `CreateWorkspace(repo, user, name)`                       | Register a workspace with op heads `{root op ID}`; `AlreadyExists` if the user already has a workspace with that name in the repo |
| `ListWorkspaces(repo, user) → workspaces[]`               | Return the user's workspaces in the repo, with each one's holder                                                                  |
| `DeleteWorkspace(repo, user, name)`                       | Drop the workspace's registry entry and op heads; its objects stay                                                                |
| `OpenWorkspace(repo, user, name, machine) → holder_token` | Make `machine` the workspace's holder, replacing any previous holder, and return a new holder token (M4)                          |

### Objects

| RPC                                              | Behavior                                                                                                                                                                           |
| ------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GetObjects(ids[])`                              | Batch fetch small objects (commits, trees, small blobs, operations, views) in one response; returns a `too_large(size)` marker instead of content for blobs over the message limit |
| `DownloadObject(id) → chunks` (server-streaming) | Download one large blob that doesn't fit in a single message                                                                                                                       |
| `GetTreeRecursive(tree_id, path, depth)`         | Fetch a subtree's tree objects without blobs, with each blob's `(size, SHA-256)`                                                                                                   |
| `HasObjects(ids[]) → missing[]`                  | Dedup before upload                                                                                                                                                                |
| `PutObjects(objects[])`                          | Upload a batch of small objects (commits, trees, small blobs, operations, views) in one request                                                                                    |
| `UploadObject(chunks)` (client-streaming)        | Upload one large blob that doesn't fit in a single message                                                                                                                         |

### OpHeads

| RPC                                                              | Behavior                                                                                                                                                                             |
| ---------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `GetOpHeads(repo, user, workspace)`                              | Return the workspace's current op heads                                                                                                                                              |
| `UpdateOpHeads(repo, user, workspace, holder_token, old[], new)` | Atomically remove `old` from the workspace's op heads and add `new`; never fails because an `old` head is already gone; `FailedPrecondition` naming the holder if the token is stale |

### Trunk

| RPC                                        | Behavior                                                                                                                                                                                            |
| ------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GetTrunk(repo) → commit`                  | Return the current trunk commit                                                                                                                                                                     |
| `PushTrunk(repo, user, workspace, commit)` | Atomically move trunk to `commit` if it descends from the current trunk and no newly pushed commit reuses a trunk change ID, and append to the push log; otherwise reject with `FailedPrecondition` |

### Behavior

- **Upload verification:** `PutObjects` and `UploadObject` re-hash every object with
  collision-detecting SHA-1 (sha1dc) and reject any whose ID doesn't match, so a bad client can't
  corrupt the shared store. Blob metadata is computed at the same time.
- **Users** aren't registered. The service rejects an empty user.
- **Workspaces** are registered. Every op-heads call names the user and the workspace, returns
  `NotFound` for a workspace that doesn't exist, and never sees or changes another workspace's op
  heads. Workspace names follow the repo name rule.
- **Holder:** a workspace is open on one machine at a time (DESIGN). `OpenWorkspace` takes the hold
  without contacting the previous holder, and the previous holder's next `UpdateOpHeads` fails. A
  workspace with no holder, as before M4, accepts any token.
- **`PushTrunk`** is the only way trunk moves, and the fast-forward check is the whole enforcement
  of trunk immutability (R12). Pushing the current trunk commit succeeds without changing anything
  or logging, so a retry after a lost reply is safe. It needs `commit` and its ancestry stored, and
  walks only the newly pushed commits, from `commit` back to the current trunk. Pushes to one repo
  are serialized, so of two concurrent pushes from the same trunk, the second is rejected unless it
  descends from the first.
- **Trunk change IDs:** the service reads the `change-id` header of each newly pushed commit and
  rejects the push if it matches another commit on trunk or another commit in the same push, naming
  the change and the commit holding it. A per-repo table from change ID to trunk commit, updated
  atomically with trunk, keeps this O(pushed commits). Only trunk commits claim change IDs, and the
  root commit is excluded. This is the only jj data the service reads; it checks nothing else about
  jj semantics (conflicts, descriptions), which the CLI does. The table is the seed of the future
  [custom commit index](commit-index.md).

## Not in v1

- Authentication, authorization and replication (PRD future work). The user on each call is whatever
  email the client sends, and every object is readable by anyone who knows its ID.

## Future work

- **Garbage collection.** A periodic mark-and-sweep from every workspace's op heads and trunk that
  deletes unreachable objects older than a grace period (e.g. 7 days), so it never races with
  in-flight publishes. Objects left by failed publishes are small; the bigger growth comes from the
  op log never being pruned, so GC only matters once there is a policy for pruning old operations.
