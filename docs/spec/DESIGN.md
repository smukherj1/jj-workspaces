# JJ Workspaces Design

See [PRD.md](PRD.md) for the product requirements. This document covers the decisions that span components and the contracts between them. Each component's API, behavior and internals are in its own document.

## Components

- [jj-cli.md](jj-cli.md): Custom jj binary using jj-lib to use the daemon as the storage and working copy backend.
- [daemon.md](daemon.md): Daemon running on developer or build machine interfacing between the jj cli and the workspaces service and providing a local cache. Serves the API the CLI calls.
- [workspace-service.md](workspace-service.md): Remote workspace service for source storage and serving. Serves the API the daemon calls.

## Overview

```
jj-cli ──gRPC/UDS──▶ daemon ──gRPC/HTTP2──▶ workspace-service
                       │
                       └── FUSE mount (one per workspace)
```

- **workspace-service** is the only source of truth for shared data: objects, operation log, op heads.
- **daemon** is a local cache and write-back buffer. It owns the FUSE mount, the per-workspace overlay of local edits and the working-copy state.
- **jj-cli** is stateless apart from workspace config. It implements jj-lib's store and working copy traits as thin RPC shims to the daemon.

jj semantics (conflict materialization, ignore rules, change IDs, merging concurrent operations) stay in the CLI, where jj-lib is available. The daemon and the service (Go) only store bytes, Git trees and dirty paths, and never need to understand jj.

## Interfaces

- **jj-cli → daemon:** gRPC over a Unix socket, mirroring the jj-lib traits so the CLI stays a thin shim. See [daemon.md](daemon.md#api).
- **daemon → workspace-service:** gRPC, with coarse, batched calls; the daemon never asks for one object at a time if it can avoid it. See [workspace-service.md](workspace-service.md#api).

## Repository model

- **One shared op log and View per repo**, for all users. Expected scale is under 50 active users, so the View is stored whole.
- **Trunk** lives in the workspace service as an ordinary bookmark in the shared View. There is no Git host behind it, and in v1 anyone can move it.
- **`jj undo` / `jj op restore`** keep stock jj semantics, even though they can revert or clobber other users' operations.
- **History** is expected to stay under 100k commits, so every machine builds the full commit index locally from commits streamed in bulk from the service. There is no shallow history and no server-side index.
- **Sparse patterns** are not supported in v1. The working copy always contains every path.

## Object model

- **IDs** are Git-compatible SHA-1 object IDs, content-addressed, so every object except op heads is immutable and freely cacheable. All hashing uses collision detection (sha1dc, as Git does).
- **Objects:** commits, trees and blobs are valid Git objects (the CLI's encoding is in [jj-cli.md](jj-cli.md#object-encoding)). Operations and views are stored in the same object store.
- **`tree_id`** is the Git object ID of a plain root tree. It is neither a commit ID nor a change ID: a jj commit points to its root tree, and many commits can share one. Change IDs live inside the commit object, so the daemon and the service never look at them. A jj tree with conflicts is a set of several tree IDs, so the working copy always serves one plain tree: the CLI materializes conflicts into it before calling `Checkout`.
- **Writing:** the CLI serializes objects and the daemon hashes them and returns the ID, so there is one hashing implementation. The service re-hashes every upload and rejects any object whose ID doesn't match.
- **Blob metadata:** Git trees don't record blob sizes. The service computes `(size, SHA-256)` for every blob on upload and stores it next to the Git ID; the daemon computes the same values for blobs it writes. This lets `stat` and `readdir` work without downloading blobs (R2).

## Publishing and consistency

- **Writing objects:** objects written by the CLI are stored in the daemon and uploaded in the background. Nobody else can see them until an operation that references them is published.
- **Publishing:** jj has no separate "push". Every jj command that changes the repo ends by writing an operation and calling `UpdateOpHeads`, and that call makes the change visible and durable. It is synchronous: it returns only once the operation, every object it references and the new op heads are on the service. If any step fails, the command fails. Op heads never point to data the service doesn't have.
- **Reading op heads:** every command reads op heads from the service, so reads are never stale.
- **Crash safety:** if anything dies before a publish finishes, the command fails and nothing published is lost. Objects written but never published aren't referenced by anything. Working-copy edits are still in the daemon's persisted overlay, so rerunning the command recreates them (R6).
- **Concurrency:** jj doesn't lock the repo across processes. Two commands that run at the same time (in two workspaces, or on two machines) both start from the same op head and each publishes its own operation. Because `UpdateOpHeads(old, new)` removes `old` and adds `new`, the second writer doesn't fail; the repo just ends up with two op heads, and the next jj command merges them with stock jj-lib logic (R5). If both operations rewrote the same change, the user sees a divergent change and resolves it the normal way.

## Known limitations (v1)

- **Network round trips per command.** A command after an edit takes about 4 round trips (`GetOpHeads`, fetch the new op and View, `PutObjects`, `UpdateOpHeads`), plus 2 more if it also changes the repo. This is fine on a low-latency network and noticeable over high-latency links such as a VPN. Commands that need the service fail when it is unreachable.
- **Ignored files cost snapshot time.** The daemon reports every changed path and the CLI applies ignore rules, so snapshot time grows with the number of ignored files in the mount (roughly tens of ms per 100k paths). Large build trees should write outputs outside the mount (e.g. Bazel `--output_base`, `CARGO_TARGET_DIR`).

## Future work

- **Landing and trunk protection.** Options: a server-owned refs namespace moved only through a `Land` RPC and imported as a remote bookmark (`main@origin`), or the service decoding Views on `UpdateOpHeads` to enforce policy on protected bookmarks.
- **Async publish and watched op heads**, if round-trip latency or network blips hurt (e.g. developers over VPN):
  - Writes: `UpdateOpHeads` returns once the new operation is fsync'd locally, and a persisted queue publishes operations to the service in order. The service's remove-`old`, add-`new` semantics need no changes.
  - Reads: the daemon keeps a `WatchOpHeads` stream open, prefetches new ops and Views, and serves `GetOpHeads` locally as the service heads plus unpublished local heads.
  - Costs: unpublished operations become data that exists only on the daemon, other machines see changes only after publish, and the CLI should warn about operations that stay unpublished.
- **Snapshot filtering hooks**, if ignored files make snapshots slow. Two generic hooks that know nothing about ignore rules: `ListDirty(ws, skip_prefixes[])`, and reporting a directory missing from the base tree as one entry (expanded on request). The CLI would compute the skip list from ignored directories with jj-lib, store it in its working-copy state, and drop entries when a `.gitignore` changes.

## Open questions

- **Operation and view encoding.** How the CLI serializes operations and views (e.g. stock jj's protobuf op-store format), how their IDs fit the Git-compatible scheme, and therefore how the daemon hashes them and the service verifies them on upload.
