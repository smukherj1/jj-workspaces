# MVP Implementation Roadmap

Technical roadmap to implement [DESIGN.md](../spec/DESIGN.md). This is the source of truth for
milestones and for open questions that must be answered before a milestone is done.

## Strategy

- Very brief and high level description of technical milestones to build out the
  initial version (excluding any future work items).

- Incrementally build capabilities through the stack in each milestone and sub-milestone.
  e.g., First milestone would be have a basic cli, daemon and service working that can
  connect and call each other but no functionality yet. Sub-milestones to create cli,
  then daemon and finally service. Second milestone could be to build checkout where
  we add this API functionality through each component in the stack from service, then
  daemon (because daemon depends on service), then cli.

- Only describe outcomes in each milestone and observable capalities, e.g., after example milestone 2
  sub-milestone 1, workflow service can serve raw GRPC requests to create workspaces. Detailed APIs,
  technology decisions will be decided when designing each sub-milestone.

- Each milestone ends with an end-to-end test that runs the CLI against a real daemon and service.

- There is no eagerly materialized working copy. The FUSE working copy comes in M3 and M4, and the
  milestones before it don't need a working copy at all.

## Milestones

**Status:** Not started.

### M1: Skeleton through the stack

The three binaries build, start and call each other, with no repository functionality yet.

1. **Service:** the workspace service starts and serves its gRPC API. Every RPC returns
   "unimplemented".
2. **Daemon:** the daemon serves its gRPC API on a Unix socket with stub RPCs, and calls the service
   with bounded timeouts.
3. **CLI:** the custom jj binary (pinned jj version) registers its store and working copy factories.
   jj commands reach the daemon and fail with a clear "not implemented" error.
4. **Test harness:** one command starts a service and a daemon and runs the CLI against them.

### M2: Shared repository without a working copy

jj commands that read and rewrite history work through the stack, with no files checked out.

1. **Service:** creates an empty repo whose only op head is the root operation, stores verified
   objects with blob metadata, serves batch and streaming object fetch and upload, streams the
   commit graph, and keeps op heads with atomic updates.
2. **Daemon:** `Store`, `OpStore` and `OpHeads` work. Written objects are hashed, persisted and
   uploaded in the background. Reads come from local storage, with a fetch on a miss. The commit
   graph is fetched in bulk. An operation is published only once every object it references is on
   the service.
3. **CLI:** backend, op store and op heads shims using the pinned jj version's commit encoding. The
   commit index is built from the bulk-fetched commit graph. The default `jj log` revset is in
   place. A command creates a repo on the service. A development-only command sets up a local repo
   directory for an existing repo in an ordinary directory, with no working copy; M3's workspace
   creation replaces it. There, `jj new`, `describe`, `rebase`, `log`, `op log` and `undo` work
   with `--ignore-working-copy`. A second machine sees the same history, and concurrent operations
   from two machines merge on the next command.

Open questions:

- **Repo creation** (before sub-milestone 1). How the service creates a new, empty repo and which
  command calls it. Setting up a machine for an existing repo must not add the root operation as an
  extra op head, as stock jj's repo init does.
- **Operation and view encoding** (before sub-milestone 1). How the CLI serializes operations and
  views (e.g. stock jj's protobuf op-store format), how their IDs fit the Git-compatible scheme, and
  therefore how the daemon hashes them and the service verifies them on upload.

### M3: Workspaces with lazy, read-only checkout

Creating a workspace and switching revisions take seconds regardless of repo size, and files are
fetched only when read. The mount is read-only.

1. **Service:** serves subtrees with each blob's size and digest, without the blobs.
2. **Daemon:** creates, lists and deletes workspaces, setting up the machine's repo directory on first
   use. It mounts each workspace with FUSE, serves `stat` and `readdir` without blob downloads, and
   fetches file contents on first read, then serves them from the cache. Checking out a tree costs
   O(diff between trees). Inodes are stable, and mtimes change only for paths whose content changed.
3. **CLI:** a workspace creation command, plus working copy checkout and state, including conflict
   materialization and stale working copy detection. On a fresh machine, a user creates a workspace
   and moves between revisions with `jj new` and `jj edit`. A first benchmark measures checkout time
   and cached read latency (R1, R4).

Open questions:

- **Workspace creation command** (before sub-milestone 3). The command name and flags for creating
  a workspace through `Workspace.Create`, including the first workspace on a machine, where no repo
  exists locally yet.

### M4: Local edits and snapshots

The standard jj edit workflow works in a workspace. After this milestone, the MVP has every feature
developers need. The service needs no changes.

1. **Daemon:** writes go to a persisted copy-on-write overlay with dirty-path tracking. Snapshot,
   finish, reset and per-workspace locking work, and untracked and ignored files survive checkouts.
   After a restart, the daemon remounts every workspace with its edits intact (R6).
2. **CLI:** snapshot with ignore rules and conflict-marker parsing, `jj reset` / `recover`, and a
   clear error for sparse patterns. Users edit files and run `jj status`, `diff`, `commit`, `squash`
   and resolve conflicts as usual. Snapshot time scales with changed files (R3), and edits survive
   killing the daemon.

### M5: Builds and CI

Build systems and CI get fast, ephemeral checkouts.

1. **Daemon:** prefetches paths or whole trees in batches on request (R8).
2. **CLI:** CI mode checks out a commit by full ID without loading the op log, View or index, and
   never publishes. A command exposes prefetch hints to builds. A CI job gets a checkout at a given
   commit from a cold machine and builds it.

### M6: Production readiness

The system runs unattended against the target codebase and meets the PRD targets.

1. **Service:** deployed on durable storage, with metrics.
2. **Daemon:** cache size limits with LRU eviction that never touches the primary store, cleanup of
   objects left by failed publishes, clear bounded-timeout errors on every service call (R9),
   metrics for cache hit rate, fetch latency and bytes fetched (R10), and a systemd unit with the
   upgrade procedure.
3. **Validation:** the target codebase is loaded into the service, and benchmarks show it meets the
   PRD requirements (R1 to R4) and success metrics, with FUSE kernel caching and passthrough tuned
   as needed.

Open questions:

- **Kernel versions on target machines** (before sub-milestone 3). FUSE passthrough for
  materialized files needs Linux 6.9+. Confirm the target machines run it.
- **Loading the target codebase** (before sub-milestone 3). The design has no bulk import.
  Committing the tree through a workspace may be enough.
