# One operation log per workspace

**Status:** accepted · **Date:** 2026-10-31

Each workspace has its own operation log and View, keyed by `(repo, user, workspace)` on the source
API. Stock jj instead shares one op log among all workspaces of a repo. We chose this so a user can
keep several workspaces of one repo, one per feature or task, and undo or restore in one without
touching the others. A workspace can move between machines, and an undo on one machine never rewinds
work done in another workspace on another machine.

## Considered options

- **One op log per user per repo** (stock jj semantics across a user's workspaces). Every workspace
  sees the user's commits and bookmarks from the others, so one feature can be stacked on another
  unpushed feature. But `jj undo` undoes the latest operation of any of the user's workspaces, on
  any machine, and only `jj op revert <op>` targets one. Rejected because independent undo matters
  more than seeing unpushed work across workspaces.
- **One op log per workspace** (chosen).

## Consequences

- Workspaces share work only through trunk, even the same user's. Stacking on another workspace's
  unpushed work needs a future feature that imports its bookmarks.
- A workspace is an entity the source API tracks: it's created explicitly with a name unique per
  `(repo, user)`, and op heads of an unknown workspace are an error, not a fresh op log.
- Each View has one workspace, so stock `jj workspace add`, `forget` and `rename` are blocked.
- jj-lib gives a repo directory one op-heads store, so each workspace has its own repo directory and
  commit index. A new workspace builds its index of trunk history from scratch, which is fine for
  v1's small histories; the future [custom commit index](../spec/commit-index.md) removes the cost.
- A workspace has one working copy, so it's open on one machine at a time, and moving it takes the
  hold from the previous machine.
