# MVP Implementation Roadmap

Technical roadmap to implement [DESIGN.md](../spec/DESIGN.md). This is the source of truth for
milestones and for open questions that must be answered before a milestone is done.

## Strategy

- Very brief and high level description of technical milestones to build out the initial version
  (excluding any future work items).

- Incrementally build capabilities through the stack in each milestone and sub-milestone. e.g.,
  First milestone would be have a basic cli, daemon and service working that can connect and call
  each other but no functionality yet. Sub-milestones to create cli, then daemon and finally
  service. Second milestone could be to build checkout where we add this API functionality through
  each component in the stack from service, then daemon (because daemon depends on service), then
  cli.

- Only describe outcomes in each milestone and observable capalities, e.g., after example milestone
  2 sub-milestone 1, workflow service can serve raw GRPC requests to create workspaces. Detailed
  APIs, technology decisions will be decided when designing each sub-milestone.

- The protos declare every service from M1 on, except the daemon's `Trunk` service, which M3 adds.
  Each RPC is added in the milestone that first needs it.

- Each milestone ends with an end-to-end test that runs the CLI against a real daemon and service.

- There is no eagerly materialized working copy. The FUSE working copy comes in M4 and M5, and the
  milestones before it don't need a working copy at all.

## Milestones

**Status:** M1 done. Re-brand done. M2 planned.

### M1: Skeleton through the stack

**Status:** Done.

The three binaries build, start and call each other, with no repository functionality yet.

1. **Service:** `source-api` serves gRPC health and the empty `Repo`, `Workspace`, `Objects`,
   `OpHeads` and `Trunk` services.
2. **Daemon:** `srcd` serves `Daemon.GetStatus` and the empty `Store`, `OpStore`, `OpHeads`,
   `WorkingCopy`, `Repo`, `Workspace` and `Prefetch` services on a Unix socket. `GetStatus` checks
   the service's health with a bounded timeout.
3. **CLI:** `jjc`, the custom jj binary at a pinned jj version. `jjc cloud status` reports whether
   the daemon and the service are up. Commands that create a repo fail with a clear error.
4. **Test harness:** one command starts a service and a daemon and runs the CLI against them.

See [m1-implementation.md](m1-implementation.md) for detailed implementation plan.

### M2: Shared repository without a working copy

Prerequisite: the [re-brand](re-brand.md) to JJ Cloud Source, done before M2 starts.

jj commands that read and rewrite history work through the stack, with no files checked out.

1. **Service:** creates an empty repo, stores verified objects with blob metadata, serves batch and
   streaming object fetch and upload, and streams the commit graph. It keeps a registry of
   workspaces, named per user and repo, and op heads per workspace with atomic updates. A new
   workspace starts at the root operation, op heads of an unknown workspace are `NotFound`, and
   workspaces never see each other's op heads.
2. **Daemon:** `Store`, `OpStore` and `OpHeads` work, with op heads named by repo, user and
   workspace, and workspace creation is forwarded. Written objects are hashed, persisted and
   uploaded in the background. Reads come from local storage, with a fetch on a miss. The commit
   graph is fetched in bulk. An operation is published only once every object it references is on
   the service.
3. **CLI:** `jjc` registers its `cloud` store factories: backend, op store and op heads shims using
   the pinned jj version's commit encoding. Stock repo creation (`git init`, `git clone`,
   `debug init-simple`) stays blocked. The commit index is built from the bulk-fetched commit graph.
   The default `trunk()` alias is in place. The user is jj's `user.email`. `jjc cloud repo create`
   creates a repo on the service, and `jjc cloud workspace create` registers a named workspace for
   the user and sets up its repo directory in an ordinary directory, with no working copy and `@` on
   the root commit. `jjc cloud workspace list` and `delete` manage the user's workspaces on the
   service. Later commands there fail clearly if `user.email` changes. There, `jj new`, `describe`,
   `rebase`, `log`, `op log` and `undo` work with `--ignore-working-copy`. Stock `jj workspace add`,
   `forget` and `rename` fail. Concurrent operations, run with the same `--at-op`, merge on the next
   command. A second workspace of the same user starts from the root commit and sees none of the
   first workspace's operations or commits.

See [m2-implementation.md](m2-implementation.md) for detailed implementation plan.

### M3: Trunk and push

Several users work in one cloud repo, each in workspaces with private op logs, and share finished
work by pushing it to trunk, which only moves forward. There is still no working copy.

1. **Service:** keeps one trunk per repo, starting at the root commit. Trunk is read in one call and
   moves only by a push of a commit that descends from it, so concurrent pushes from the same trunk
   can't both succeed. Every push is appended to a push log.
2. **Daemon:** forwards trunk reads, and forwards a push only once every object written before it is
   on the service.
3. **CLI:** `jjc cloud workspace create` starts a new workspace's `@` on the current trunk.
   `jjc cloud fetch` brings trunk into the workspace's View as `main@origin`, and `jjc cloud push`
   moves trunk to a commit, refusing conflicted commits and empty descriptions. User A creates a
   repo and a workspace and pushes a commit. User B creates a workspace in the repo, starts on that
   commit, and sees none of A's unpushed work or operations. B's push from a stale trunk is
   rejected, and succeeds after a fetch and rebase. B's `jj undo` never changes A's history, and
   undoing a push doesn't move trunk. A's second workspace sees A's pushed work but not A's unpushed
   work, and `jj undo` in one of A's workspaces never changes the other. Two jj processes in one
   workspace publish concurrent operations that merge on the next command.

### M4: Workspaces with lazy, read-only checkout

Creating a workspace and switching revisions take seconds regardless of repo size, and files are
fetched only when read. The mount is read-only.

1. **Service:** serves subtrees with each blob's size and digest, without the blobs.
2. **Daemon:** creates, opens, lists and deletes workspaces, setting up each workspace's repo
   directory, and takes the workspace's hold on the service when it creates or opens one. It mounts
   each workspace with FUSE, serves `stat` and `readdir` without blob downloads, and fetches file
   contents on first read, then serves them from the cache. Checking out a tree costs O(diff between
   trees). Inodes are stable, and mtimes change only for paths whose content changed.
3. **CLI:** `jjc cloud workspace create` now mounts the workspace at the given path, and
   `jjc cloud workspace open` opens an existing workspace on another machine. The repo directory
   moves to the daemon's location. A registered working copy factory handles checkout and state,
   including conflict materialization and stale working copy detection. On a fresh machine, a user
   creates a workspace for an existing cloud repo and moves between revisions with `jj new` and
   `jj edit`. The user opens the workspace on a second machine and sees its history there, and the
   first machine's next command fails with `workspace <name> is open on <host>`. A first benchmark
   measures workspace creation, checkout time and cached read latency (R1, R4).

Open questions:

- **Cold workspace creation at 100k commits** (before sub-milestone 3). Each workspace builds its
  full commit index when it's created, and jj-lib reads every commit through `Backend::read_commit`,
  one daemon RPC each. Measure creation against R1. Mitigations, in order: raise the backend's
  `concurrency()` so reads pipeline; have `FetchCommitGraph` return the commits to the CLI so the
  build reads them from memory; seed the new workspace's index by hard-linking segments from another
  of the same user's workspaces of the repo on the machine, which makes that workspace's private
  commit IDs resolvable in the new one.

### M5: Local edits and snapshots

The standard jj edit workflow works in a workspace. After this milestone, the MVP has every feature
developers need. The service needs no changes.

1. **Daemon:** writes go to a persisted copy-on-write overlay with dirty-path tracking. Snapshot,
   finish, reset and per-workspace locking work, and untracked and ignored files survive checkouts.
   After a restart, the daemon remounts every workspace with its edits intact (R6).
2. **CLI:** snapshot with ignore rules and conflict-marker parsing, `jj reset` / `recover`, and a
   clear error for sparse patterns. Users edit files and run `jj status`, `diff`, `commit`, `squash`
   and resolve conflicts as usual. Snapshot time scales with changed files (R3), and edits survive
   killing the daemon.

### M6: Builds and CI

Build systems and CI get fast, ephemeral checkouts.

1. **Daemon:** prefetches paths or whole trees in batches on request (R8).
2. **CLI:** CI mode checks out a commit by full ID, or trunk by name, without loading an op log,
   View or index, and never publishes. A command exposes prefetch hints to builds. A CI job gets a
   checkout of trunk or a given commit from a cold machine and builds it.

### M7: Production readiness

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

- **Kernel versions on target machines** (before sub-milestone 3). FUSE passthrough for materialized
  files needs Linux 6.9+. Confirm the target machines run it.
- **Op-log fetch at scale** (before sub-milestone 3). In M2, reading the op log costs one round trip
  per operation or View missing from the cache, so opening a workspace on a fresh machine walks the
  workspace's whole op log one object at a time. Options: a bulk op-log fetch RPC, or the service
  walking operation parents.
- **Loading the target codebase** (before sub-milestone 3). The design has no bulk import.
  Committing the tree through a workspace may be enough.
