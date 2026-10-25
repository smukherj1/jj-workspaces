# JJ Cloud Workspaces

Cloud-hosted jj repositories whose workspaces are checked out lazily on each machine instead of
cloned.

## Language

**Cloud repo**:
A jj repo whose objects, operation log and op heads live on the workspace service.
_Avoid_: remote repo, virtualized repo

**Cloud workspace**:
A jj workspace of a cloud repo whose working copy the daemon serves on demand instead of writing
it to disk.
_Avoid_: virtual workspace, virtualized workspace, VFS workspace

**Stock repo**:
A jj repo that uses jj's built-in backends (Git or simple), not a cloud repo.
