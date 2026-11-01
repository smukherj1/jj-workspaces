# Commit Index

**Status:** Future work, not in v1. This sketches the direction and the behavior users will see; it
is not a design.

## v1

A cloud repo is assumed to have O(100s) of commits. Each workspace builds jj-lib's stock index
eagerly in its repo directory, reading every commit through the daemon one at a time. Neither the
daemon nor the source API has any index-specific support. Creation cost grows with history length,
which is acceptable at this size.

The source API does keep one piece of jj data from v1 on: a table of the change IDs of commits on
trunk, each mapped to the trunk commit that holds it. `PushTrunk` fills it and uses it to reject a
push that would make a change divergent on trunk (see [DESIGN](DESIGN.md#trunk-and-push)).

## Custom index

The index is split along the line between shared and private history:

- **Trunk** is shared, append-only and immutable, so the source API indexes it once for all users,
  extending the trunk change-ID table. Clients never index trunk history in full; they walk it on
  demand.
- **A workspace's private commits** (unpushed, hidden and rewritten ones) are few and only that
  workspace sees them, so the CLI indexes them locally. Other users' and workspaces' private commits
  are never indexed or visible.

Creating a workspace then costs nothing proportional to history length (R1). Most users never look
at most of trunk, so most of it is never walked.

## Contracts and deviations from jj

- **The source API reads jj data.** It parses change IDs and evaluates a subset of revsets over
  trunk, so it's no longer jj-agnostic. It still never reads operations or Views.
- **Revsets:** expressions bounded to a small part of trunk, such as the default `jj log` revset,
  `trunk()..@` or `ancestors(x, 10)`, always work. Expressions that would scan all of trunk, such as
  `all()`, `::trunk()` or `description(x)` over trunk, work only for a supported subset that the
  source API evaluates. Anything else fails with a clear error asking to narrow the range. Results
  are never silently partial.
- **ID prefixes** resolve, and short IDs are shown unique, within the workspace's private commits
  plus all of trunk. Other users' private commits never affect them. As in stock jj, a short ID
  shown earlier can become ambiguous as trunk grows.
- **Change IDs** keep stock jj semantics and are never rewritten. A change ID is unique on trunk,
  enforced on push.
- **Hidden commits, `jj evolog`, `jj undo`, `jj op restore` and `--at-op`** behave as in stock jj,
  because every commit a workspace's op log references is in its private index.
- **Revsets and prefix lookups that touch trunk need the source API**, as every command already does
  in v1.
- **Scope:** this covers commit history only. Op-log size is a separate concern.
