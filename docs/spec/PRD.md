# PRD: Virtualized Workspaces for jj

**Status:** Draft · **Date:** 2026-10-21

## 1. Problem

Developers and build systems working in a large source repository have to clone and check out the whole repository before they can do anything. On large codebases this takes minutes to hours, uses a lot of disk, and is mostly wasted: a typical task or build touches only a small fraction of the files.

## 2. Goals

1. **Instant checkout:** creating a workspace or switching revisions finishes in seconds, regardless of repository size.
2. **Lazy loading:** file contents are fetched only when a developer, tool, or build reads them, and then cached locally.
3. **Native jj experience:** standard jj commands (`log`, `new`, `edit`, `diff`, `status`, `rebase`, `describe`, etc.) work without special workflows.
4. **Build-friendly:** builds see a normal filesystem, and reads that hit the cache are close to local-disk speed.

## 3. Non-Goals (v1)

- Full offline support. Uncached content needs the remote service to be reachable.
- Interoperating with Git remotes (`jj git push/fetch`) for the virtualized repository.
- Windows and macOS support. Linux (FUSE) comes first; see Future Work.
- Replacing existing code review systems.

## 4. Users

- **Developers** who need fast access to a large codebase and edit a small part of it.
- **CI and build systems** that need many short-lived checkouts at specific revisions.
- **Tooling and IDE integrations** that read the workspace as a normal directory tree.

## 5. Architecture Overview

```
 ┌──────────────┐   jj Backend / WorkingCopy APIs   ┌────────────────────┐        ┌──────────────────┐
 │   jj CLI     │ ────────────── IPC ─────────────▶ │   Local Daemon     │ ─RPC─▶ │  Remote Service  │
 │ (custom      │                                   │ - object cache     │        │ - object store   │
 │  backend +   │ ◀──── VFS mount (FUSE) ─────────▶ │ - VFS (FUSE)       │        │ - commit graph   │
 │  working     │                                   │ - prefetch         │        │ - op log + heads │
 │  copy)       │                                   │ - overlay of edits │        │                  │
 └──────────────┘                                   └────────────────────┘        └──────────────────┘
```

### 5.1 jj custom backend (in-process, Rust)

- Implements jj-lib's `Backend` trait (commits, trees, files, symlinks, copy tracking) by calling the local daemon over IPC (for example gRPC over a Unix socket).
- Implements a custom `WorkingCopy` / `LockedWorkingCopy` that does **not** write files to disk. A checkout tells the daemon to point the VFS at a root tree ID, and a snapshot asks the daemon for the set of modified paths instead of scanning the tree.
- Registered through jj's store factories. Creating a workspace goes through the local daemon, which sets up the machine's repo directory on first use and mounts the workspace; no local clone is made.

### 5.2 Local daemon

- **Cache plus local state:** the daemon caches data from the remote service and gives jj a common API for talking to it. Everything shared lives on the remote. The only data held solely by the daemon is the overlay of edits not yet snapshotted, per-workspace working-copy state, and objects not yet uploaded; cache eviction never touches it.
- **Content-addressed cache** for trees and file blobs on local disk, with size limits and LRU eviction. It is shared across all workspaces on the machine.
- **VFS layer:** serves a read-only view of the checked-out tree with a copy-on-write overlay for local edits. It tracks dirty paths so that jj snapshots cost O(changed files), not O(repo size).
- **Lazy fetch:** directory listings and file metadata are served from tree objects, and blob contents are fetched from the remote on first `read()`.
- **Prefetch:** batched and predictive fetching (for example by directory, by build target, or from historical access patterns) to avoid round-trip storms.
- **Write path:** uploads new objects (commits, trees, blobs) to the remote in the background as they are written. Publishing an operation (updating op heads) waits until every object it references is on the remote.

### 5.3 Remote service

- Source of truth and durable storage for all repository data: commits, trees, file contents, the operation log, view state, and op heads.
- Provides batch fetch APIs, tree and metadata queries (so the client can list a directory without downloading blobs), and ideally server-side diff and history queries for large ranges.
- Sharing between machines goes through the remote service. Because the operation log and view state live here, a user can pick up the same repository state from any machine.

### 5.4 Object IDs

- Objects use Git-compatible object IDs. We will revisit this only if an alternative offers significant advantages for our needs.

## 6. Key Requirements

| ID  | Requirement                                                                                                           | Priority |
| --- | --------------------------------------------------------------------------------------------------------------------- | -------- |
| R1  | `jj workspace add` / checkout completes in < 5 s; cost scales with the diff between trees, not the repo size          | P0       |
| R2  | File contents are downloaded only on first access, and stat/readdir need no blob downloads                            | P0       |
| R3  | `jj status` / snapshot time scales with modified files plus ignored files in the mount, not the total file count      | P0       |
| R4  | Cached reads within ~1.5× of native local-disk latency                                                                | P0       |
| R5  | Correct jj semantics: commits, rewrites, conflicts, and the operation log behave the same as with the default backend | P0       |
| R6  | Daemon crash or restart does not lose uncommitted local edits                                                         | P0       |
| R7  | Local cache is shared across workspaces and deduplicated by content hash                                              | P1       |
| R8  | Prefetch API/hints for builds (for example "prefetch these paths/targets")                                            | P1       |
| R9  | Graceful degradation: clear errors (not hangs) when the remote is unreachable, with bounded timeouts                  | P1       |
| R10 | Observability: metrics for cache hit rate, fetch latency, and bytes fetched per workspace                             | P1       |

## 7. Success Metrics

- p50 workspace creation < 2 s and p95 < 5 s on the target codebase.
- More than 90% reduction in bytes downloaded per checkout compared with full clones.
- Build times on a cold cache no worse than 20% slower than on a full local checkout, and equal on a warm cache.
- Developer adoption and satisfaction (survey), measured against the current full-clone workflow.

## 8. Risks

- **Tool behavior:** tools that walk the whole tree (grep, IDE indexers, `find`) will trigger mass fetches. Mitigations: sparse/scoped views, fetch throttling, and server-side search.
- **FUSE performance and complexity:** kernel round-trips and page-cache behavior. Mitigations: kernel caching options, passthrough for materialized files (FUSE passthrough needs Linux 6.9+), and benchmarks done early.
- **jj API stability:** the `Backend` and `WorkingCopy` traits change between jj releases, so we need to pin a version and track upstream.
- **Consistency:** races between VFS edits, snapshots, and checkouts. Defining the locking semantics is essential.

## 9. Future Work

- Server-side diff and history queries for large commit ranges.
- Windows and macOS support, using platform alternatives to FUSE (for example ProjFS on Windows and FSKit on macOS).
- Server side authentication, authorization (ACLs), and replication.
- Server-side commit index for revset evaluation (`jj log`) on large histories. In v1, jj-lib builds its index locally from the commit graph.
