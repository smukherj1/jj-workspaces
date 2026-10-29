# M2 Implementation Plan: Shared repository without a working copy

Detailed plan for [M2](mvp-impl-roadmap.md#m2-shared-repository-without-a-working-copy). jj commands
that read and rewrite history work through the stack, with no files checked out. See
[DESIGN.md](../spec/DESIGN.md) for the overall design and
[m1-implementation.md](m1-implementation.md) for the skeleton this builds on.

**Status:** Not started.

## Outcome

- `jjw cws repo create <name> [path]` creates a cloud repo on the service and sets up its repo
  directory at `path` for the user in `user.email`, with a `default` workspace whose `@` is on
  `root()`. Later commands there fail clearly if `user.email` changes.
- In that directory, `jj new`, `describe`, `rebase`, `log`, `op log` and `undo` work with
  `--ignore-working-copy`. Commands that would snapshot or check out fail with a clear error.
- Two mutating commands run with the same `--at-op` publish sibling op heads, and the next command
  merges them.
- Op heads are kept per user. Users never see or change each other's op heads, and a user the
  service hasn't seen starts at the root operation.
- The service stores verified objects and op heads on local disk. The daemon persists written
  objects, uploads them in the background, caches reads, fetches the commit graph in bulk, and
  publishes an operation only once every object written before it is on the service.

Not in M2: trunk, `jjw cws fetch` and `jjw cws push`, and setting up a repo directory for an
existing repo, for a second user or on another machine (M3); the two-machine outcomes (M3, M4);
working copies (M4, M5); `GetTreeRecursive` and daemon blob metadata (M4); cache eviction and
cleanup of objects left by failed publishes (M7).

## Decisions

### Objects and IDs

- **IDs:** 20-byte SHA-1 with collision detection (sha1dc) over `"<kind> <len>\0<content>"`, as Git
  does. Kinds are `commit`, `tree`, `blob`, `operation` and `view`. On the wire an object is
  `(id, kind, content)`, with no header. The CLI never hashes: the daemon computes every ID.
- **Operations and views** are serialized with stock jj's `simple_op_store` protobufs
  (`jj_lib::protos::simple_op_store`). jj-lib's conversion functions are private, so `jjw` keeps a
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
- **Users:** the user is jj's `user.email`, sent on every op-heads call. The service accepts any
  non-empty string of at most 254 bytes with no control characters, and otherwise returns
  `InvalidArgument`. Users aren't registered.
- **Repo creation:** `WorkspaceService.CreateRepo(name)` records the repo and returns
  `AlreadyExists` if it exists. It doesn't touch op heads: `GetOpHeads(repo, user)` for a user with
  no entry returns `{root op ID}`. The `cws` op-heads initializer does nothing, so jj-lib's
  `ReadonlyRepo::init` never adds the root operation as an op head. Workspace init then publishes
  the user's first real operation (the `default` workspace and its `@`) with
  `UpdateOpHeads(repo, user, [root], new)`, which creates the user's entry. M3 adds trunk and treats
  a repo with no stored trunk as being at the root commit, so M2 stores nothing for it.
- **Storage:** one content-addressed object store shared by all repos and users, plus op heads per
  `(repo, user)`. `GetOpHeads` and `UpdateOpHeads` return `NotFound` for an unknown repo.
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
- **Bulk commit-graph fetch:** `jjw` registers a `cws` index store that wraps jj's
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
  - A repo is the directory `<data-dir>/repos/<name>/`. A user's op heads go in
    `<data-dir>/repos/<name>/op_heads/<user>`, with the email percent-encoded (everything outside
    `[A-Za-z0-9._@-]`), one hex ID per line, rewritten atomically under a per-repo mutex. This
    assumes a single service process.
  - Everything sits behind a Go interface that M7 replaces.
- **Daemon:** `--data-dir` defaults to `$XDG_STATE_HOME/jjw`, or `~/.local/state/jjw` if that's
  unset.
  - `primary/objects/`: written objects not yet confirmed on the service. An object moves to the
    cache by rename once its upload is confirmed. At startup, the uploader re-queues whatever is
    left.
  - `cache/objects/`: confirmed or fetched objects. No eviction until M7.
  - `repos/<name>/op_ids`: append-only list of known op IDs, for prefix resolution.
  - `complete_heads`: the complete-heads set, rewritten atomically.
  - The daemon re-hashes fetched objects before caching them. It computes no blob metadata until M4.

### CLI

- **Repo directory in M2** is the `path` given to `jjw cws repo create`. Its `.jj/repo` has `store`
  (`cws`), `workspace_store` (stock `simple`), `op_store` and `op_heads` (`cws`, each holding a
  `repo_name` file), `index` (`cws`) and `submodule_store` (stock). `op_heads` also holds a
  `user_email` file.
- **User check:** the `cws` op-heads store loader compares `user_email` with `user.email` from the
  settings it's given. A mismatch fails with
  `this repo directory belongs to <old>; user.email is <new>` and the hint
  `set it back, e.g. for this repo only: jj config set --repo user.email <old>`. An empty
  `user.email` fails with `user.email isn't set` and a hint to set it. Every command that loads the
  repo goes through the loader, so none can publish to another user's op heads by accident.
- **Working copy `cws-none`:** stores the op ID and tree ID it was initialized with.
  `start_mutation` fails with `this workspace has no working copy` and the hint
  `pass --ignore-working-copy`, so any command that would snapshot or check out fails.
- **Default config layer** (`CliRunner::add_extra_config`), which users can override:
  - `revset-aliases."trunk()" = 'latest(present(main@origin) | root())'`. Cloud repos have no Git
    remotes, so the stock alias finds nothing. `main@origin` appears once M3 adds fetch and push;
    until then `trunk()` is `root()`.
  - `revsets.log` and `immutable_heads()` stay stock: a View holds only its user's commits.
- **Daemon client:** `DaemonClient` becomes cheaply cloneable with `&self` methods (tonic channels
  clone), shared by the store shims, the working copy and commands.

## Repo layout

New and changed paths:

```
proto/jjcw/service/v1/workspace_service.proto   object, repo and op-heads RPCs
proto/jjcw/daemon/v1/daemon.proto               Store, OpStore, OpHeads, Workspace RPCs
lib-go/gitobj/                    kinds, sha1dc IDs, commit parent parsing, blob metadata
lib-go/objstore/                  on-disk content-addressed object directory (service and daemon)
lib-go/service/storage/           Storage interface and its file implementation
lib-go/service/                   RPC handlers: objects.go, repos.go, commitgraph.go
lib-go/daemon/localstore/         primary store, cache, complete heads, op ID lists
lib-go/daemon/uploader/           background uploader and publish barrier
lib-go/daemon/serviceclient/      batched and chunked object calls, op heads, commit graph
lib-go/daemon/                    handlers: store.go, opstore.go, opheads.go, workspace.go
bin-rs/jjw/src/store.rs           StoreFactories for the cws backend, op store, op heads, index
bin-rs/jjw/src/store/backend.rs
bin-rs/jjw/src/store/git_encoding.rs     commit and tree encoding, copied from git_backend.rs
bin-rs/jjw/src/store/op_store.rs
bin-rs/jjw/src/store/op_proto.rs         op and view conversion, copied from simple_op_store.rs
bin-rs/jjw/src/store/op_heads_store.rs
bin-rs/jjw/src/store/index_store.rs
bin-rs/jjw/src/working_copy.rs    cws-none working copy and factory
bin-rs/jjw/src/default_config.rs  default config layer
bin-rs/jjw/src/commands/cws/repo.rs      jjw cws repo create
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

- [ ] `jjw-workspace-service --data-dir <dir>` (required).
- [ ] `lib-go/service/storage`: `Storage` interface with `PutObject`, `GetObject`, `HasObjects`,
      `ObjectSize`, `Generation`/`SetGeneration`, and the repo methods of section 3.
      `NewFS(dataDir)` is the file implementation over `objstore` plus sidecars.
- [ ] `workspace_service.proto`: `ObjectKind`,
      `Object { bytes id; ObjectKind kind; bytes content; }`.
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

## 3. Service repos and op heads

Outcome: the service creates repos and keeps their op heads with atomic updates.

- [ ] `CreateRepo(repo)`: validates the name (`InvalidArgument`), creates the repo directory,
      `AlreadyExists` if the repo exists.
- [ ] `GetOpHeads(repo, user) → op_ids[]`: `NotFound` for an unknown repo, `InvalidArgument` for an
      invalid user, `{root op ID}` for a user with no entry.
- [ ] `UpdateOpHeads(repo, user, old_ids[], new_id)`: `NotFound` for an unknown repo,
      `InvalidArgument` for an invalid user, `FailedPrecondition` if `new_id` isn't stored as an
      operation. Removes `old_ids` from the user's op heads (ignoring ones already gone) and adds
      `new_id`, atomically under the per-repo mutex.

Tests:

- [ ] Integration: create, get (`{root}`), update, get. `AlreadyExists` on a second create, and
      `InvalidArgument` for `""`, `.x`, `a/b` and a 101-character name.
- [ ] Integration: two users in one repo. Updates by one never change the other's op heads, a third
      user still reads `{root}`, and `InvalidArgument` for an empty user and one with a newline. An
      email with `/` and `%` round-trips through its percent-encoded file name.
- [ ] Integration: `NotFound` from both op-heads RPCs for an unknown repo, and `FailedPrecondition`
      for an op that isn't stored.
- [ ] Integration: 20 parallel clients, as one user, each publish an op from the same old head. All
      20 end up as that user's heads, and the root is gone.
- [ ] Integration: op heads survive a restart.

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

- [ ] `jjw-daemon --data-dir` (default `$XDG_STATE_HOME/jjw`, then `~/.local/state/jjw`).
- [ ] `lib-go/daemon/localstore`: primary and cache `objstore`s, `Read(id)` (cache, then primary),
      `Write(kind, content) → id` (hash, fsync'd write to primary unless already stored), op ID
      lists per repo, and the complete-heads set.
- [ ] `lib-go/daemon/serviceclient`: `GetObjects` in batches of at most 4 MiB of expected content,
      falling back to `DownloadObject` for `too_large`. Every call is bounded by
      `--service-timeout`.
- [ ] `daemon.proto`:
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

## 6. Daemon uploads, op heads and repo creation

Outcome: written objects reach the service in the background, and an operation is published only
once everything written before it is uploaded.

- [ ] `lib-go/daemon/uploader`:
  - Each written object gets a sequence number. Batches of at most 4 MiB go through `HasObjects`,
    then `PutObjects` for the missing ones. Objects over 1 MiB go through `UploadObject`.
  - Once an upload is confirmed, the object moves from primary to cache.
  - At startup, it re-queues everything in primary.
  - Failures back off exponentially from 1 s to 60 s.
  - `Barrier(ctx)`: captures the current sequence number and waits until every object up to it is
    confirmed. It fails at the first failed attempt for one of those objects, and retries any of
    them that are backing off at once.
- [ ] `OpHeads.GetOpHeads(repo, user)`: forwarded to the service on every call.
- [ ] `OpHeads.UpdateOpHeads(repo, user, old_ids[], new_id)`: `Barrier`, then the service call. The
      barrier failing returns `Unavailable` with the upload error. `InvalidArgument` for the user
      passes through.
- [ ] `Workspace.CreateRepo(repo)`: forwarded. `AlreadyExists` and `InvalidArgument` pass through.

Tests:

- [ ] Integration: written objects reach the service and move to the cache. Objects the service
      already has aren't sent again (`PutObjects` is counted by a wrapper).
- [ ] Integration: a wrapper fails `PutObjects`. `UpdateOpHeads` fails quickly with `Unavailable`,
      the service's op heads are unchanged, and the upload succeeds after the wrapper stops failing.
- [ ] Integration: a wrapper stalls `PutObjects`. `UpdateOpHeads` fails after about
      `--service-timeout`, not after the CLI deadline.
- [ ] Integration: a daemon restarted on the same data dir uploads what was left in primary.
- [ ] Integration: `CreateRepo` passes `AlreadyExists` through.
- [ ] Integration: two users through one daemon publish to the same repo, and each reads back only
      their own op heads.

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

- [ ] `bin-rs/jjw` depends on `gix-object` at jj-lib's version.
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

## 9. CLI store shims, repo creation and defaults

Outcome: `jjw cws repo create` makes a working cloud repo, and jj history commands work in it.

- [ ] `daemon.proto` client RPCs for `Store`, `OpStore`, `OpHeads` and `Workspace.CreateRepo`, and
      matching `DaemonClient` methods. The client accepts 8 MiB messages. gRPC errors map to jj-lib
      errors: `NotFound` to `ObjectNotFound`, `Unavailable` and others to an error carrying the
      daemon's message.
- [ ] `store.rs` registers, as type `cws`:
  - `store/backend.rs`: `Backend` over `Store`, with commit ID length 20 and change ID length 16.
    The root commit and empty tree are synthesized. Signing, copy records, tracked copies and `gc`
    behave as in [Decisions](#objects-and-ids).
  - `store/op_store.rs`: `OpStore` over `OpStore`, reading `repo_name`. The root operation and root
    view are synthesized.
  - `store/op_heads_store.rs`: `OpHeadsStore` over `OpHeads`, with a no-op initializer and the file
    lock. The initializer writes `user_email`, and the loader does the user check.
  - `store/index_store.rs`: the `DefaultIndexStore` wrapper with the bulk commit-graph fetch.
    `collect_unindexed_commit_ids(index_dir, op)` is a separate function.
- [ ] `working_copy.rs`: the `cws-none` working copy and factory.
- [ ] `default_config.rs`: the default config layer.
- [ ] `commands/cws/repo.rs`: `jjw cws repo create <name> [path]`:
  1. Fails with `<path> already has a .jj directory`, or with the unset `user.email` error, before
     calling the daemon.
  2. Calls `Workspace.CreateRepo(name)`. `AlreadyExists` becomes
     `repo <name> already exists on the workspace service`, and `InvalidArgument` passes the name
     rule through.
  3. Runs `Workspace::init_with_factories` with the `cws` factories, and prints the repo name and
     path.

  If step 3 fails, the repo exists on the service with no repo directory. M3's setup for an existing
  repo recovers it. The error says so.

- [ ] `main.rs`: `add_store_factories`, `add_working_copy_factories` and `add_extra_config`. The
      `git init`, `git clone` and `debug init-simple` block stays.
- [ ] Harness: `Service::start` and `Daemon::start` pass temp `--data-dir`s. `Service::restart()`
      restarts on the same data dir and port. `TestEnv` sets jj's deterministic test variables
      (`JJ_RANDOMNESS_SEED`, `JJ_TIMESTAMP`, `JJ_OP_TIMESTAMP`, `JJ_OP_HOSTNAME`, `JJ_OP_USERNAME`)
      and a fixed `user.email`.

Tests:

- [ ] Unit: `collect_unindexed_commit_ids` on a stock local repo in a temp dir, with ops that
      abandon and rewrite commits. It includes the abandoned commit and the rewritten predecessor,
      and stops at an op that has an `op_links` marker.
- [ ] Unit: the mapping from gRPC errors to jj-lib errors.
- [ ] E2E (`e2e/tests/repo.rs`): `repo_lifecycle`. `repo create`, then `jj new`, `describe`, another
      `new`, `rebase`, `log` (snapshot of the default revset and `trunk()`), `op log` and `undo`,
      all with `--ignore-working-copy`.
- [ ] E2E: `concurrent_at_op`. Two `describe`s with the same `--at-op` publish two op heads, and the
      next `jj log` reports the concurrent operations, merges them, and shows both changes.
- [ ] Integration (`e2e/tests/repo_errors.rs`, using the harness):
  - `repo create` with an existing name: the error, and no `.jj` created.
  - `repo create` where `.jj` exists: the error, and the name is still free.
  - `repo create` with an invalid name: the name rule in the error.
  - `repo create` with `user.email` unset: the error, and the name is still free.
  - `jj log` with a different `user.email` than at creation: the mismatch error and hint, and no new
    operation for either email. With `--config user.email=<old>` it works.
  - `jj log` without `--ignore-working-copy`: `this workspace has no working copy` and the hint.
  - `jj describe` with signing configured: the signing error, and no new operation.
  - The service killed, then `jj describe`: fails with the upload error. After `Service::restart()`,
    `op log` shows no new operation and the same `describe` succeeds.

## 10. Spec, roadmap and glossary updates

After the code is in:

- [ ] [DESIGN.md](../spec/DESIGN.md):
  - Object model: operation and view IDs, root objects, the wire form `(id, kind, content)`.
  - Publishing: the barrier and how it fails fast when the service is unreachable.
  - Repository model: repo creation leaves op heads unset, so every user starts at the root
    operation, and repo names. Users and per-user op heads are already in the spec.
- [ ] [workspace-service.md](../spec/workspace-service.md):
  - `--data-dir` and the storage layout behind the `Storage` interface.
  - `CreateRepo`, the `NotFound`, `InvalidArgument` and `FailedPrecondition` cases, user key
    validation and encoding, message sizes and chunking.
  - `GetCommitGraph` exactness with stored generation numbers.
- [ ] [daemon.md](../spec/daemon.md):
  - `--data-dir` and the local storage layout.
  - The uploader, its backoff, and the barrier.
  - The final `Store`, `OpStore`, `OpHeads` and `Workspace.CreateRepo` RPCs, with repo names on
    `OpStore` and prefix resolution.
  - Complete heads in `FetchCommitGraph`, fetch verification, and the op-heads file lock.
  - Remove `GetCopyRecords`. Blob metadata starts in M4.
- [ ] [jj-cli.md](../spec/jj-cli.md):
  - `jjw cws repo create`, and the narrowed repo creation block.
  - Operation and view encoding, the copied conversion code, and the root objects.
  - The `cws-none` working copy.
  - Commit index: replace `FetchCommitGraph(view heads)` with the wrapper's walk over unindexed ops,
    hidden commits included.
  - User identity: the `user_email` file in the repo directory. The check and its error are already
    in the spec.
  - v1 limitations: no signing, no copy records or tracked copies, no `jj util gc`.
- [ ] [GLOSSARY.md](../../GLOSSARY.md): already has **Repo directory**, **Op heads** and
      **Publish**. Check that nothing in the implementation changed their meaning.
- [ ] Mark M2 and this plan done in the [roadmap](mvp-impl-roadmap.md).

## Open questions

None blocking M2.
