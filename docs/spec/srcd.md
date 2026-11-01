# srcd

The daemon: a Go binary running on each developer and build machine. It serves the `jjc` CLI over
gRPC on a Unix socket, owns one FUSE mount per workspace, and talks to `source-api`. See
[DESIGN.md](DESIGN.md) for the overall design and [source-api.md](source-api.md#api) for the API it
calls.

## Running

- **Flags:**
  - `--socket <path>` (default `$XDG_RUNTIME_DIR/jjc/srcd.sock`). If `XDG_RUNTIME_DIR` is unset and
    the flag isn't given, the daemon exits with an error that names `--socket`.
  - `--source-api-addr <host:port>` (required): the source API.
  - `--source-api-timeout <duration>` (default `10s`): bounds every call to the source API.
  - `--version` prints `srcd <version>`.
- **Socket:** the daemon creates the socket's parent directory with mode `0700` if it doesn't exist.
  If the socket file already exists, the daemon dials it: an answer means another daemon is running,
  and the new one exits with `srcd is already running on <socket>`; connection refused means the
  file is stale, and it is removed. A path that exists but isn't a socket is an error. The socket
  gets mode `0600`. Once it listens, the daemon prints `listening on <socket>` to stdout. Test
  harnesses rely on this line. Logs go to stderr.
- **Startup without the source API:** the daemon starts even if `source-api` is down, because cache
  hits must work offline. It connects to it lazily, on the first call.
- **Health:** `grpc.health.v1` reports `SERVING` for `""`. Server reflection is enabled.
- **Shutdown:** on SIGINT or SIGTERM, health switches to `NOT_SERVING`, then the server stops
  gracefully, waiting at most 5 s for in-flight RPCs before closing the remaining connections.
  Closing the listener unlinks the socket.

## API

Served to the CLI over gRPC on a Unix socket. It mirrors the jj-lib traits so the CLI stays a thin
shim. The services are declared up front; each RPC is added in the milestone that first needs it.

| Service       | RPCs                                                                                                                                                                                                    | Backs jj-lib trait                  |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------- |
| `Store`       | `Read/Write{Commit,Tree,File,Symlink}`, `GetCopyRecords`                                                                                                                                                | `Backend`                           |
| `OpStore`     | `Read/Write{Operation,View}`, `ResolveOpIdPrefix`                                                                                                                                                       | `OpStore`                           |
| `OpHeads`     | `GetOpHeads(repo, user, ws)`, `UpdateOpHeads(repo, user, ws, old[], new)`                                                                                                                               | `OpHeadsStore`                      |
| `Trunk`       | `GetTrunk(repo)`, `PushTrunk(repo, user, ws, commit)`                                                                                                                                                   | `jjc cloud fetch` / `push`, CI      |
| `WorkingCopy` | `Checkout(ws, tree_id)`, `ListDirty(ws) → paths[]`, `Snapshot(ws, paths[]) → entries[]`, `Finish(ws, op_id, entries[], tree_id, state)`, `Reset(ws, tree_id, state)`, `GetState(ws)`, `Lock/Unlock(ws)` | `WorkingCopy` / `LockedWorkingCopy` |
| `Repo`        | `CreateRepo(repo)`                                                                                                                                                                                      | repo lifecycle                      |
| `Workspace`   | `CreateWorkspace(repo, user, ws, path)`, `OpenWorkspace(repo, user, ws, path)`, `DeleteWorkspace(repo, user, ws)`, `ListWorkspaces(repo, user)`                                                         | workspace lifecycle                 |
| `Prefetch`    | `Prefetch(ws, paths[] \| tree_id)`                                                                                                                                                                      | build hint (R8)                     |
| `Daemon`      | `GetStatus`                                                                                                                                                                                             | diagnostics                         |

### Daemon

- **`GetStatus`** returns the daemon's version, the source API address and its status
  (`source_api_address`, `source_api_status`), from a `grpc.health.v1` `Check` of `""` bounded by
  `--source-api-timeout`:
  - `OK` when `source-api` reports `SERVING`.
  - `UNHEALTHY` for any other health status, with the status name (e.g. `NOT_SERVING`) as `details`.
  - `UNREACHABLE` when the call fails, with the error as `details`, e.g.
    `deadline exceeded after 10s` or `Unavailable: connection error: ...`.

### Store and OpStore

- **`Write*`** take raw serialized object bytes. The daemon computes the ID (SHA-1 with sha1dc, plus
  `(size, SHA-256)` for blobs), writes the object to the primary store (fsync'd), starts uploading
  it in the background, deduplicating with `HasObjects`, and returns the ID.
- **`Read*`** are served from local storage, fetching from the service on a miss. There is no bulk
  commit-graph fetch: v1 histories are small (see [commit-index.md](commit-index.md)).

### OpHeads

Every call names the repo, user and workspace whose op heads it reads or changes, and the daemon
passes them through. The daemon doesn't track users. From M4, it adds the holder token it got from
`Workspace.OpenWorkspace` or `Workspace.CreateWorkspace` to `UpdateOpHeads`.

- **`GetOpHeads`** is forwarded to the service on every call; op heads are never cached.
- **`UpdateOpHeads(old, new)`** waits until every object reachable from the new operation is on the
  service, calls the service's `UpdateOpHeads`, and only then returns. If any step fails, it returns
  an error. A rejection because another machine now holds the workspace passes through to the CLI,
  which fails with `workspace <name> is open on <host>`.

### Trunk

- **`GetTrunk(repo)`** is forwarded to the service on every call; trunk is never cached.
- **`PushTrunk(repo, user, ws, commit)`** waits until every object written before the call is on the
  service, using the same barrier as `UpdateOpHeads`, then calls the service's `PushTrunk` and
  returns only once trunk has moved. A rejection because the commit doesn't descend from trunk
  passes through to the CLI.

### WorkingCopy

State per workspace: the checked-out op ID, the materialized `tree_id` the mount serves, the CLI's
opaque state blob, the dirty set and per-path write generations. All of it is updated atomically.

- **`Checkout(ws, tree_id)`** points the mount at a new tree without moving any file contents, so
  its cost is O(diff between the trees) (R1). It diffs the old and new trees, skipping subtrees with
  identical IDs, and swaps the mount's root to the new tree. For each changed path, it invalidates
  the kernel's dentry, attribute and page caches and sets the path's mtime to the checkout time.
  Overlay entries for tracked paths are dropped (jj has already snapshotted them); untracked and
  ignored files stay in the overlay.
- **`ListDirty(ws)`** returns every path modified, added or deleted since the last snapshot. There
  is no filtering: ignored and untracked paths are included, and the CLI applies ignore rules.
- **`Snapshot(ws, paths[])`**: for each path, reads its write generation (`gen`, bumped by every
  FUSE write), hashes the file, writes the blob to the primary store, and returns
  `(path, gen, blob_id, mode)` or a deletion. No working-copy state changes.
- **`Finish(ws, op_id, entries, tree_id, state)`** is the commit point. It atomically clears the
  dirty bit of each entry whose generation is unchanged, sets the materialized tree, drops overlay
  entries that now match it, and stores `op_id` and `state`. A file written during a snapshot keeps
  its dirty bit and lands in the next snapshot, so writes are never blocked. If `Finish` never
  arrives (failed publish, crash), nothing has changed and there is nothing to recover.
- **`Reset(ws, tree_id, state)`** sets the base tree without changing file contents, and marks the
  paths that differ between the old and new base as dirty.
- **`GetState(ws)`** returns the stored op ID, materialized `tree_id` and state.
- **`Lock/Unlock(ws)`** serialize snapshot and checkout within a workspace. FUSE writes are never
  blocked.

### Repo

- **`CreateRepo(repo)`** is forwarded to the service's `Repo.CreateRepo`.

### Workspace

- **`CreateWorkspace(repo, user, ws, path)`** registers the workspace on the service with
  `CreateWorkspace`. From M4 it also takes the hold, sets up the workspace's own repo directory
  (e.g. `~/.cache/jjc/<repo>/<user>/<ws>/`) holding `.jj/repo`, the commit index and repo config,
  and mounts the workspace at `path`, synthesizing its `.jj` in the mount to point to the repo
  directory, the way a secondary jj workspace points to its repo. Until then, the CLI sets up the
  repo directory at `path` itself. No local clone is made. Per-workspace working-copy state is held
  by the daemon, not in `.jj`.
- **`OpenWorkspace(repo, user, ws, path)`** (M4) does the same for an existing workspace: it takes
  the hold with `OpenWorkspace`, sets up the repo directory if this machine doesn't have it, and
  mounts the workspace. The previous holder's overlay stays in that machine's daemon.
- **`DeleteWorkspace(repo, user, ws)`** deletes the workspace on the service. From M4 it also
  unmounts it if it's mounted here, and removes its repo directory and working-copy state.
- **`ListWorkspaces(repo, user)`** returns the user's workspaces in the repo from the service, with
  each one's holder.

## Local storage

The daemon keeps two stores, shared by all workspaces on the machine:

- **Primary store** (pinned, never evicted): workspace overlays, working-copy state, file metadata
  records, and objects not yet confirmed on the service. Everything in it is persisted and fsync'd.
- **Cache:** an LRU content-addressed store holding only objects confirmed on the service,
  deduplicated across workspaces (R7).
- An object written by the CLI starts in the primary store and moves to the cache once its upload is
  confirmed. Objects left behind by a failed publish are uploaded or dropped after an age limit.

## FUSE mount

- **Read:** `readdir` and `stat` are served from tree objects plus per-blob `(size, SHA-256)`,
  fetched with `GetTreeRecursive`, with no blob downloads (R2). `read` fetches the blob on first
  access and then serves it from the cache (R4).
- **Write:** writes go to a copy-on-write overlay on local disk. The path is marked dirty and its
  write generation is bumped. The overlay is persisted, so a daemon restart keeps edits (R6).
- **File metadata:** a persisted per-workspace `path → (inode, mtime)` record. Inode numbers are
  assigned on first lookup and stay stable across checkouts and daemon restarts. mtime changes only
  when the content changes, through a checkout or a write, so make and ninja rebuild only what
  changed.

## Source API client

- **Fetching:** batch requests with `GetObjects`; Git trees don't record blob sizes, so for any ID
  returned as `too_large(size)`, fetch the content with `DownloadObject`.
- **Errors:** cache hits work offline. Every service call has a bounded timeout and fails with a
  clear error, never hangs (R9).

## Restarts and upgrades

- When the daemon exits, its FUSE mounts go with it. Open files, shells whose current directory is
  inside a mount, and running builds fail, but no edits are lost (R6).
- systemd restarts the daemon, which remounts every workspace it still holds from the primary store.
- Upgrades are scheduled to keep this to about one failure a day at most: build machines are drained
  and upgraded one at a time, and developer machines upgrade overnight.

## Future work

- **Digest xattr for build tools.** Expose the stored SHA-256 as an xattr (e.g. `user.sha256` for
  Bazel's `--unix_digest_hash_attribute_name`) so builds don't read every input.
- **Mount survival across restarts**, if daily failures from restarts become a problem:
  - Upgrades: the old daemon finishes in-flight requests and passes the `/dev/fuse` fd and its
    in-memory tables (open file handles, lookup counts) to the new daemon over a Unix socket. This
    works on any kernel.
  - Crashes: keep the fd in systemd's fd store, persist the file-handle tables, and replay in-flight
    requests with `FUSE_NOTIFY_RESEND` (Linux 6.9+).
- **Periodic auto-snapshots**, if edits should reach the service without the user running a jj
  command. The daemon would run the CLI's snapshot when a workspace is dirty, like jj's watchman
  `register-snapshot-trigger`. Cost: many more operations in the workspace's op log, and more
  op-head merges with commands the user runs at the same time.
