# jj-cli

Rust binary for jj and a minimal shim to interact with the daemon. See [DESIGN.md](DESIGN.md) for the overall design and [daemon.md](daemon.md#api) for the API it calls.

The binary is stock jj with custom store and working copy implementations registered through jj-lib's store factories. It pins one jj version, and the `Backend` and `WorkingCopy` traits, plus the commit encoding below, are checked against each new version before upgrading.

## Store backends

- **`Backend`** → daemon `Store`. Objects are serialized with gix-object and sent as raw bytes.
- **`OpStore`** → daemon `OpStore`.
- **`OpHeadsStore`** → daemon `OpHeads`.

### Object encoding

- Commits are encoded byte for byte the way the pinned jj version's Git backend writes them.
- The change ID goes in a `change-id` commit header. Commits that differ only in change ID therefore have different bytes and different IDs, and writing the same commit twice is idempotent.
- A conflicted commit lists its tree IDs in a `jj:trees` header. Its Git tree is made of `.jjconflict-base-N` / `.jjconflict-side-N` subtrees, as stock jj writes it.
- Nothing is kept in a local extras table. All metadata lives in the commit object or the operation, because nothing local is shared.

## Commit index

- The index lives in the machine's repo directory that the daemon sets up (see [daemon.md](daemon.md#workspace)). All cloud workspaces of a cloud repo on the machine share it, so the index is built once per machine.
- Before building or updating the index, the CLI calls `Store.FetchCommitGraph(view heads)`. jj-lib's stock index builder then runs against the daemon's cache with no per-commit network round trips.

## Working copy

The custom `WorkingCopy` / `LockedWorkingCopy` backs cloud workspaces. It never writes files to disk; the daemon serves the files through FUSE.

- **State:** the CLI's working-copy state (the commit's merged tree IDs) is an opaque blob to the daemon. Stock jj logic detects stale working copies from what `GetState` returns.
- **Checkout:**
  1. If the commit has conflicts, build one plain tree with jj-lib where conflicted files contain conflict markers, and write it through `Store`. This costs time proportional to the conflicted paths, not the repo. Otherwise use the commit's tree directly.
  2. Call `Checkout(ws, tree_id)`. The mount changes immediately (jj publishes the operation before checking out).
  3. `LockedWorkingCopy::finish(op_id)` calls `Finish` with no entries to record the op ID and state. A crash between the two shows up as a stale working copy, fixed by `jj workspace update-stale` as in stock jj.
- **Snapshot** (R3):
  1. Call `ListDirty(ws)`.
  2. Drop added (untracked) paths that jj-lib's ignore matcher rejects: `.gitignore` files read through the mount, plus `snapshot.auto-track`. Already-tracked files are always kept, matching jj. Nothing is cached between snapshots, so `.gitignore` changes need no special handling. Dropped paths stay in the daemon's overlay as untracked files.
  3. Call `Snapshot(ws, paths[])` for the rest.
  4. Build the new MergedTree with jj-lib from the stored merged tree plus the entries, parsing conflict markers in edited conflicted files back into jj conflicts. Also write the materialized plain tree.
  5. After the operation is published, `LockedWorkingCopy::finish(op_id)` calls `Finish(ws, op_id, entries, tree_id, state)`.
- **`jj reset` / `recover`** call `Reset(ws, tree_id, state)`.
- **Sparse patterns:** `jj sparse set` fails with a clear error, and the patterns are always "everything".

## CI mode

CI checkouts are ephemeral and unrecorded. CI checks out by full commit ID only: it reads the commit, checks out its tree, and never loads the op log, View or index, so CI cold start doesn't depend on the index. CI never publishes operations.

## Defaults

- The default `jj log` revset is limited to `mine()`, trunk and `@`, because the stock default would show every user's in-progress commits in the shared View.

## Future work

- **Snapshotting build outputs** for incremental builds. The daemon already tracks every write in the mount, so only the CLI would change.
- **View chunking.** If the user count grows well past 50, store the View as content-addressed chunks in `OpStore`, so each operation uploads only the chunks that changed.
