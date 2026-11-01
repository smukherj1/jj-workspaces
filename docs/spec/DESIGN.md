# JJ Cloud Source Design

See [PRD.md](PRD.md) for the product requirements. This document covers the decisions that span
components and the contracts between them. Each component's API, behavior and internals are in its
own document.

A _workspace_ is a named jj workspace of a cloud repo. Unlike a stock jj workspace, it has its own
operation log, and the daemon serves its working copy on demand instead of writing it to disk. A
_cloud repo_ is a jj repo whose objects, workspaces' operation logs and trunk live on the _source
API_. See the [glossary](../../GLOSSARY.md).

## Components

- [jjc.md](jjc.md): `jjc` (Rust), a custom jj binary using jj-lib to use the daemon as the storage
  and working copy backend.
- [srcd.md](srcd.md): `srcd` (Go), the daemon running on a developer or build machine, interfacing
  between the jj cli and the source API and providing a local cache. Serves the API the CLI calls.
- [source-api.md](source-api.md): `source-api` (Go), the remote server for source storage and
  serving. Serves the source API the daemon calls.

All three binaries share one project version, `workspace.package.version` in the root `Cargo.toml`,
and report it with `--version`.

## Overview

```
jjc ──gRPC/UDS──▶ srcd ──gRPC/HTTP2──▶ source-api
                    │
                       └── FUSE mount (one per workspace)
```

- **source-api** is the only source of truth for shared data: objects, the workspace registry, each
  workspace's operation log and op heads, and trunk.
- **srcd** is a local cache and write-back buffer. It owns the FUSE mount, the per-workspace overlay
  of local edits and the working-copy state.
- **jjc** is stateless apart from workspace config. It implements jj-lib's store and working copy
  traits as thin RPC shims to the daemon.

jj semantics (conflict materialization, ignore rules, change IDs, merging concurrent operations)
stay in the CLI, where jj-lib is available. The daemon and the service (Go) only store bytes, Git
trees and dirty paths, and never need to understand jj. The one exception is the service reading the
change IDs of commits pushed to trunk, to keep them unique there (see
[Trunk and push](#trunk-and-push)).

## Interfaces

- **jjc → srcd:** gRPC over a Unix socket, mirroring the jj-lib traits so the CLI stays a thin shim.
  See [srcd.md](srcd.md#api).
- **srcd → source-api:** plaintext gRPC over HTTP/2 (no TLS or auth in v1), with coarse, batched
  calls; the daemon never asks for one object at a time if it can avoid it. See
  [source-api.md](source-api.md#api).

## Repository model

- **Users** are identified by jj's `user.email`. The client asserts it and nothing verifies it,
  because v1 has no authentication. Users aren't registered. Each repo directory records the email
  and workspace it was set up for, and the CLI fails with a clear error if `user.email` no longer
  matches, so changing it never silently switches op logs. An unset email is an error.
- **Workspaces** are entities the source API tracks, keyed by `(repo, user, name)`. A user creates
  one explicitly with a name unique among their workspaces in the repo, following the repo name
  rule. Op-heads calls for a workspace that doesn't exist fail with `NotFound`, so a typo never
  starts a fresh op log. Deleting a workspace drops its registry entry and op heads; its objects
  stay. See [ADR 0001](../adr/0001-op-log-per-workspace.md).
- **One op log and View per workspace.** Op heads are keyed by `(repo, user, workspace)`. A View
  holds exactly one jj workspace, named as in the registry, plus that workspace's bookmarks and
  visible commits, and trunk. Stock `jj workspace add`, `forget` and `rename` are blocked, because
  they would put a second workspace into the View or break its link to the registry.
- **Creating a workspace** publishes its first operation: `main@origin` at the current trunk and `@`
  a new empty commit on it.
- **One machine at a time.** A workspace has one working copy, so it is open on at most one machine.
  The service records the holder, and `UpdateOpHeads` carries a holder token. Opening the workspace
  on another machine takes the hold at once, without contacting the previous machine, and the
  previous machine's next publish is rejected. Edits it never snapshotted stay in its daemon.
- **Unpushed work is private to its workspace:** it is in no other View, even the same user's, and
  workspaces share work only through [trunk](#trunk-and-push). Objects still live in one store
  shared by all repos and users and are readable by ID, which CI relies on; v1 has no ACLs.
- **No merging between workspaces.** Op logs are merged only between concurrent operations of the
  same workspace (see [Concurrency](#publishing-and-consistency)). A View holds one workspace's
  state, so it stays small and is stored whole.
- **Bookmarks** belong to the workspace whose View holds them, by construction. The only gap is that
  anyone can claim another user's email.
- **`jj undo` / `jj op restore`** keep stock jj semantics and affect only the workspace's own op log
  (R11).
- **History** is expected to stay at O(100s) of commits in v1, so every workspace builds jj-lib's
  stock commit index in full when it is created, reading each commit through the daemon, and updates
  it incrementally after that. Neither the daemon nor the service has index-specific support. Each
  workspace has its own repo directory and index, because jj-lib gives a repo directory exactly one
  op-heads store. Larger histories need a [custom commit index](commit-index.md).
- **Sparse patterns** are not supported in v1. The working copy always contains every path.

## Trunk and push

- **Trunk** is one commit ID per cloud repo, held by the service. It starts at the root commit. It
  is the only state workspaces share, and it has no op log: it only moves forward, so its history is
  its commit ancestry. The service also appends `(commit, user, workspace, time)` to a per-repo push
  log on every push. Nothing reads the push log in v1; it's kept for audit and debugging because it
  can't be recovered later.
- **In the View** trunk is the untracked remote bookmark `main@origin`, with no local `main`
  bookmark and no Git remote behind `origin`. Stock `immutable_heads()` covers `trunk()` and
  untracked remote bookmarks, so jj also refuses to rewrite pushed commits locally. `main@origin`
  changes only on push and fetch, so a workspace's op log changes only when the user acts in it.
- **Push** (`jjc cloud push`) moves trunk to a commit:
  1. The CLI refuses commits with conflicts or empty descriptions, as `jj git push` does. These are
     jj semantics, so the service doesn't check them.
  2. The daemon waits until every object written before the call is on the service, using the same
     barrier as publishing.
  3. The service's `PushTrunk(repo, user, workspace, commit)` moves trunk atomically if the commit
     descends from the current trunk, merges included. Pushing the current trunk commit again is a
     no-op, so retries are safe. Anything else is rejected, and the user fetches, rebases and pushes
     again. This fast-forward rule is the whole enforcement of trunk immutability (R12): nothing can
     move trunk backwards or sideways.
  4. The service also rejects the push if a newly pushed commit has the change ID of another commit
     on trunk or in the same push, so a change can never become divergent on trunk, where it could
     never be fixed. It keeps a table from each trunk change ID to its commit, updated atomically
     with trunk, so the check costs O(pushed commits), not O(trunk). Only commits on trunk claim a
     change ID, so other workspaces' private commits never block a push. The root commit is
     excluded.
  5. The CLI publishes an operation that sets `main@origin` to the pushed commit.
- **Fetch** (`jjc cloud fetch`) reads trunk with `GetTrunk` and publishes one operation setting
  `main@origin` to it, or nothing if it hasn't moved. It never rebases bookmarks or changes, as
  `jj git fetch` doesn't.
- **Undo:** undoing a push or fetch moves `main@origin` back in the workspace's View only. Trunk
  doesn't move, and the next fetch restores `main@origin`. A push can't be undone.
- **CI** reads trunk with `GetTrunk` and checks out its tree, with no op log.

## Object model

- **IDs** are Git-compatible SHA-1 object IDs, content-addressed, so every object except op heads is
  immutable and freely cacheable. All hashing uses collision detection (sha1dc, as Git does).
- **Objects:** commits, trees and blobs are valid Git objects (the CLI's encoding is in
  [jjc.md](jjc.md#object-encoding)). Operations and views are stored in the same object store.
- **`tree_id`** is the Git object ID of a plain root tree. It is neither a commit ID nor a change
  ID: a jj commit points to its root tree, and many commits can share one. Change IDs live inside
  the commit object. The daemon never looks at them, and the service reads them only on push. A jj
  tree with conflicts is a set of several tree IDs, so the working copy always serves one plain
  tree: the CLI materializes conflicts into it before calling `Checkout`.
- **Writing:** the CLI serializes objects and the daemon hashes them and returns the ID, so there is
  one hashing implementation. The service re-hashes every upload and rejects any object whose ID
  doesn't match.
- **Blob metadata:** Git trees don't record blob sizes. The service computes `(size, SHA-256)` for
  every blob on upload and stores it next to the Git ID; the daemon computes the same values for
  blobs it writes. This lets `stat` and `readdir` work without downloading blobs (R2).

## Publishing and consistency

- **Writing objects:** objects written by the CLI are stored in the daemon and uploaded in the
  background. Nobody else can see them until an operation that references them is published.
- **Publishing** is not [pushing](#trunk-and-push) and needs no separate command. Every jj command
  that changes the repo ends by writing an operation and calling `UpdateOpHeads` on the workspace's
  op heads, and that call makes the change durable, so it is there wherever the workspace is opened
  next. It is synchronous: it returns only once the operation, every object it references and the
  new op heads are on the service. If any step fails, the command fails. Op heads never point to
  data the service doesn't have.
- **Reading op heads:** every command reads the workspace's op heads from the service, so reads are
  never stale. Trunk is read only by push and fetch.
- **Crash safety:** if anything dies before a publish finishes, the command fails and nothing
  published is lost. Objects written but never published aren't referenced by anything. Working-copy
  edits are still in the daemon's persisted overlay, so rerunning the command recreates them (R6).
- **Concurrency:** jj doesn't lock the repo across processes. Two commands in the same workspace
  that run at the same time (two terminals, or an IDE and a terminal), or a command run with an
  older `--at-op`, both start from the same op head and each publishes its own operation. Because
  `UpdateOpHeads(old, new)` removes `old` and adds `new`, the second writer doesn't fail; the
  workspace just ends up with two op heads, and the next jj command merges them with stock jj-lib
  logic (R5). If both operations rewrote the same change, the user sees a divergent change and
  resolves it the normal way. Two machines never publish to one workspace, because only the holder
  can. Different workspaces never share op heads, so their operations never need merging; they meet
  only on trunk, where concurrent pushes are serialized by the fast-forward rule.

## Known limitations (v1)

- **Network round trips per command.** A command after an edit takes about 4 round trips
  (`GetOpHeads`, fetch the new op and View, `PutObjects`, `UpdateOpHeads`), plus 2 more if it also
  changes the repo. This is fine on a low-latency network and noticeable over high-latency links
  such as a VPN. Commands that need the service fail when it is unreachable.
- **Ignored files cost snapshot time.** The daemon reports every changed path and the CLI applies
  ignore rules, so snapshot time grows with the number of ignored files in the mount (roughly tens
  of ms per 100k paths). Large build trees should write outputs outside the mount (e.g. Bazel
  `--output_base`, `CARGO_TARGET_DIR`).

## Future work

- **Sharing unpushed work.** A fetch could import another workspace's bookmarks, read from its op
  heads, as read-only remote bookmarks (e.g. `feature@alice/fix-login`), so users can review or
  build on each other's work, or stack on their own work from another workspace, without merging op
  logs.
- **[Custom commit index](commit-index.md)** for large histories: the service indexes trunk, the CLI
  indexes only the workspace's private commits, and trunk is walked on demand.
- **Async publish and watched op heads**, if round-trip latency or network blips hurt (e.g.
  developers over VPN):
  - Writes: `UpdateOpHeads` returns once the new operation is fsync'd locally, and a persisted queue
    publishes operations to the service in order. The service's remove-`old`, add-`new` semantics
    need no changes.
  - Reads: the daemon keeps a `WatchOpHeads` stream open, prefetches new ops and Views, and serves
    `GetOpHeads` locally as the service heads plus unpublished local heads.
  - Costs: unpublished operations become data that exists only on the daemon, a workspace moved to
    another machine sees them only after publish, and the CLI should warn about operations that stay
    unpublished.
- **Snapshot filtering hooks**, if ignored files make snapshots slow. Two generic hooks that know
  nothing about ignore rules: `ListDirty(ws, skip_prefixes[])`, and reporting a directory missing
  from the base tree as one entry (expanded on request). The CLI would compute the skip list from
  ignored directories with jj-lib, store it in its working-copy state, and drop entries when a
  `.gitignore` changes.
