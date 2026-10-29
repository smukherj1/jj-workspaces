# JJ Cloud Workspaces

Cloud-hosted jj repositories whose workspaces are checked out lazily on each machine instead of
cloned.

## Language

**Cloud repo**: A jj repo whose objects, per-user operation logs and trunk live on the workspace
service. _Avoid_: remote repo, virtualized repo

**Cloud workspace**: A jj workspace of a cloud repo whose working copy the daemon serves on demand
instead of writing it to disk. _Avoid_: virtual workspace, virtualized workspace, VFS workspace

**Stock repo**: A jj repo that uses jj's built-in backends (Git or simple), not a cloud repo.

**Repo directory**: A machine's local directory for one user's view of one cloud repo, holding its
jj repo metadata and commit index, shared by that user's cloud workspaces on the machine. _Avoid_:
local repo, local clone

**User**: A person working in a cloud repo, identified by email, with their own operation log,
bookmarks and in-progress commits that no other user sees. _Avoid_: account, owner

**Op heads**: A user's current set of head operations in a cloud repo.

**Publish**: To make an operation visible to all of the user's machines by updating their op heads.
_Avoid_: sync

**Trunk**: A cloud repo's single shared line of history. It only moves forward, so a commit on trunk
stays on it forever. _Avoid_: mainline, master

**Push**: To move trunk forward to a commit that descends from it. _Avoid_: land, submit, publish

**Fetch**: To update a user's view of trunk to its current position on the workspace service.
_Avoid_: pull, sync
