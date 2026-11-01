# M2 Implementation Plan: Shared repository without a working copy

Detailed plan for [M2](mvp-impl-roadmap.md#m2-shared-repository-without-a-working-copy). jj commands
that read and rewrite history work through the stack, with no files checked out. See
[DESIGN.md](../spec/DESIGN.md) for the overall design and
[m1-implementation.md](m1-implementation.md) for the skeleton this builds on.

**Status:** Not started.

## Outcome

- `jjc cloud repo create <repo>` creates a cloud repo on the service. Workspaces are created
  separately, by name: `jjc cloud workspace create <repo> <name> [path]` registers a workspace for
  the user in `user.email` and sets up its repo directory at `path` (default `./<name>`), with `@` a
  new empty commit on `root()`. `jjc cloud workspace list` and `delete` list and delete the user's
  workspaces on the service. Later commands in the directory fail clearly if `user.email` changes.
  See [ADR 0001](../adr/0001-op-log-per-workspace.md).
- In that directory, `jj new`, `describe`, `rebase`, `log`, `op log` and `undo` work with
  `--ignore-working-copy`. Commands that would snapshot or check out fail with a clear error, and
  stock `jj workspace add`, `forget` and `rename` fail.
- Two mutating commands run with the same `--at-op` publish sibling op heads, and the next command
  merges them.
- Op heads are kept per workspace. Workspaces never see or change each other's op heads, even the
  same user's, a new workspace starts at the root operation, and op heads of an unknown workspace
  are `NotFound`.
- The service stores verified objects and op heads on local disk. The daemon persists written
  objects, uploads them in the background, caches reads, fetches the commit graph in bulk, and
  publishes an operation only once every object written before it is on the service.

Not in M2: trunk, `jjc cloud fetch` and `jjc cloud push`, and starting a new workspace on trunk
(M3); opening a workspace on another machine, the holder token, and moving repo directories to the
daemon (M4); working copies (M4, M5); `GetTreeRecursive` and daemon blob metadata (M4); cache
eviction and cleanup of objects left by failed publishes (M7).

## Decisions

### Objects and IDs

- **IDs:** 20-byte SHA-1 with collision detection (sha1dc) over `"<kind> <len>\0<content>"`, as Git
  does. Kinds are `commit`, `tree`, `blob`, `operation` and `view`. On the wire an object is
  `(id, kind, content)`, with no header. The CLI never hashes: the daemon computes every ID.
- **Operations and views** are serialized with stock jj's `simple_op_store` protobufs
  (`jj_lib::protos::simple_op_store`). jj-lib's conversion functions are private, so `jjc` keeps a
  copy of them (Apache-2.0, attributed in the file header), checked on every jj upgrade.
- **Commits and trees** are encoded byte for byte as jj 0.46's `GitBackend` writes them, including
  the `change-id`, `jj:trees` and `jj:conflict-labels` headers. The parent is omitted when it is the
  root commit. The encoding is copied and adapted from `git_backend.rs` (Apache-2.0).
- **Root objects:** the root commit and root operation have all-zero 20-byte IDs. The CLI
  synthesizes them, the root view and the empty tree (`4b825dc…`), as stock jj does. They are never
  stored or uploaded.
- **Not supported:** commit signing (`write_commit` fails with
  `commit signing isn't supported in cloud repos`), copy records (`get_copy_records` returns an
  empty stream; there's no `GetCopyRecords` RPC), tracked copies (`read_copy`, `write_copy` and
  `get_related_copies` return `Unsupported`, as `GitBackend` does), and `jj util gc` (the op store's
  `gc` returns an error).

### Repos and publishing

- **Repo names** match `[A-Za-z0-9][A-Za-z0-9._-]{0,99}`.
- **Users:** the user is jj's `user.email`, sent on every workspace and op-heads call. The service
  accepts any non-empty string of at most 254 bytes with no control characters, and otherwise
  returns `InvalidArgument`. Users aren't registered.
- **Workspace names** follow the repo name rule and are unique per `(repo, user)`. The name is also
  the jj workspace name in the View.
- **Repo creation:** `Repo.CreateRepo(repo)` records the repo and returns `AlreadyExists` if it
  exists. It creates no workspace. M3 adds trunk and treats a repo with no stored trunk as being at
  the root commit, so M2 stores nothing for it.
- **Workspace creation:** `CreateWorkspace(repo, user, name)` registers the workspace with op heads
  `{root op ID}`, and returns `AlreadyExists` if the user already has it. The `cloud` op-heads
  initializer only writes its files, so jj-lib's `ReadonlyRepo::init` never adds the root operation
  as an op head. `Workspace::init_with_factories`, given the workspace name, then publishes the
  workspace's first real operation (the workspace and its `@`) with
  `UpdateOpHeads(repo, user, workspace, [root], new)`.
- **Workspace deletion:** `DeleteWorkspace(repo, user, name)` drops the registry entry and op heads.
  Objects stay. In M2 the CLI doesn't touch the repo directory; later commands there fail with
  `NotFound`.
- **Storage:** one content-addressed object store shared by all repos and users, plus a workspace
  registry and op heads per `(repo, user, workspace)`. Workspace and op-heads calls return
  `NotFound` for an unknown repo, and op-heads calls return `NotFound` for an unknown workspace.
- **Publish barrier:** the daemon's `UpdateOpHeads` waits until every object written to the daemon
  before the call is uploaded. It doesn't parse the operation or View. The service's `UpdateOpHeads`
  rejects a new operation it doesn't have with `FailedPrecondition`.
- **Publishing with the service unreachable (R9):**
  - While the barrier waits, the first failed upload attempt for an object it waits on makes
    `UpdateOpHeads` fail with `Unavailable`, naming the cause, e.g.
    `upload of <id> failed: deadline exceeded after 10s`. It doesn't wait for retries. An object
    that is backing off when the barrier starts is retried at once.
  - Objects stay in the primary store, and the uploader retries them in the background with
    exponential backoff from 1 s to 60 s.
  - The jj command fails and op heads don't change. The next command publishes again without
    re-uploading objects the service already has.
- **Op-heads lock:** `OpHeadsStore::lock()` is jj-lib's `FileLock` on `.jj/repo/op_heads/lock` in
  the repo directory.

### Reading history

- **Op ID prefixes:** `OpStore` RPCs carry the repo name. The daemon records the ID of every op it
  reads, writes or gets from `GetOpHeads` for that repo, and resolves prefixes against that list.
  It's complete in practice because building the index walks the whole op log. The CLI also
  considers the root op ID. A full-length ID is looked up directly.
- **Op-log round trips:** one round trip per op or View missing from the cache. Bulk fetch is an M7
  open question.
- **Bulk commit-graph fetch:** `jjc` registers a `cloud` index store that wraps jj's
  `DefaultIndexStore`. In `get_index_at_op` for an op that isn't indexed yet:
  1. It walks back the op log as `build_index_at_operation` does, stopping at the first op with an
     `index/op_links/<op id>` marker. This depends on jj's internal index layout, checked on every
     jj upgrade.
  2. It collects every commit ID those ops and their Views reference, hidden commits included,
     because the builder indexes them all. The root commit ID is dropped.
  3. It calls `Store.FetchCommitGraph` once with them, then hands off to the default store.
- **Complete heads (daemon):** a persisted set of commits whose whole ancestry is cached.
  - `FetchCommitGraph` skips requested IDs already in the set. If none remain, it makes no call.
  - After a successful fetch, the requested IDs join the set. A commit the daemon writes joins it if
    it has no parents or all its parents are in the set.
  - When a commit joins the set, its parents leave it.
  - At most the 1000 most recently added entries are sent as `known_heads`. Hidden commits stay in
    the set as heads, so it grows with rewrites. Sending fewer heads only means re-sending some
    commits.
- **`GetCommitGraph`:** returns exactly "reachable from `heads`, not from `known_heads`". It walks
  both sides in one priority queue ordered by generation number and stops when only excluded commits
  remain. A commit's generation (no parents: 1, else 1 + max over parents) is computed on first need
  and stored. Commits can be uploaded before their parents, so it can't be computed on upload.

### Sizes

- Any object over 1 MiB, of any kind, goes through `DownloadObject` and `UploadObject` in 1 MiB
  chunks. `GetObjects` returns `too_large(size)` for it.
- A batch (`GetObjects`, `PutObjects`, `GetCommitGraph` stream messages) carries at most 4 MiB of
  content. The service, the daemon and the CLI's client all accept messages up to 8 MiB.
- The daemon's `ReadFile` and `WriteFile` stream to and from the CLI in 1 MiB chunks. Other `Store`
  and `OpStore` RPCs are unary.

### Storage on disk

- **Object layout** (service and daemon): `objects/<2 hex>/<38 hex>`, stored as the Git header plus
  content, uncompressed. Writes go to a temp file, fsync, rename.
- **Service:** `--data-dir` is required.
  - Objects go in `<data-dir>/objects/`. A sidecar `<object>.meta` holds the upload time,
    `(size, SHA-256)` for blobs, and the generation for commits once computed.
  - A repo is the directory `<data-dir>/repos/<repo>/`. A workspace is the directory
    `<data-dir>/repos/<repo>/workspaces/<user>/<name>/`, with the email percent-encoded (everything
    outside `[A-Za-z0-9._@-]`). Its `op_heads` file holds one hex ID per line, rewritten atomically
    under a per-repo mutex that also covers creating and deleting workspaces. This assumes a single
    service process.
  - Everything sits behind a Go interface that M7 replaces.
- **Daemon:** `--data-dir` defaults to `$XDG_STATE_HOME/jjc/srcd`, or `~/.local/state/jjc/srcd` if
  that's unset.
  - `primary/objects/`: written objects not yet confirmed on the service. An object moves to the
    cache by rename once its upload is confirmed. At startup, the uploader re-queues whatever is
    left.
  - `cache/objects/`: confirmed or fetched objects. No eviction until M7.
  - `repos/<name>/op_ids`: append-only list of known op IDs, for prefix resolution.
  - `complete_heads`: the complete-heads set, rewritten atomically.
  - The daemon re-hashes fetched objects before caching them. It computes no blob metadata until M4.

### CLI

- **Repo directory in M2** is the `path` given to `jjc cloud workspace create`, `./<name>` by
  default as `jj git clone` names its destination. With no working copy yet, it's where the user
  runs `jj`, so it isn't in a daemon-managed location; M4 moves it there. Each workspace has its own
  repo directory and commit index. Its `.jj/repo` has `store` (`cloud`), `workspace_store` (stock
  `simple`), `op_store` and `op_heads` (`cloud`, each holding a `repo_name` file), `index` (`cloud`)
  and `submodule_store` (stock). `op_heads` also holds `user_email` and `workspace_name` files,
  written by its initializer.
- **User check:** the `cloud` op-heads store loader compares `user_email` with `user.email` from the
  settings it's given. A mismatch fails with
  `this repo directory belongs to <old>; user.email is <new>` and the hint
  `set it back, e.g. for this repo only: jj config set --repo user.email <old>`. An empty
  `user.email` fails with `user.email isn't set` and a hint to set it. Every command that loads the
  repo goes through the loader, so none can publish to another user's op heads by accident. Op heads
  of a deleted workspace fail with `workspace <name> doesn't exist in repo <repo>`.
- **Stock workspace commands:** the dispatch hook that blocks stock repo creation also rejects
  `jj workspace add`, `forget` and `rename` in a cloud repo with
  `workspaces are managed with jjc cloud workspace` and a hint naming `jjc cloud workspace create`
  or `delete`. `jj workspace list` and `root` work.
- **Working copy `cloud-none`:** stores the op ID and tree ID it was initialized with.
  `start_mutation` fails with `this workspace has no working copy` and the hint
  `pass --ignore-working-copy`, so any command that would snapshot or check out fails.
- **Default config layer** (`CliRunner::add_extra_config`), which users can override:
  - `revset-aliases."trunk()" = 'latest(present(main@origin) | root())'`. Cloud repos have no Git
    remotes, so the stock alias finds nothing. `main@origin` appears once M3 adds fetch and push;
    until then `trunk()` is `root()`.
  - `revsets.log` and `immutable_heads()` stay stock: a View holds only its workspace's commits.
- **Daemon client:** `DaemonClient` becomes cheaply cloneable with `&self` methods (tonic channels
  clone), shared by the store shims, the working copy and commands.

## Repo layout

New and changed paths:

```
proto/jjc/sourceapi/v1/source_api.proto   object, repo, workspace and op-heads RPCs
proto/jjc/srcd/v1/srcd.proto              Store, OpStore, OpHeads, Repo, Workspace RPCs
lib-go/gitobj/                    kinds, sha1dc IDs, commit parent parsing, blob metadata
lib-go/objstore/                  on-disk content-addressed object directory (service and daemon)
lib-go/sourceapi/storage/         Storage interface and its file implementation
lib-go/sourceapi/                 RPC handlers: objects.go, repos.go, workspaces.go, commitgraph.go
lib-go/srcd/localstore/           primary store, cache, complete heads, op ID lists
lib-go/srcd/uploader/             background uploader and publish barrier
lib-go/srcd/sourceapiclient/      batched and chunked object calls, op heads, commit graph
lib-go/srcd/                      handlers: store.go, opstore.go, opheads.go, workspace.go
bin-rs/jjc/src/store.rs           StoreFactories for the cloud backend, op store, op heads, index
bin-rs/jjc/src/store/backend.rs
bin-rs/jjc/src/store/git_encoding.rs     commit and tree encoding, copied from git_backend.rs
bin-rs/jjc/src/store/op_store.rs
bin-rs/jjc/src/store/op_proto.rs         op and view conversion, copied from simple_op_store.rs
bin-rs/jjc/src/store/op_heads_store.rs
bin-rs/jjc/src/store/index_store.rs
bin-rs/jjc/src/working_copy.rs    cloud-none working copy and factory
bin-rs/jjc/src/default_config.rs  default config layer
bin-rs/jjc/src/commands/cloud/repo.rs      jjc cloud repo create
bin-rs/jjc/src/commands/cloud/workspace.rs jjc cloud workspace create, list, delete
e2e/tests/repo.rs                 happy path
e2e/tests/repo_errors.rs          CLI unhappy paths against the real daemon and service
```

## 1. Go object package

Outcome: one Go implementation of object IDs and parsing, shared by the service and the daemon.

- [ ] `lib-go/gitobj`:
  - `type Kind` (`Commit`, `Tree`, `Blob`, `Operation`, `View`), `String()` giving the header word,
    and conversion to and from the proto `ObjectKind`.
  - `type ID [20]byte` with `Hex()`, `ParseHex` and `RootID` (all zeros).
  - `Hash(kind, content) ID`: sha1dc (`github.com/pjbgf/sha1cd`) over the header plus content.
    Returns an error when a collision attack is detected.
  - `CommitParents(content) ([]ID, error)`: the `parent` header lines of a commit.
  - `BlobMeta(content) (size uint64, sha256 [32]byte)`.
- [ ] `lib-go/objstore`: `Open(dir)`, `Put(id, kind, content)` (atomic, idempotent),
      `Get(id) (kind, content, error)`, `Has(id)`, `Size(id)`, `Remove(id)`, `MoveTo(other, id)`
      (rename into another directory on the same filesystem) and `List()`. It doesn't hash; callers
      verify.

Tests:

- [ ] Unit: `Hash` matches Git for the empty blob (`e69de29…`), the empty tree (`4b825dc…`) and a
      sample commit, and reports the SHAttered collision PDF as an attack.
- [ ] Unit: `CommitParents` for no, one and two parents, and for a malformed commit.
- [ ] Unit: `objstore` round trip, idempotent `Put`, `MoveTo`, `List`, and no partial file left when
      a write fails.

## 2. Service object storage

Outcome: the service stores verified objects with blob metadata and serves them in batches and
streams.

- [ ] `source-api --data-dir <dir>` (required).
- [ ] `lib-go/sourceapi/storage`: `Storage` interface with `PutObject`, `GetObject`, `HasObjects`,
      `ObjectSize`, `Generation`/`SetGeneration`, and the repo methods of section 3.
      `NewFS(dataDir)` is the file implementation over `objstore` plus sidecars.
- [ ] `source_api.proto`: `ObjectKind`, `Object { bytes id; ObjectKind kind; bytes content; }`, and
      these RPCs on `Objects`:
  - `PutObjects(objects[])`: re-hashes each object and rejects the whole batch with
    `InvalidArgument` naming the first ID that doesn't match its content. Writes blob metadata and
    upload time.
  - `UploadObject(stream: header {id, kind, size}, then chunks)`: same verification. Rejects a size
    mismatch.
  - `HasObjects(ids[]) → missing[]`.
  - `GetObjects(ids[]) → results[]`: each result is the object, `too_large(size)` for objects over 1
    MiB, or `missing`.
  - `DownloadObject(id) → stream`: first message `{kind, size}`, then 1 MiB chunks. `NotFound` for
    an unknown ID.
- [ ] Servers accept 8 MiB messages.

Tests:

- [ ] Integration (in-process gRPC server on a temp data dir): put then get each kind. A content/ID
      mismatch is rejected and nothing from the batch is stored. `HasObjects` reports only missing
      IDs. Blob metadata is stored.
- [ ] Integration: a 3 MiB blob uploads in chunks, `GetObjects` returns `too_large(3 MiB)` for it,
      and `DownloadObject` returns the same bytes. A size mismatch in `UploadObject` is rejected.
- [ ] Integration: objects survive a server restart on the same data dir.

## 3. Service repos, workspaces and op heads

Outcome: the service creates repos, keeps a registry of workspaces, and keeps each workspace's op
heads with atomic updates.

- [ ] `Repo.CreateRepo(repo)`: validates the name (`InvalidArgument`), creates the repo directory,
      `AlreadyExists` if the repo exists.
- [ ] `Workspace.CreateWorkspace(repo, user, name)`: `NotFound` for an unknown repo,
      `InvalidArgument` for an invalid user or name, `AlreadyExists` if the user has the workspace.
      Creates the workspace directory with op heads `{root op ID}`.
- [ ] `Workspace.ListWorkspaces(repo, user) → names[]`: `NotFound` for an unknown repo, sorted
      names, empty for a user with none.
- [ ] `Workspace.DeleteWorkspace(repo, user, name)`: `NotFound` for an unknown repo or workspace.
      Removes the workspace directory.
- [ ] `OpHeads.GetOpHeads(repo, user, workspace) → op_ids[]`: `NotFound` for an unknown repo or
      workspace, `InvalidArgument` for an invalid user.
- [ ] `OpHeads.UpdateOpHeads(repo, user, workspace, old_ids[], new_id)`: `NotFound` for an unknown
      repo or workspace, `InvalidArgument` for an invalid user, `FailedPrecondition` if `new_id`
      isn't stored as an operation. Removes `old_ids` from the workspace's op heads (ignoring ones
      already gone) and adds `new_id`, atomically under the per-repo mutex. The holder token comes
      in M4.

Tests:

- [ ] Integration: create repo, create workspace, get (`{root}`), update, get. `AlreadyExists` on a
      second create of either, and `InvalidArgument` for repo and workspace names `""`, `.x`, `a/b`
      and a 101-character name.
- [ ] Integration: two workspaces of one user and one workspace of another user with the same name
      as one of them. Updates in one never change the others' op heads, and each new one reads
      `{root}`. `InvalidArgument` for an empty user and one with a newline. An email with `/` and
      `%` round-trips through its percent-encoded directory name.
- [ ] Integration: list returns only the user's workspaces. After delete, list omits it, both
      op-heads RPCs return `NotFound`, and the name can be created again starting at `{root}`.
- [ ] Integration: `NotFound` from the workspace and op-heads RPCs for an unknown repo and an
      unknown workspace, and `FailedPrecondition` for an op that isn't stored.
- [ ] Integration: 20 parallel clients each publish an op to one workspace from the same old head.
      All 20 end up as its heads, and the root is gone.
- [ ] Integration: the registry and op heads survive a restart.

## 4. Service commit graph

Outcome: the service streams exactly the commits a machine is missing.

- [ ] `GetCommitGraph(heads[], known_heads[]) → stream { commits[] }`: every commit reachable from
      `heads` and not from `known_heads`, in batches of at most 4 MiB. `NotFound` if a head isn't
      stored. Unknown `known_heads` are ignored. `DataLoss` naming the commit if an ancestor is
      missing. Generations are computed iteratively (no recursion) and stored.

Tests:

- [ ] Integration: linear history, `known_heads` empty, then at the old tip: the full history, then
      only the new commits.
- [ ] Integration: a branch forked from an old trunk commit, with the current trunk tip known: only
      the branch commits, none of trunk.
- [ ] Integration: merges, a known head that is a descendant of a requested head (nothing returned),
      and a history over 4 MiB split across several messages.
- [ ] Integration: generations are still there after a restart, and a child uploaded before its
      parent gets the right generation.

## 5. Daemon local store and reads and writes

Outcome: the daemon serves `Store` and `OpStore` from local storage, with a fetch on a miss.

- [ ] `srcd --data-dir` (default `$XDG_STATE_HOME/jjc/srcd`, then `~/.local/state/jjc/srcd`).
- [ ] `lib-go/srcd/localstore`: primary and cache `objstore`s, `Read(id)` (cache, then primary),
      `Write(kind, content) → id` (hash, fsync'd write to primary unless already stored), op ID
      lists per repo, and the complete-heads set.
- [ ] `lib-go/srcd/sourceapiclient`: `GetObjects` in batches of at most 4 MiB of expected content,
      falling back to `DownloadObject` for `too_large`. Every call is bounded by
      `--source-api-timeout`.
- [ ] `srcd.proto`:
  - `Store`: `ReadCommit`, `ReadTree`, `ReadSymlink` (`id → content`), `WriteCommit`, `WriteTree`,
    `WriteSymlink` (`content → id`), `ReadFile(id) → stream chunks`,
    `WriteFile(stream chunks) → id`. A read checks the stored kind (`InvalidArgument` on a
    mismatch). `NotFound` if the service doesn't have it. `Unavailable` with the cause if the
    service can't be reached.
  - `OpStore`: `ReadOperation(repo, id)`, `ReadView(repo, id)`, `WriteOperation(repo, content)`,
    `WriteView(repo, content)`, and
    `ResolveOpIdPrefix(repo, hex_prefix) → id | ambiguous | not_found`. Each records the op IDs it
    sees for the repo.
- [ ] A fetched object is re-hashed and checked against the requested ID and kind before it's
      cached. A mismatch fails with `DataLoss` and isn't cached.
- [ ] Writes are queued for upload once section 6 exists. Until then they stay in primary.

Tests:

- [ ] Integration (real in-process service): a write then a read hits local storage with the service
      stopped. A read miss fetches from the service and then works with the service stopped.
- [ ] Integration: a 3 MiB blob is read through `DownloadObject` and streamed to the client in
      chunks, and `WriteFile` accepts it streamed.
- [ ] Integration: a service wrapper that returns corrupted content: the read fails with `DataLoss`
      and nothing is cached. A kind mismatch is rejected.
- [ ] Integration: prefix resolution finds a unique op, and reports ambiguous and unknown prefixes.
      The list survives a daemon restart.

## 6. Daemon uploads, op heads, repos and workspaces

Outcome: written objects reach the service in the background, and an operation is published only
once everything written before it is uploaded.

- [ ] `lib-go/srcd/uploader`:
  - Each written object gets a sequence number. Batches of at most 4 MiB go through `HasObjects`,
    then `PutObjects` for the missing ones. Objects over 1 MiB go through `UploadObject`.
  - Once an upload is confirmed, the object moves from primary to cache.
  - At startup, it re-queues everything in primary.
  - Failures back off exponentially from 1 s to 60 s.
  - `Barrier(ctx)`: captures the current sequence number and waits until every object up to it is
    confirmed. It fails at the first failed attempt for one of those objects, and retries any of
    them that are backing off at once.
- [ ] `OpHeads.GetOpHeads(repo, user, workspace)`: forwarded to the service on every call.
- [ ] `OpHeads.UpdateOpHeads(repo, user, workspace, old_ids[], new_id)`: `Barrier`, then the service
      call. The barrier failing returns `Unavailable` with the upload error. `NotFound` and
      `InvalidArgument` pass through.
- [ ] `Repo.CreateRepo(repo)`, `Workspace.CreateWorkspace(repo, user, name)`,
      `Workspace.ListWorkspaces(repo, user)` and `Workspace.DeleteWorkspace(repo, user, name)`:
      forwarded to the same RPCs on the source API. Errors pass through. Mounts and repo directories
      come in M4.

Tests:

- [ ] Integration: written objects reach the service and move to the cache. Objects the service
      already has aren't sent again (`PutObjects` is counted by a wrapper).
- [ ] Integration: a wrapper fails `PutObjects`. `UpdateOpHeads` fails quickly with `Unavailable`,
      the service's op heads are unchanged, and the upload succeeds after the wrapper stops failing.
- [ ] Integration: a wrapper stalls `PutObjects`. `UpdateOpHeads` fails after about
      `--source-api-timeout`, not after the CLI deadline.
- [ ] Integration: a daemon restarted on the same data dir uploads what was left in primary.
- [ ] Integration: `CreateRepo` and `CreateWorkspace` pass `AlreadyExists` through.
- [ ] Integration: two users through one daemon, one with two workspaces, publish to the same repo,
      and each workspace reads back only its own op heads.

## 7. Daemon bulk commit-graph fetch

Outcome: the daemon fetches the commits the index builder needs in one call.

- [ ] `Store.FetchCommitGraph(commit_ids[])`: implements the complete-heads rules above. Each
      streamed commit is verified and cached. The complete-heads set is updated only after the whole
      stream succeeds.
- [ ] `WriteCommit` adds the commit to the complete-heads set when the rules allow it.

Tests:

- [ ] Integration: a cold fetch caches the full history. Then reads work with the service stopped.
- [ ] Integration: a second fetch for a newer head sends the earlier heads as `known_heads` and
      receives only new commits (counted by a wrapper).
- [ ] Integration: IDs that are all complete make no service call. The set survives a restart.
- [ ] Integration: a stream that fails midway leaves the set unchanged.

## 8. CLI encoding

Outcome: the CLI encodes objects, operations and views exactly as stock jj does.

- [ ] `bin-rs/jjc` depends on `gix-object` at jj-lib's version.
- [ ] `store/git_encoding.rs`: `encode_commit(&Commit) -> Vec<u8>`,
      `decode_commit(id, &[u8]) ->     Commit`, `encode_tree(&Tree)`, `decode_tree(&[u8])`, adapted
      from `git_backend.rs`.
- [ ] `store/op_proto.rs`: `operation_to_proto`, `operation_from_proto`, `view_to_proto` and
      `view_from_proto`, copied from `simple_op_store.rs` with the Apache-2.0 notice.

Tests:

- [ ] Unit: write commits through jj-lib's `GitBackend` in a temp dir and through `encode_commit`,
      and compare the raw bytes from the Git object store. Cases: a root-parent commit, a merge, a
      conflicted tree (`jj:trees`, conflict labels), and multi-line descriptions. Decoding the bytes
      gives back the same `Commit`.
- [ ] Unit: trees with files, executables, symlinks and subtrees round-trip and match `GitBackend`.
- [ ] Unit: an operation and a View with bookmarks, tags, several workspaces and conflicted refs,
      written through stock `SimpleOpStore` and through `op_proto`, give the same protobuf bytes and
      decode back.

## 9. CLI store shims, repo and workspace commands, and defaults

Outcome: `jjc cloud repo create` and `jjc cloud workspace create` make a working workspace, and jj
history commands work in it.

- [ ] `srcd.proto` client RPCs for `Store`, `OpStore`, `OpHeads`, `Repo` (`CreateRepo`) and
      `Workspace` (`CreateWorkspace`, `ListWorkspaces`, `DeleteWorkspace`), and matching
      `DaemonClient` methods. The client accepts 8 MiB messages. gRPC errors map to jj-lib errors:
      `NotFound` to `ObjectNotFound`, `Unavailable` and others to an error carrying the daemon's
      message.
- [ ] `store.rs` registers, as type `cloud`:
  - `store/backend.rs`: `Backend` over `Store`, with commit ID length 20 and change ID length 16.
    The root commit and empty tree are synthesized. Signing, copy records, tracked copies and `gc`
    behave as in [Decisions](#objects-and-ids).
  - `store/op_store.rs`: `OpStore` over `OpStore`, reading `repo_name`. The root operation and root
    view are synthesized.
  - `store/op_heads_store.rs`: `OpHeadsStore` over `OpHeads`, with the file lock. The initializer
    writes `repo_name`, `user_email` and `workspace_name` and publishes nothing, and the loader does
    the user check and maps `NotFound` to the deleted-workspace error.
  - `store/index_store.rs`: the `DefaultIndexStore` wrapper with the bulk commit-graph fetch.
    `collect_unindexed_commit_ids(index_dir, op)` is a separate function.
- [ ] `working_copy.rs`: the `cloud-none` working copy and factory.
- [ ] `default_config.rs`: the default config layer.
- [ ] `commands/cloud/repo.rs`: `jjc cloud repo create <repo>` calls `Repo.CreateRepo(repo)`.
      `AlreadyExists` becomes `repo <repo> already exists on the source API`, and `InvalidArgument`
      passes the name rule through.
- [ ] `commands/cloud/workspace.rs`:
  - `jjc cloud workspace create <repo> <name> [path]`, with `path` defaulting to `./<name>`:
    1. Fails with `<path> already has a .jj directory`, or with the unset `user.email` error, before
       calling the daemon.
    2. Calls `Workspace.CreateWorkspace(repo, user, name)`. `AlreadyExists` becomes
       `workspace <name> already exists in repo <repo>`, `NotFound` becomes
       `repo <repo> doesn't exist`, and `InvalidArgument` passes the name rule through.
    3. Runs `Workspace::init_with_factories` with the `cloud` factories and the workspace name, and
       prints the repo, workspace and path.

    If step 3 fails, the workspace is registered with no repo directory. The error says so and
    suggests `jjc cloud workspace delete <repo> <name>` before creating it again.

  - `jjc cloud workspace list <repo>`: one name per line.
  - `jjc cloud workspace delete <repo> <name>`: deletes the workspace on the service and leaves any
    repo directory in place, saying so.
- [ ] Dispatch hook: rejects `jj workspace add`, `forget` and `rename` in cloud repos (see
      [CLI](#cli)).

- [ ] `main.rs`: `add_store_factories`, `add_working_copy_factories` and `add_extra_config`. The
      `git init`, `git clone` and `debug init-simple` block stays.
- [ ] Harness: `SourceApi::start` and `Daemon::start` pass temp `--data-dir`s.
      `SourceApi::restart()` restarts on the same data dir and port. `TestEnv` sets jj's
      deterministic test variables (`JJ_RANDOMNESS_SEED`, `JJ_TIMESTAMP`, `JJ_OP_TIMESTAMP`,
      `JJ_OP_HOSTNAME`, `JJ_OP_USERNAME`) and a fixed `user.email`.

Tests:

- [ ] Unit: `collect_unindexed_commit_ids` on a stock local repo in a temp dir, with ops that
      abandon and rewrite commits. It includes the abandoned commit and the rewritten predecessor,
      and stops at an op that has an `op_links` marker.
- [ ] Unit: the mapping from gRPC errors to jj-lib errors.
- [ ] E2E (`e2e/tests/repo.rs`): `repo_lifecycle`. `repo create`, then `workspace create` with no
      `path` (the repo directory is `./<name>`, and `jj log` shows `<name>@`), then `jj new`,
      `describe`, another `new`, `rebase`, `log` (snapshot of the default revset and `trunk()`),
      `op log` and `undo`, all with `--ignore-working-copy`.
- [ ] E2E: `independent_workspaces`. Two workspaces of one user in one repo. Commits and operations
      in the first don't appear in the second's `log` or `op log`, the second starts on the root
      commit, and `jj undo` in the second leaves the first unchanged. `workspace list` shows both.
- [ ] E2E: `concurrent_at_op`. Two `describe`s with the same `--at-op` publish two op heads, and the
      next `jj log` reports the concurrent operations, merges them, and shows both changes.
- [ ] Integration (`e2e/tests/repo_errors.rs`, using the harness):
  - `repo create` with an existing name, and with an invalid name: the errors.
  - `workspace create` with an existing name: the error, and no `.jj` created.
  - `workspace create` in an unknown repo: the error.
  - `workspace create` where `.jj` exists, and with `user.email` unset: the errors, and the name is
    still free.
  - `workspace create` with an invalid name: the name rule in the error.
  - `jj log` in a workspace deleted with `workspace delete`: the deleted-workspace error.
  - `jj workspace add`, `forget` and `rename`: the error and hint, and no new operation.
  - `jj log` with a different `user.email` than at creation: the mismatch error and hint, and no new
    operation for either email. With `--config user.email=<old>` it works.
  - `jj log` without `--ignore-working-copy`: `this workspace has no working copy` and the hint.
  - `jj describe` with signing configured: the signing error, and no new operation.
  - The service killed, then `jj describe`: fails with the upload error. After
    `SourceApi::restart()`, `op log` shows no new operation and the same `describe` succeeds.

## 10. Spec, roadmap and glossary updates

After the code is in:

- [ ] [DESIGN.md](../spec/DESIGN.md):
  - Object model: operation and view IDs, root objects, the wire form `(id, kind, content)`.
  - Publishing: the barrier and how it fails fast when the service is unreachable.
  - Repository model: repo names. Workspaces, the registry and per-workspace op heads are already in
    the spec ([ADR 0001](../adr/0001-op-log-per-workspace.md)).
- [ ] [source-api.md](../spec/source-api.md):
  - `--data-dir` and the storage layout behind the `Storage` interface.
  - `CreateRepo`, the `NotFound`, `InvalidArgument` and `FailedPrecondition` cases, user key
    validation and encoding, message sizes and chunking. The workspace RPCs are already in the spec;
    check their errors match.
  - `GetCommitGraph` exactness with stored generation numbers.
- [ ] [srcd.md](../spec/srcd.md):
  - `--data-dir` and the local storage layout.
  - The uploader, its backoff, and the barrier.
  - The final `Store`, `OpStore`, `OpHeads` and `Workspace` RPCs, with repo names on `OpStore` and
    prefix resolution.
  - Complete heads in `FetchCommitGraph`, fetch verification, and the op-heads file lock.
  - Remove `GetCopyRecords`. Blob metadata starts in M4.
- [ ] [jjc.md](../spec/jjc.md):
  - The narrowed repo creation block. `repo create` and the `workspace` commands are already in the
    spec; check their errors match.
  - Operation and view encoding, the copied conversion code, and the root objects.
  - The `cloud-none` working copy.
  - Commit index: replace `FetchCommitGraph(view heads)` with the wrapper's walk over unindexed ops,
    hidden commits included.
  - User identity: the `user_email` and `workspace_name` files in the repo directory. The check and
    its error are already in the spec.
  - v1 limitations: no signing, no copy records or tracked copies, no `jj util gc`.
- [ ] [GLOSSARY.md](../../GLOSSARY.md): already has **Workspace**, **Workspace name**, **Repo
      directory**, **Op heads** and **Publish**. Check that nothing in the implementation changed
      their meaning.
- [ ] Mark M2 and this plan done in the [roadmap](mvp-impl-roadmap.md).

## Open questions

None blocking M2.
