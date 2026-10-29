# jj-cli

Rust binary for jj and a minimal shim to interact with the daemon. See [DESIGN.md](DESIGN.md) for
the overall design and [daemon.md](daemon.md#api) for the API it calls.

The binary is `jjw`: stock jj with custom store and working copy implementations registered through
jj-lib's store factories. It pins one jj version, and the `Backend` and `WorkingCopy` traits, plus
the commit encoding below, are checked against each new version before upgrading.

## Binary

- `jjw --version` prints the project version and the pinned jj version, e.g.
  `jjw 0.1.0 (jj 0.46.0)`.
- Custom commands live under `jjw cws <sub>`. All other commands are stock jj.
- **Repo creation is blocked** until cloud repos can be created: a dispatch hook rejects
  `jjw git init`, `jjw git clone` and `jjw debug init-simple` with
  `jjw can't create repositories yet` before anything touches disk. Existing stock jj repos still
  load (unsupported and untested).

## Daemon connection

- The socket path comes from the jj config key `cws.daemon-socket`, falling back to
  `$XDG_RUNTIME_DIR/jjw/daemon.sock`. If neither is set, commands that need the daemon fail and
  point to the config key.
- jj runs commands on `pollster`, so the client owns a current-thread tokio runtime and blocks on
  each call. It connects with a 5 s timeout and sets a 30 s deadline on each RPC, longer than the
  daemon's own timeout for calls to the service.
- If the daemon can't be reached, commands fail with `can't reach jjw-daemon at <socket>: <cause>`
  and a hint to start it.

## User identity

- The user is jj's `user.email`. Every `OpHeads` call and every push sends it, so it selects whose
  op log a command reads and publishes to. See [DESIGN.md](DESIGN.md#repository-model).
- The repo directory records the email it was set up with. A command that loads the repo fails with
  `this repo directory belongs to <old>; user.email is <new>` and a hint to set `user.email` back,
  for example only for this repo with `jj config set --repo`. An unset `user.email` fails with a
  hint to set it.

## Commands

- **`jjw cws status`** calls `Daemon.GetStatus` and prints:

  ```
  daemon: ok (version 0.1.0, socket /run/user/1000/jjw/daemon.sock)
  service localhost:7470: SERVING
  ```

  A service that isn't serving is shown as `UNHEALTHY` or `UNREACHABLE`, followed by the daemon's
  details in parentheses. The command exits 0 only when the service is serving, and otherwise fails
  with `workspace service is <status>` after printing both lines.

- **`jjw cws fetch`** calls `Trunk.GetTrunk`. If trunk differs from `main@origin` in the View, it
  publishes one operation that sets the untracked remote bookmark `main@origin` to it and adds it as
  a head. Otherwise it publishes nothing. It never rebases anything.
- **`jjw cws push [-r REV]`** (default `@-`) moves trunk to one commit:
  1. Fails if any commit in `trunk()..REV` has conflicts or an empty description, as `jj git push`
     does.
  2. Calls `Trunk.PushTrunk`. If the commit doesn't descend from trunk, it fails with
     `<commit> doesn't descend from trunk (<trunk>)` and the hint
     `run jjw cws fetch, rebase onto trunk() and push again`.
  3. Publishes an operation that sets `main@origin` to the commit. If the command dies before this,
     the push has still happened, and running it again or fetching records it. Pushing the current
     trunk commit again succeeds without changing trunk.

  `jj undo` of a push only moves `main@origin` back in the user's View. Trunk stays where it is.

## Store backends

- **`Backend`** → daemon `Store`. Objects are serialized with gix-object and sent as raw bytes.
- **`OpStore`** → daemon `OpStore`.
- **`OpHeadsStore`** → daemon `OpHeads`, for the repo directory's user.

### Object encoding

- Commits are encoded byte for byte the way the pinned jj version's Git backend writes them.
- The change ID goes in a `change-id` commit header. Commits that differ only in change ID therefore
  have different bytes and different IDs, and writing the same commit twice is idempotent.
- A conflicted commit lists its tree IDs in a `jj:trees` header. Its Git tree is made of
  `.jjconflict-base-N` / `.jjconflict-side-N` subtrees, as stock jj writes it.
- Nothing is kept in a local extras table. All metadata lives in the commit object or the operation,
  because nothing local is shared.

## Commit index

- The index lives in the machine's repo directory that the daemon sets up (see
  [daemon.md](daemon.md#workspace)). All cloud workspaces of a cloud repo on the machine share it,
  so the index is built once per machine.
- Before building or updating the index, the CLI calls `Store.FetchCommitGraph(view heads)`.
  jj-lib's stock index builder then runs against the daemon's cache with no per-commit network round
  trips.

## Working copy

The custom `WorkingCopy` / `LockedWorkingCopy` backs cloud workspaces. It never writes files to
disk; the daemon serves the files through FUSE.

- **State:** the CLI's working-copy state (the commit's merged tree IDs) is an opaque blob to the
  daemon. Stock jj logic detects stale working copies from what `GetState` returns.
- **Checkout:**
  1. If the commit has conflicts, build one plain tree with jj-lib where conflicted files contain
     conflict markers, and write it through `Store`. This costs time proportional to the conflicted
     paths, not the repo. Otherwise use the commit's tree directly.
  2. Call `Checkout(ws, tree_id)`. The mount changes immediately (jj publishes the operation before
     checking out).
  3. `LockedWorkingCopy::finish(op_id)` calls `Finish` with no entries to record the op ID and
     state. A crash between the two shows up as a stale working copy, fixed by
     `jj workspace update-stale` as in stock jj.
- **Snapshot** (R3):
  1. Call `ListDirty(ws)`.
  2. Drop added (untracked) paths that jj-lib's ignore matcher rejects: `.gitignore` files read
     through the mount, plus `snapshot.auto-track`. Already-tracked files are always kept, matching
     jj. Nothing is cached between snapshots, so `.gitignore` changes need no special handling.
     Dropped paths stay in the daemon's overlay as untracked files.
  3. Call `Snapshot(ws, paths[])` for the rest.
  4. Build the new MergedTree with jj-lib from the stored merged tree plus the entries, parsing
     conflict markers in edited conflicted files back into jj conflicts. Also write the materialized
     plain tree.
  5. After the operation is published, `LockedWorkingCopy::finish(op_id)` calls
     `Finish(ws, op_id, entries, tree_id, state)`.
- **`jj reset` / `recover`** call `Reset(ws, tree_id, state)`.
- **Sparse patterns:** `jj sparse set` fails with a clear error, and the patterns are always
  "everything".

## CI mode

CI checkouts are ephemeral and unrecorded. CI checks out by full commit ID or by `trunk`, which it
resolves with `Trunk.GetTrunk`: it reads the commit, checks out its tree, and never loads the op
log, View or index, so CI cold start doesn't depend on the index. CI never publishes operations.

## Defaults

- `trunk()` is `latest(present(main@origin) | root())`. Cloud repos have no Git remotes, so the
  stock alias would find nothing.
- The default `jj log` revset and `immutable_heads()` stay stock. A View holds only the user's own
  commits and trunk, so the stock log doesn't show other users' work, and pushed commits are
  immutable.

## Future work

- **Snapshotting build outputs** for incremental builds. The daemon already tracks every write in
  the mount, so only the CLI would change.
- **Presubmit on unpushed commits.** CI could test a user's commits before they're pushed by merging
  their diff onto a trunk commit: the tree `trunk_tree + (tip_tree - base_tree)`, computed with
  jj-lib's tree merge, materialized and written as one plain tree, then checked out. This needs no
  op log, index or `MutableRepo`, only reading the user's commits by ID, which works because objects
  are shared. The new tree is never published, so it's an unreferenced object that GC's grace period
  covers. Open parts: how CI is given `(base, tip)`, or whether the service computes the fork point
  with trunk, and an ACL letting CI read users' commits once authorization exists.
