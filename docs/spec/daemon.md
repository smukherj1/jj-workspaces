# daemon

Go binary running on each developer and build machine. It serves the jj CLI over gRPC on a Unix socket, owns one FUSE mount per cloud workspace, and talks to the workspace service. See [DESIGN.md](DESIGN.md) for the overall design and [workspace-service.md](workspace-service.md#api) for the API it calls.

## Running

- **Flags:**
  - `--socket <path>` (default `$XDG_RUNTIME_DIR/jjw/daemon.sock`). If `XDG_RUNTIME_DIR` is unset
    and the flag isn't given, the daemon exits with an error that names `--socket`.
  - `--service-addr <host:port>` (required): the workspace service.
  - `--service-timeout <duration>` (default `10s`): bounds every call to the service.
  - `--version` prints `jjw-daemon <version>`.
- **Socket:** the daemon creates the socket's parent directory with mode `0700` if it doesn't exist.
  If the socket file already exists, the daemon dials it: an answer means another daemon is running,
  and the new one exits with `jjw-daemon is already running on <socket>`; connection refused means
  the file is stale, and it is removed. A path that exists but isn't a socket is an error. The
  socket gets mode `0600`. Once it listens, the daemon prints `listening on <socket>` to stdout.
  Test harnesses rely on this line. Logs go to stderr.
- **Startup without the service:** the daemon starts even if the service is down, because cache hits
  must work offline. It connects to the service lazily, on the first call.
- **Health:** `grpc.health.v1` reports `SERVING` for `""`. Server reflection is enabled.
- **Shutdown:** on SIGINT or SIGTERM, health switches to `NOT_SERVING`, then the server stops
  gracefully, waiting at most 5 s for in-flight RPCs before closing the remaining connections.
  Closing the listener unlinks the socket.

## API

Served to the CLI over gRPC on a Unix socket. It mirrors the jj-lib traits so the CLI stays a thin shim.
The services are declared up front; each RPC is added in the milestone that first needs it.

| Service       | RPCs                                                                                           | Backs jj-lib trait                  |
| ------------- | ---------------------------------------------------------------------------------------------- | ----------------------------------- |
| `Store`       | `Read/Write{Commit,Tree,File,Symlink}`, `GetCopyRecords`, `FetchCommitGraph(heads[])`          | `Backend`, index build              |
| `OpStore`     | `Read/Write{Operation,View}`, `ResolveOpIdPrefix`                                              | `OpStore`                           |
| `OpHeads`     | `GetOpHeads`, `UpdateOpHeads(old[], new)`                                                      | `OpHeadsStore`                      |
| `WorkingCopy` | `Checkout(ws, tree_id)`, `ListDirty(ws) → paths[]`, `Snapshot(ws, paths[]) → entries[]`, `Finish(ws, op_id, entries[], tree_id, state)`, `Reset(ws, tree_id, state)`, `GetState(ws)`, `Lock/Unlock(ws)` | `WorkingCopy` / `LockedWorkingCopy` |
| `Workspace`   | `Create(repo, ws, mount_path)`, `Delete(ws)`, `List`                                           | workspace lifecycle                 |
| `Prefetch`    | `Prefetch(ws, paths[] \| tree_id)`                                                             | build hint (R8)                     |
| `Daemon`      | `GetStatus`                                                                                    | diagnostics                         |

### Daemon

- **`GetStatus`** returns the daemon's version, the service address and the service's status, from a
  `grpc.health.v1` `Check` of `""` bounded by `--service-timeout`:
  - `OK` when the service reports `SERVING`.
  - `UNHEALTHY` for any other health status, with the status name (e.g. `NOT_SERVING`) as `details`.
  - `UNREACHABLE` when the call fails, with the error as `details`, e.g.
    `deadline exceeded after 10s` or `Unavailable: connection error: ...`.

### Store and OpStore

- **`Write*`** take raw serialized object bytes. The daemon computes the ID (SHA-1 with sha1dc, plus `(size, SHA-256)` for blobs), writes the object to the primary store (fsync'd), starts uploading it in the background, deduplicating with `HasObjects`, and returns the ID.
- **`Read*`** are served from local storage, fetching from the service on a miss.
- **`FetchCommitGraph(heads)`** makes sure every commit reachable from `heads` is in the cache, in bulk, so jj-lib's index builder never makes one round trip per generation. It calls `GetCommitGraph(heads, known_heads)`, passing the heads whose full ancestry is already cached, and writes the streamed commits into the cache.

### OpHeads

- **`GetOpHeads`** is forwarded to the service on every call; op heads are never cached.
- **`UpdateOpHeads(old, new)`** waits until every object reachable from the new operation is on the service, calls the service's `UpdateOpHeads`, and only then returns. If any step fails, it returns an error.

### WorkingCopy

State per workspace: the checked-out op ID, the materialized `tree_id` the mount serves, the CLI's opaque state blob, the dirty set and per-path write generations. All of it is updated atomically.

- **`Checkout(ws, tree_id)`** points the mount at a new tree without moving any file contents, so its cost is O(diff between the trees) (R1). It diffs the old and new trees, skipping subtrees with identical IDs, and swaps the mount's root to the new tree. For each changed path, it invalidates the kernel's dentry, attribute and page caches and sets the path's mtime to the checkout time. Overlay entries for tracked paths are dropped (jj has already snapshotted them); untracked and ignored files stay in the overlay.
- **`ListDirty(ws)`** returns every path modified, added or deleted since the last snapshot. There is no filtering: ignored and untracked paths are included, and the CLI applies ignore rules.
- **`Snapshot(ws, paths[])`**: for each path, reads its write generation (`gen`, bumped by every FUSE write), hashes the file, writes the blob to the primary store, and returns `(path, gen, blob_id, mode)` or a deletion. No working-copy state changes.
- **`Finish(ws, op_id, entries, tree_id, state)`** is the commit point. It atomically clears the dirty bit of each entry whose generation is unchanged, sets the materialized tree, drops overlay entries that now match it, and stores `op_id` and `state`. A file written during a snapshot keeps its dirty bit and lands in the next snapshot, so writes are never blocked. If `Finish` never arrives (failed publish, crash), nothing has changed and there is nothing to recover.
- **`Reset(ws, tree_id, state)`** sets the base tree without changing file contents, and marks the paths that differ between the old and new base as dirty.
- **`GetState(ws)`** returns the stored op ID, materialized `tree_id` and state.
- **`Lock/Unlock(ws)`** serialize snapshot and checkout within a workspace. FUSE writes are never blocked.

### Workspace

- **`Create(repo, ws, mount_path)`**: on first use of a cloud repo on the machine, sets up its repo directory (e.g. `~/.cache/jj-cloud-workspaces/<repo>/`) holding `.jj/repo`, the commit index and repo config, shared by all cloud workspaces of that repo. It then mounts the cloud workspace and synthesizes its `.jj` in the mount to point to the repo directory, the way a secondary jj workspace points to its repo. No local clone is made. Per-workspace working-copy state is held by the daemon, not in `.jj`.

## Local storage

The daemon keeps two stores, shared by all cloud workspaces on the machine:

- **Primary store** (pinned, never evicted): workspace overlays, working-copy state, file metadata records, and objects not yet confirmed on the service. Everything in it is persisted and fsync'd.
- **Cache:** an LRU content-addressed store holding only objects confirmed on the service, deduplicated across workspaces (R7).
- An object written by the CLI starts in the primary store and moves to the cache once its upload is confirmed. Objects left behind by a failed publish are uploaded or dropped after an age limit.

## FUSE mount

- **Read:** `readdir` and `stat` are served from tree objects plus per-blob `(size, SHA-256)`, fetched with `GetTreeRecursive`, with no blob downloads (R2). `read` fetches the blob on first access and then serves it from the cache (R4).
- **Write:** writes go to a copy-on-write overlay on local disk. The path is marked dirty and its write generation is bumped. The overlay is persisted, so a daemon restart keeps edits (R6).
- **File metadata:** a persisted per-workspace `path → (inode, mtime)` record. Inode numbers are assigned on first lookup and stay stable across checkouts and daemon restarts. mtime changes only when the content changes, through a checkout or a write, so make and ninja rebuild only what changed.

## Service client

- **Fetching:** batch requests with `GetObjects`; Git trees don't record blob sizes, so for any ID returned as `too_large(size)`, fetch the content with `DownloadObject`.
- **Errors:** cache hits work offline. Every service call has a bounded timeout and fails with a clear error, never hangs (R9).

## Restarts and upgrades

- When the daemon exits, its FUSE mounts go with it. Open files, shells whose current directory is inside a mount, and running builds fail, but no edits are lost (R6).
- systemd restarts the daemon, which remounts every cloud workspace from the primary store.
- Upgrades are scheduled to keep this to about one failure a day at most: build machines are drained and upgraded one at a time, and developer machines upgrade overnight.

## Future work

- **Digest xattr for build tools.** Expose the stored SHA-256 as an xattr (e.g. `user.sha256` for Bazel's `--unix_digest_hash_attribute_name`) so builds don't read every input.
- **Mount survival across restarts**, if daily failures from restarts become a problem:
  - Upgrades: the old daemon finishes in-flight requests and passes the `/dev/fuse` fd and its in-memory tables (open file handles, lookup counts) to the new daemon over a Unix socket. This works on any kernel.
  - Crashes: keep the fd in systemd's fd store, persist the file-handle tables, and replay in-flight requests with `FUSE_NOTIFY_RESEND` (Linux 6.9+).
- **Periodic auto-snapshots**, if edits should reach the service without the user running a jj command. The daemon would run the CLI's snapshot when a workspace is dirty, like jj's watchman `register-snapshot-trigger`. Cost: many more operations in the shared op log (each uploading the whole View) and more frequent op-head merges.
