# M2 Implementation Plan: Shared repository without a working copy

Detailed plan for [M2](mvp-impl-roadmap.md#m2-shared-repository-without-a-working-copy). jj commands
that read and rewrite history work through the stack, with no files checked out. See
[DESIGN.md](../spec/DESIGN.md) for the overall design and
[m1-implementation.md](m1-implementation.md) for the skeleton this builds on.

**Status:** In progress.

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
  objects, uploads them in the background, caches reads, and publishes an operation only once every
  object written before it is on the service.

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
- **Commit index:** jj-lib's stock `DefaultIndexStore`, reading each commit through
  `Backend::read_commit`. v1 histories are O(100s) of commits, so there's no bulk commit-graph fetch
  (see [commit-index.md](../spec/commit-index.md)).

### Sizes

- Every RPC message carries at most 4 MiB of object content, whether a batch (`GetObjects`,
  `PutObjects`) or a stream chunk. The service, the daemon and the CLI's client all accept messages
  up to 8 MiB, which leaves room for IDs and framing.
- Any object over 4 MiB, of any kind, goes through `DownloadObject` and `UploadObject` in 4 MiB
  chunks.
- `PutObjects` and `GetObjects` reject a batch over the limit with `InvalidArgument`. Callers learn
  sizes from `GetObjectsMeta`, split a batched read to fit, and fetch any object over 4 MiB with
  `DownloadObject`.
- The daemon's `ReadFile` and `WriteFile` stream to and from the CLI in 4 MiB chunks. Other `Store`
  and `OpStore` RPCs are unary.
- Streams go through memory one chunk at a time: `UploadObject` and `DownloadObject` read and write
  the object store as chunks move, so object size doesn't bound a stream's memory.

### Storage on disk

- **Split:** object content lives in content-addressed files. All other state lives in one SQLite
  database per binary, `<data-dir>/meta.db`, opened through `database/sql` with `modernc.org/sqlite`
  (pure Go, no cgo).
- **Object layout** (service and daemon): `objects/<2 hex>/<38 hex>`, stored as the Git header plus
  content, uncompressed. Writes go to a temp file, fsync, rename.
- **SQLite settings:** WAL, `synchronous=NORMAL` so a publish that returned is durable to crashes,
  `foreign_keys=ON`, a `busy_timeout`, and every write in a `BEGIN IMMEDIATE` transaction. One
  process per data dir.
- **Schema:** no migrations in the MVP. A binary creates its schema in an empty database and records
  its schema version in `PRAGMA user_version`. If an existing database has another version, it
  refuses to start with
  `schema version mismatch: <data-dir> has schema v<n>, this <binary> needs v<m>; delete <data-dir> to start over`.
  A database with tables but no version is refused the same way, as v0. It never wipes data itself,
  because the daemon's data dir holds objects not yet uploaded.
- **Service:** `--data-dir` is required. Object content goes in `<data-dir>/objects/`, and `meta.db`
  has these tables:
  - `objects(id PK, kind, size, sha256, uploaded_at)`, with `sha256` set for blobs only.
  - `repos(id INTEGER PK, name UNIQUE, created_at)`.
  - `workspaces(id INTEGER PK, repo_id, user_email, name, created_at)`, unique on
    `(repo_id, user_email, name)`. Users have no table: the user is the opaque `user_email` column.
  - `op_heads(workspace_id, op_id)`, keyed on both, deleted with its workspace by cascade.
  - Repos and workspaces have integer keys so they can be renamed later without rewriting other
    tables. Names are what the API uses.
- **Writing objects on the service:** each object's file is written first, then its `objects` row,
  with a batch's rows in one transaction. `UploadObject` writes its temp file as chunks arrive,
  hashing as it goes, and renames it once the ID matches. The row is what makes an object stored:
  `GetObjectsMeta`, `GetObjects` and `UpdateOpHeads` read the table. A file with no row, left by a
  crash between the two, counts as missing and the next upload overwrites it.
- **Op heads on the service:** `UpdateOpHeads` checks that `new_id` has an `operation` row, deletes
  `old_ids` and inserts `new_id` in one transaction. Workspace creation and deletion are each one
  transaction too.
- **Packages on the service:** `metastore.Store` holds everything in `meta.db` and never sees object
  content. `objstore.Store` holds the content. The RPC handlers coordinate the two: they verify IDs,
  compute blob SHA-256, keep the file-before-row order and check files against rows. `objects.go`
  holds that logic in private methods for now, and M3's `PushTrunk` is what lifts it out, since it
  also reads objects. `metastore.Store` and `objstore.Store` are concrete types. Tests run them on a
  temp data dir, with no in-memory fake. How the service stores metadata in production is an M7 open
  question, and `metastore.Store` is where that implementation changes.
- **Missing objects:** an object with no row is `NotFound`. A row whose file is missing or disagrees
  with it is corruption, and reads fail with `Internal`.
- **Daemon:** `--data-dir` defaults to `$XDG_STATE_HOME/jjc/srcd`, or `~/.local/state/jjc/srcd` if
  that's unset.
  - `primary/objects/`: written objects not yet confirmed on the service. An object moves to the
    cache by rename once its upload is confirmed. At startup, the uploader re-queues whatever is
    left. Where an object is records its upload state, and sequence numbers live only in memory, so
    `meta.db` has no object table.
  - `cache/objects/`: confirmed or fetched objects. No eviction until M7.
  - `meta.db` holds `op_ids(repo, op_id)`, keyed on both, with `op_id` as lowercase hex text. Prefix
    resolution is a range query on it. M4 and M5 add the workspace, mount and file metadata.
  - The daemon re-hashes fetched objects before caching them. It computes no blob metadata until M4.

### CLI

- **Repo directory in M2** is the `path` given to `jjc cloud workspace create`, `./<name>` by
  default as `jj git clone` names its destination. With no working copy yet, it's where the user
  runs `jj`, so it isn't in a daemon-managed location; M4 moves it there. Each workspace has its own
  repo directory and commit index. Its `.jj/repo` has `store` (`cloud`), `workspace_store` (stock
  `simple`), `op_store` and `op_heads` (`cloud`, each holding a `repo_name` file), `index` (stock
  `default`) and `submodule_store` (stock). `op_heads` also holds `user_email` and `workspace_name`
  files, written by its initializer.
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
lib-go/obj/                       object kinds, IDs and hashing, blob metadata
lib-go/objstore/                  on-disk content-addressed object directory (service and daemon)
lib-go/metadb/                    opens meta.db with its settings and creates or checks the schema
lib-go/sourceapi/metastore/       Store over meta.db: object rows, repos, workspaces, op heads
lib-go/sourceapi/                 RPC handlers: objects.go, repos.go, workspaces.go
lib-go/srcd/localstore/           primary store, cache, op IDs in meta.db
lib-go/srcd/uploader/             background uploader and publish barrier
lib-go/srcd/sourceapiclient/      batched and chunked object calls, op heads
lib-go/srcd/                      handlers: store.go, opstore.go, opheads.go, workspace.go
bin-rs/jjc/src/store.rs           StoreFactories for the cloud backend, op store, op heads
bin-rs/jjc/src/store/backend.rs
bin-rs/jjc/src/store/git_encoding.rs     commit and tree encoding, copied from git_backend.rs
bin-rs/jjc/src/store/op_store.rs
bin-rs/jjc/src/store/op_proto.rs         op and view conversion, copied from simple_op_store.rs
bin-rs/jjc/src/store/op_heads_store.rs
bin-rs/jjc/src/working_copy.rs    cloud-none working copy and factory
bin-rs/jjc/src/default_config.rs  default config layer
bin-rs/jjc/src/commands/cloud/repo.rs      jjc cloud repo create
bin-rs/jjc/src/commands/cloud/workspace.rs jjc cloud workspace create, list, delete
e2e/tests/repo.rs                 happy path
e2e/tests/repo_errors.rs          CLI unhappy paths against the real daemon and service
```

## 1. Go object and metadata packages (done)

Outcome: one Go implementation of object IDs and parsing, and of opening `meta.db`, shared by the
service and the daemon.

- [x] `lib-go/obj`:
  - `type Kind` (`Commit`, `Tree`, `Blob`, `Operation`, `View`), `String()` giving the header word,
    and conversion to and from the proto `ObjectKind`, which this section adds to
    `source_api.proto`.
  - `type ID [20]byte` with `Hex()`, `ParseHex` and `RootID` (all zeros).
  - `Hash(kind, content) (ID, error)`: sha1dc (`github.com/pjbgf/sha1cd`) over the header plus
    content. Returns `ErrCollision` when a collision attack is detected.
  - `BlobMeta(content) (size uint64, sha256 [32]byte)`: the size and SHA-256 of a blob's content
    (the file bytes, without the header), which the service stores in the `objects` table.
- [x] `lib-go/objstore`: `Open(dir)`, `Put(id, kind, content)` (atomic, idempotent),
      `Get(id) (kind, content, error)`, `Has(id)`, `Size(id)`, `Remove(id)`, `MoveTo(other, id)`
      (rename into another directory on the same filesystem) and `List()`. It doesn't hash; callers
      verify.
- [x] `lib-go/metadb`: `Open(path, binary, version, schema) (*sql.DB, error)` applies the
      [SQLite settings](#storage-on-disk), creates the schema in an empty database and sets
      `user_version`, and fails with `ErrSchemaVersionMismatch` on a mismatch. Adds the
      `modernc.org/sqlite` dependency, pinned to v1.50.0 because later versions need Go 1.26.

Tests:

- [x] Unit: `Hash` matches Git for the empty blob (`e69de29…`), the empty tree (`4b825dc…`) and a
      sample commit. The collision-detecting hash reports the SHAttered PDF (its first 1 KiB) as an
      attack. Hashed as a blob it isn't one, because the Git header shifts the collision blocks, so
      the test checks `Hash` gives Git's ID for it.
- [x] Unit: `objstore` object lifecycle for each kind, idempotent `Put`, `MoveTo`, `List`, and no
      partial file left when a write fails.
- [x] Unit: `metadb.Open` creates the schema once, reopens it, and fails with
      `metadb.ErrSchemaVersionMismatch` when the version differs or a non-empty database has none.

## 2. Service object storage (done)

Outcome: the service stores verified objects with blob metadata and serves them in batches and
streams.

- [x] `source-api --data-dir <dir>` (required).
- [x] `lib-go/sourceapi/metastore`: `Store` over `meta.db`, opened with `metastore.Open(path)`, with
      `GetObjectsMeta` and `InsertObjects` (rows `{id, kind, size, sha256}` in one transaction,
      keeping an existing row's upload time); section 4 adds the repo methods. Schema v1 already has
      all four tables, so section 4 needs no version bump.
- [x] `lib-go/sourceapi/objects.go`: the service holds a `*metastore.Store` and an
      `*objstore.Store`, which `source-api` opens and passes to `NewServer`. `storeObjects` verifies
      a batch, writes the files of objects with no row, then inserts their rows. `readObject` and
      `readFile` check each file against its row (see [Storage on disk](#storage-on-disk)).
- [x] `source_api.proto`: `ObjectKind`, `Object { bytes id; ObjectKind kind; bytes content; }`, and
      these RPCs on `Objects`:
  - `PutObjects(objects[])`: re-hashes each object and rejects the whole batch with
    `InvalidArgument` naming the first ID that doesn't match its content. Rejects a batch over 4 MiB
    of content. Writes blob metadata and upload time.
  - `UploadObject(stream: header {id, kind, size}, then chunks)`: same verification. Rejects a size
    mismatch and a chunk over 4 MiB.
  - `GetObjectsMeta(ids[]) → objects[]`: one `ObjectMeta { id, present, size }` per ID, in request
    order.
  - `GetObjects(ids[]) → objects[]`: the objects in request order, or an overall error: `NotFound`
    naming the first unknown ID, or `InvalidArgument` if they're over 4 MiB of content in all.
  - `DownloadObject(id) → stream`: first message `{kind, size}`, then 4 MiB chunks. `NotFound` for
    an unknown ID.
- [x] Servers accept 8 MiB messages.
- [x] Harness: `SourceApi::start` passes a temp `--data-dir`, which is now required. `restart()` is
      still section 8's.

Tests:

- [x] Integration (in-process gRPC server on a temp data dir): put then get each kind. A content/ID
      mismatch is rejected and nothing from the batch is stored. `GetObjectsMeta` reports presence
      and size per ID in request order, and `GetObjects` of an unknown ID is `NotFound`. Blob
      metadata is stored.
- [x] Integration: a 9 MiB blob uploads in chunks, `GetObjectsMeta` reports its size, `GetObjects`
      rejects it, and `DownloadObject` returns the same bytes. A size mismatch and a chunk over 4
      MiB in `UploadObject` are rejected.
- [x] Integration: a `PutObjects` batch over 4 MiB is rejected and stores nothing, and a 4 MiB
      object fits. `GetObjects` of two 3 MiB blobs is rejected with `InvalidArgument`, and a batch
      of exactly 4 MiB is returned.
- [x] Integration: objects survive a server restart on the same data dir.
- [x] Integration: an object file with no `objects` row is reported missing by `GetObjectsMeta`,
      `NotFound` from `GetObjects`, and a later upload of it succeeds.
- [x] Integration: a row whose file is gone is `Internal` from `GetObjects` and `DownloadObject`.

## 3. Streamed object storage

Outcome: `UploadObject` and `DownloadObject` move an object between the network and disk a chunk at
a time, so a stream holds one chunk in memory whatever the object's size. Section 2's handlers load
the whole object, which defeats the point of streaming.

- [ ] `lib-go/obj`: `NewHasher(kind, size)`, an `io.Writer` over the content, with
      `Sum() (Digest, error)` returning `ErrCollision` as `Hash` does. `Digest` is `{ID, SHA256}`,
      with the SHA-256 set for blobs. `Hash` is built on it and still returns just the ID.
      `BlobMeta` goes.
- [ ] `lib-go/objstore`:
  - `Create(kind, size) (*Writer, error)` starts a temp file with the header. `Writer` has `Write`,
    `Commit(id)` (fails if the bytes written aren't `size`, then fsync and rename, as `Put` does)
    and `Abort()` (removes the temp file). `Put` is built on it.
  - `Open(id) (kind, size, io.ReadCloser, error)` reads the content without loading it.
- [ ] `UploadObject` writes each chunk to an `objstore.Writer` and an `obj.NewHasher` as it arrives,
      and aborts on any error, including the client going away. It then verifies the ID, calls
      `Commit(id)` and calls `InsertObjects`. It doesn't check whether the object is already stored:
      the file is replaced with verified bytes and the row keeps its upload time.
- [ ] `DownloadObject` calls `GetObjectsMeta`, then `objstore.Open`, checks kind and size against
      the row, and reads into one 4 MiB buffer.
- [ ] `PutObjects` and `GetObjects` stay in memory: they serve at most 4 MiB.

Tests:

- [ ] Unit: `NewHasher` matches `Hash` with content split at various points, and reports the
      collision `Hash` reports.
- [ ] Unit: a `Writer` commit gives the same file as `Put`. A size mismatch and `Abort` leave no
      object and no temp file.
- [ ] Integration: while a 64 MiB blob uploads, the temp file holds the chunks received so far, and
      the blob then downloads with the same bytes.
- [ ] Integration: a client that cancels mid-upload leaves no temp file, no row and no object file.
      A tampered upload is rejected the same way.

## 4. Service repos, workspaces and op heads

Outcome: the service creates repos, keeps a registry of workspaces, and keeps each workspace's op
heads with atomic updates. These are `metastore.Store` methods, since none of them touch object
content; each transaction, including `UpdateOpHeads`'s operation row check, is one method.

- [ ] `Repo.CreateRepo(repo)`: validates the name (`InvalidArgument`), inserts the repo row,
      `AlreadyExists` if the repo exists.
- [ ] `Workspace.CreateWorkspace(repo, user, name)`: `NotFound` for an unknown repo,
      `InvalidArgument` for an invalid user or name, `AlreadyExists` if the user has the workspace.
      Inserts the workspace row and op heads `{root op ID}` in one transaction.
- [ ] `Workspace.ListWorkspaces(repo, user) → names[]`: `NotFound` for an unknown repo, sorted
      names, empty for a user with none.
- [ ] `Workspace.DeleteWorkspace(repo, user, name)`: `NotFound` for an unknown repo or workspace.
      Deletes the workspace row, and its op heads with it.
- [ ] `OpHeads.GetOpHeads(repo, user, workspace) → op_ids[]`: `NotFound` for an unknown repo or
      workspace, `InvalidArgument` for an invalid user.
- [ ] `OpHeads.UpdateOpHeads(repo, user, workspace, old_ids[], new_id)`: `NotFound` for an unknown
      repo or workspace, `InvalidArgument` for an invalid user, `FailedPrecondition` if `new_id`
      isn't stored as an operation. Removes `old_ids` from the workspace's op heads (ignoring ones
      already gone) and adds `new_id`, in one transaction with the check. The holder token comes in
      M4.

Tests:

- [ ] Integration: create repo, create workspace, get (`{root}`), update, get. `AlreadyExists` on a
      second create of either, and `InvalidArgument` for repo and workspace names `""`, `.x`, `a/b`
      and a 101-character name.
- [ ] Integration: two workspaces of one user and one workspace of another user with the same name
      as one of them. Updates in one never change the others' op heads, and each new one reads
      `{root}`. `InvalidArgument` for an empty user and one with a newline. An email with `/`, `%`
      and `'` works as a user.
- [ ] Integration: list returns only the user's workspaces. After delete, list omits it, both
      op-heads RPCs return `NotFound`, and the name can be created again starting at `{root}`.
- [ ] Integration: `NotFound` from the workspace and op-heads RPCs for an unknown repo and an
      unknown workspace, and `FailedPrecondition` for an op that isn't stored.
- [ ] Integration: 20 parallel clients each publish an op to one workspace from the same old head.
      All 20 end up as its heads, and the root is gone.
- [ ] Integration: the registry and op heads survive a restart.

## 5. Daemon local store and reads and writes

Outcome: the daemon serves `Store` and `OpStore` from local storage, with a fetch on a miss.

- [ ] `srcd --data-dir` (default `$XDG_STATE_HOME/jjc/srcd`, then `~/.local/state/jjc/srcd`).
- [ ] `lib-go/srcd/localstore`: primary and cache `objstore`s, `Read(id)` (cache, then primary),
      `Write(kind, content) → id` (hash, fsync'd write to primary unless already stored), and the
      `op_ids` table in `meta.db`.
- [ ] `lib-go/srcd/sourceapiclient`: a read of one ID calls `DownloadObject` directly, one round
      trip at any size. A read of several IDs calls `GetObjectsMeta`, splits the objects up to 4 MiB
      into `GetObjects` batches of at most 4 MiB, and calls `DownloadObject` for the rest.
      `DownloadObject` streams into the local store through `objstore.Writer` rather than into
      memory. Every call is bounded by `--source-api-timeout`.
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
- [ ] `ReadFile` and `WriteFile` go through `objstore.Open` and `objstore.Writer`, holding one chunk
      in memory.
- [ ] Writes are queued for upload once section 6 exists. Until then they stay in primary.

Tests:

- [ ] Integration (real in-process service): a write then a read hits local storage with the service
      stopped. A read miss fetches from the service and then works with the service stopped.
- [ ] Integration: a 9 MiB blob is read through `DownloadObject` and streamed to the client in
      chunks, and `WriteFile` accepts it streamed.
- [ ] Integration: a service wrapper that returns corrupted content: the read fails with `DataLoss`
      and nothing is cached. A kind mismatch is rejected.
- [ ] Integration: prefix resolution finds a unique op, and reports ambiguous and unknown prefixes.
      Op IDs are kept per repo and survive a daemon restart.

## 6. Daemon uploads, op heads, repos and workspaces

Outcome: written objects reach the service in the background, and an operation is published only
once everything written before it is uploaded.

- [ ] `lib-go/srcd/uploader`:
  - Each written object gets a sequence number. Batches of at most 4 MiB go through
    `GetObjectsMeta`, then `PutObjects` for the missing ones. Objects over 4 MiB go through
    `UploadObject`, read from `objstore.Open` a chunk at a time.
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

## 7. CLI encoding

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

## 8. CLI store shims, repo and workspace commands, and defaults

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
- [ ] Harness: `Daemon::start` passes a temp `--data-dir` (`SourceApi::start` does since section 2).
      `SourceApi::restart()` restarts on the same data dir and port. `TestEnv` sets jj's
      deterministic test variables (`JJ_RANDOMNESS_SEED`, `JJ_TIMESTAMP`, `JJ_OP_TIMESTAMP`,
      `JJ_OP_HOSTNAME`, `JJ_OP_USERNAME`) and a fixed `user.email`.

Tests:

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

## 9. Spec, roadmap and glossary updates

After the code is in:

- [ ] [DESIGN.md](../spec/DESIGN.md):
  - Object model: operation and view IDs, root objects, the wire form `(id, kind, content)`.
  - Publishing: the barrier and how it fails fast when the service is unreachable.
  - Repository model: repo names. Workspaces, the registry and per-workspace op heads are already in
    the spec ([ADR 0001](../adr/0001-op-log-per-workspace.md)).
- [ ] [source-api.md](../spec/source-api.md):
  - `--data-dir`, the object files and `meta.db` tables, and the schema version check.
  - `CreateRepo`, the `NotFound`, `InvalidArgument` and `FailedPrecondition` cases, user key
    validation and encoding, message sizes and chunking. The workspace RPCs are already in the spec;
    check their errors match.
- [ ] [srcd.md](../spec/srcd.md):
  - `--data-dir`, the local storage layout, `meta.db` and the schema version check.
  - The uploader, its backoff, and the barrier.
  - The final `Store`, `OpStore`, `OpHeads` and `Workspace` RPCs, with repo names on `OpStore` and
    prefix resolution.
  - Fetch verification and the op-heads file lock.
  - Remove `GetCopyRecords`. Blob metadata starts in M4.
- [ ] [jjc.md](../spec/jjc.md):
  - The narrowed repo creation block. `repo create` and the `workspace` commands are already in the
    spec; check their errors match.
  - Operation and view encoding, the copied conversion code, and the root objects.
  - The `cloud-none` working copy.
  - User identity: the `user_email` and `workspace_name` files in the repo directory. The check and
    its error are already in the spec.
  - v1 limitations: no signing, no copy records or tracked copies, no `jj util gc`.
- [ ] [GLOSSARY.md](../../GLOSSARY.md): already has **Workspace**, **Workspace name**, **Repo
      directory**, **Op heads** and **Publish**. Check that nothing in the implementation changed
      their meaning.
- [ ] Mark M2 and this plan done in the [roadmap](mvp-impl-roadmap.md).

## Open questions

None blocking M2.
