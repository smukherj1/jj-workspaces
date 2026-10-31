# JJ Cloud Workspaces

Cloud-hosted jj repositories whose workspaces are checked out lazily on each machine instead of
cloned.

## Language

**Cloud repo**: A jj repo whose objects, cloud workspaces' operation logs and trunk live on the
workspace service. _Avoid_: remote repo, virtualized repo

**Cloud workspace**: A named jj workspace of a cloud repo, owned by one user, with its own operation
log, bookmarks and in-progress commits that no other workspace sees. It is open on one machine at a
time, and the daemon there serves its working copy on demand instead of writing it to disk.
_Avoid_: virtual workspace, virtualized workspace, VFS workspace, checkout, clone

**Workspace name**: The name a user gives a cloud workspace when creating it, unique among that
user's workspaces in the cloud repo.

**Stock repo**: A jj repo that uses jj's built-in backends (Git or simple), not a cloud repo.

**Repo directory**: A machine's local directory for one cloud workspace, holding its jj repo
metadata and commit index. _Avoid_: local repo, local clone

**User**: A person working in a cloud repo, identified by email, who creates and owns cloud
workspaces. _Avoid_: account

**Op heads**: A cloud workspace's current set of head operations.

**Publish**: To make a cloud workspace's operation durable on the workspace service by updating the
workspace's op heads. _Avoid_: sync

**Trunk**: A cloud repo's single shared line of history. It only moves forward, so a commit on trunk
stays on it forever. _Avoid_: mainline, master

**Push**: To move trunk forward to a commit that descends from it. _Avoid_: land, submit, publish

**Fetch**: To update a cloud workspace's view of trunk to its current position on the workspace
service. _Avoid_: pull, sync
