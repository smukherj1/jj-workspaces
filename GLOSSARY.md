# JJ Cloud Source

Cloud-hosted jj repositories whose workspaces are checked out lazily on each machine instead of
cloned.

## Language

**Cloud repo**: A jj repo whose objects, workspaces' operation logs and trunk live on the source
API. _Avoid_: remote repo, virtualized repo

**Workspace**: A named jj workspace of a cloud repo, owned by one user, with its own operation log,
bookmarks and in-progress commits that no other workspace sees, unlike stock jj workspaces, which
share one. It is open on one machine at a time, and the daemon there serves its working copy on
demand instead of writing it to disk. _Avoid_: cloud workspace, source, virtual workspace, VFS
workspace, checkout, clone

**Workspace name**: The name a user gives a workspace when creating it, unique among that user's
workspaces in the cloud repo.

**Source API**: The remote that holds every cloud repo's objects, workspaces, op heads and trunk,
and is the source of truth for them. _Avoid_: workspace service, remote service, server

**Stock repo**: A jj repo that uses jj's built-in backends (Git or simple), not a cloud repo.

**Repo directory**: A machine's local directory for one workspace, holding its jj repo metadata and
commit index. _Avoid_: local repo, local clone

**User**: A person working in a cloud repo, identified by email, who creates and owns cloud
workspaces. _Avoid_: account

**Op heads**: A workspace's current set of head operations.

**Publish**: To make a workspace's operation durable on the source API by updating the workspace's
op heads. _Avoid_: sync

**Trunk**: A cloud repo's single shared line of history. It only moves forward, so a commit on trunk
stays on it forever. _Avoid_: mainline, master

**Push**: To move trunk forward to a commit that descends from it. _Avoid_: land, submit, publish

**Fetch**: To update a workspace's view of trunk to its current position on the source API. _Avoid_:
pull, sync
