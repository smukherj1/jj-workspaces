# Re-brand Plan: JJ Cloud Workspaces → JJ Cloud Source

Rename the product, binaries, protos, code and docs so that "workspace" only means a workspace (a jj
workspace with its own operation log), never the product or the remote service. Done before
[M2](mvp-impl-roadmap.md#m2-shared-repository-without-a-working-copy) starts, so M2 code is written
with the new names. Nothing has launched, so everything is renamed outright, with no aliases or
migration. See the [glossary](../../GLOSSARY.md) for the terms.

**Status:** Not started. The glossary is already updated.

## Outcome

- The product is **JJ Cloud Source**. The binaries are `jjc` (CLI), `srcd` (daemon) and `source-api`
  (remote service).
- The domain terms are **workspace** (was "cloud workspace") and **source API** (was "workspace
  service"). "Cloud repo" stays.
- `source-api` serves five gRPC services, `Repo`, `Workspace`, `Objects`, `OpHeads` and `Trunk`,
  instead of one `WorkspaceService`.
- No old name is left outside the [allowlist](#4-docs-and-leftover-check).

## Decisions

### Names

| Old                                                                                  | New                                                               |
| ------------------------------------------------------------------------------------ | ----------------------------------------------------------------- |
| Product "JJ Cloud Workspaces"                                                        | "JJ Cloud Source"                                                 |
| Term "cloud workspace"                                                               | "workspace"                                                       |
| Term "workspace service", "remote service" (the remote)                              | "source API"                                                      |
| `jjw` (`bin-rs/jjw`, crate `jjw`)                                                    | `jjc` (`bin-rs/jjc`, crate `jjc`)                                 |
| `jjw-daemon` (`bin-go/jjw-daemon`)                                                   | `srcd` (`bin-go/srcd`)                                            |
| `jjw-workspace-service` (`bin-go/jjw-workspace-service`)                             | `source-api` (`bin-go/source-api`)                                |
| `lib-go/daemon`, `lib-go/daemon/serviceclient`                                       | `lib-go/srcd`, `lib-go/srcd/sourceapiclient`                      |
| `lib-go/service`                                                                     | `lib-go/sourceapi`                                                |
| `jjw cws <sub>` (`commands/cws.rs`, `commands/cws/`)                                 | `jjc cloud <sub>` (`commands/cloud.rs`, `commands/cloud/`)        |
| Config key `cws.daemon-socket`                                                       | `cloud.daemon-socket`                                             |
| Socket `$XDG_RUNTIME_DIR/jjw/daemon.sock`                                            | `$XDG_RUNTIME_DIR/jjc/srcd.sock`                                  |
| Proto `jjcw.service.v1` in `proto/jjcw/service/v1/workspace_service.proto`           | `jjc.sourceapi.v1` in `proto/jjc/sourceapi/v1/source_api.proto`   |
| Proto `jjcw.daemon.v1` in `proto/jjcw/daemon/v1/daemon.proto`                        | `jjc.srcd.v1` in `proto/jjc/srcd/v1/srcd.proto`                   |
| Generated `gen-go/jjcw/{service,daemon}/v1`, aliases `servicev1`, `daemonv1`         | `gen-go/jjc/{sourceapi,srcd}/v1`, aliases `sourceapiv1`, `srcdv1` |
| Daemon flags `--service-addr`, `--service-timeout`                                   | `--source-api-addr`, `--source-api-timeout`                       |
| `GetStatusResponse.service_address`, `service_status`; `ServiceStatus`               | `source_api_address`, `source_api_status`; `SourceApiStatus`      |
| `ServiceStatusCode`, `SERVICE_STATUS_CODE_*`                                         | `SourceApiStatusCode`, `SOURCE_API_STATUS_CODE_*`                 |
| `jjw cws status` output `service <addr>: <status>`, error `workspace service is <s>` | `source-api <addr>: <status>`, error `source-api is <s>`          |
| Env `JJW_BIN`, `JJW_DAEMON_BIN`, `JJW_SERVICE_BIN`                                   | `JJC_BIN`, `SRCD_BIN`, `SOURCE_API_BIN`                           |
| e2e `jjw_bin`, `daemon_bin`, `service_bin`, `TestEnv::jjw`                           | `jjc_bin`, `srcd_bin`, `source_api_bin`, `TestEnv::jjc`           |
| e2e `Service`, `UnhealthyService`                                                    | `SourceApi`, `UnhealthySourceApi`                                 |
| jj backend types `cws`, working copy `cws-none` (planned in M2, not built)           | `cloud`, `cloud-none`                                             |
| Spec `jj-cli.md`, `daemon.md`, `workspace-service.md`                                | `jjc.md`, `srcd.md`, `source-api.md`                              |

Unchanged: the git repo name `jj-workspaces`, the Go module path
`github.com/smukherj1/jj-workspaces`, git history, port 7470, the daemon's `Daemon` service and the
word "daemon" in prose and in `jjc cloud status` output (`daemon: ok …`), and Cargo's own
`workspace` keys. Prose may still say "the service" where the context is clear. Identifiers, flags
and output say `source-api` / `SourceApi`.

### source-api services

`WorkspaceService` splits into five services in `jjc.sourceapi.v1`. RPCs keep their VerbNoun names,
because proto messages are scoped to the package and `Objects.Get` and `OpHeads.Get` would both want
a `GetRequest`.

| Service     | RPCs                                                                                                             |
| ----------- | ---------------------------------------------------------------------------------------------------------------- |
| `Repo`      | `CreateRepo`                                                                                                     |
| `Workspace` | `CreateWorkspace`, `ListWorkspaces`, `DeleteWorkspace`, `OpenWorkspace`                                          |
| `Objects`   | `GetObjects`, `DownloadObject`, `GetCommitGraph`, `GetTreeRecursive`, `HasObjects`, `PutObjects`, `UploadObject` |
| `OpHeads`   | `GetOpHeads`, `UpdateOpHeads`                                                                                    |
| `Trunk`     | `GetTrunk`, `PushTrunk`                                                                                          |

As in M1, the protos declare the services with no RPCs. Each RPC is added in the milestone that
first needs it.

The daemon gets a `Repo` service too. Its planned RPCs are renamed the same way:
`Workspace.CreateRepo` → `Repo.CreateRepo`, and `Workspace.Create`, `List`, `Delete` and `Open` →
`Workspace.CreateWorkspace`, `ListWorkspaces`, `DeleteWorkspace` and `OpenWorkspace`. `WorkingCopy`
RPCs (`Checkout`, `Snapshot`, `ListDirty`, …) don't collide with any other message names in the
package, so they keep their names.

Health on `source-api` reports `SERVING` for `""` and for each of the five fully qualified service
names. The daemon's `GetStatus` still checks `""`.

### Not done

- No ADR. A rename before launch is cheap to reverse, and the glossary's _Avoid_ entries say why the
  old names are gone.
- No permanent CI lint for old names. Step 4 checks once.

## 1. Protos

Outcome: protos have the new packages, paths and service split. Code compiles against the generated
code, and no behavior changes.

- [ ] `git mv` the protos to `proto/jjc/sourceapi/v1/source_api.proto` and
      `proto/jjc/srcd/v1/srcd.proto`. Set `package` and `go_package`
      (`github.com/smukherj1/jj-workspaces/gen-go/jjc/{sourceapi,srcd}/v1;{sourceapiv1,srcdv1}`).
      Update the header comments to name `source-api` and `srcd`.
- [ ] `source_api.proto`: replace `WorkspaceService {}` with empty `Repo`, `Workspace`, `Objects`,
      `OpHeads` and `Trunk` services.
- [ ] `srcd.proto`: add an empty `Repo` service. Rename the `GetStatus` fields and types per
      [Names](#names).
- [ ] Delete `gen-go/jjcw` and regenerate into `gen-go/jjc`. Update Go imports and aliases.
- [ ] `bin-rs/jjw/build.rs` and `src/proto.rs`: the new proto path and
      `tonic::include_proto!("jjc.srcd.v1")`. Update the Rust uses of the renamed status types.
- [ ] `lib-go/service`: register every new service (as empty implementations) and set health for
      `""` and for each service name.

Tests:

- [ ] Integration (existing `lib-go/service/server_test.go`): health is `SERVING` for `""` and each
      of the five service names, and `NOT_SERVING` for all of them after shutdown.
- [ ] Existing Go, Rust and e2e tests pass with only renames.

## 2. Go binaries and libraries

Outcome: the Go binaries are `srcd` and `source-api`, built from the new directories with the new
flags and strings.

- [ ] `git mv bin-go/jjw-daemon bin-go/srcd`, `bin-go/jjw-workspace-service bin-go/source-api`,
      `lib-go/daemon lib-go/srcd`, `lib-go/daemon/serviceclient lib-go/srcd/sourceapiclient` and
      `lib-go/service lib-go/sourceapi`. Rename the Go packages to match and fix imports.
- [ ] `srcd` flags `--source-api-addr` and `--source-api-timeout`. Update the usage and help text,
      the error `srcd is already running on <socket>`, and `--version` printing `srcd <version>`.
      Identifiers that mean the remote become `sourceAPI…`, e.g. `d.service` → `d.sourceAPI`.
- [ ] `source-api --version` prints `source-api <version>`.
- [ ] `Taskfile.yml`: Go build targets, the `desc`, and the e2e env vars `SRCD_BIN` and
      `SOURCE_API_BIN`. `JJW_BIN` is renamed with the CLI in step 3.
- [ ] `e2e/src/lib.rs`: `srcd_bin`, `source_api_bin`, `SourceApi` and `UnhealthySourceApi`, the
      process names in spawn errors, and `Daemon::start` passing `--source-api-addr` and
      `--source-api-timeout`.

Tests:

- [ ] Existing Go unit and integration tests pass after the renames. The version tests expect the
      new binary names.
- [ ] E2E passes.

## 3. Rust CLI and wiring

Outcome: the CLI is `jjc` with a `cloud` namespace and the new socket path, and CI builds the
renamed binaries.

- [ ] `git mv bin-rs/jjw bin-rs/jjc`. Change the package and `[[bin]]` name to `jjc`, the
      `CliRunner` name, and `--version` output `jjc 0.1.0 (jj 0.46.0)`. Update `Cargo.lock` and
      `cargo build -p jjc` in the Taskfile.
- [ ] `git mv src/commands/cws.rs src/commands/cloud.rs` and `src/commands/cws src/commands/cloud`.
      The clap subcommand is `cloud`.
- [ ] `daemon_client.rs`: config key `cloud.daemon-socket`, and the default
      `$XDG_RUNTIME_DIR/jjc/srcd.sock`. The error names `srcd` and its hint names
      `cloud.daemon-socket`. `srcd`'s default `--socket` changes in the same commit, so both sides
      agree.
- [ ] `commands/cloud/status.rs`: output `source-api <addr>: <status>` and the error
      `source-api is <status>`.
- [ ] `block_repo_creation.rs`: `jjc can't create repositories yet`.
- [ ] `e2e` and `Taskfile.yml`: `JJC_BIN`, `jjc_bin`, `TestEnv::jjc`, `["cloud", "status"]`, and the
      doc comments.
- [ ] `.github/workflows/ci.yml`: any step or artifact naming the old binaries.

Tests:

- [ ] Unit (existing `daemon_client.rs` tests): the socket comes from `cloud.daemon-socket`, falls
      back to `/run/user/1000/jjc/srcd.sock`, and is an error when `XDG_RUNTIME_DIR` is unset or
      empty.
- [ ] E2E (existing `cli.rs`, `status.rs`): `jjc --version`, `jjc cloud status` output with the
      `source-api` line, and the blocked `jjc git init`.

## 4. Docs and leftover check

Outcome: every doc uses the new names, and no old name is left outside the allowlist.

- [ ] `git mv` `docs/spec/jj-cli.md` → `jjc.md`, `daemon.md` → `srcd.md`, and `workspace-service.md`
      → `source-api.md`. Rename their `#` titles and update every link to them.
- [ ] `PRD.md`, `DESIGN.md` and the three component specs: the product name, binaries, commands,
      config key, socket path, flags and proto names per [Names](#names). "Cloud workspace" becomes
      "workspace", and "workspace service" and "remote service" become "source API". Where a
      workspace is contrasted with a stock jj one, say "stock jj workspace" explicitly.
- [ ] `source-api.md`: split the API table by service per
      [source-api services](#source-api-services), and list the health names.
- [ ] `srcd.md`: add a `Repo` service section with `CreateRepo`, and rename the `Workspace` RPCs.
      `jjc.md` and the M2 plan use the new names.
- [ ] `m1-implementation.md` and `m2-implementation.md`: every name per [Names](#names), including
      M2's directory layout (`lib-go/sourceapi/...`, `lib-go/srcd/...`), backend types `cloud` and
      `cloud-none`, the message `workspaces are managed with jjc cloud workspace`, and service-split
      RPC references (e.g. `Workspace.CreateRepo` → `Repo.CreateRepo`).
- [ ] `mvp-impl-roadmap.md`: the names in M1–M7, and one line before M2 linking this plan as a
      prerequisite. Mark this plan done.
- [ ] `docs/adr/0001-op-log-per-workspace.md`: "workspace service" → "source API", "cloud workspace"
      → "workspace".
- [ ] `AGENTS.md` title "JJ Cloud Source Project". `.claude/skills/rust-coding/SKILL.md` example
      paths (`commands/cloud.rs`). Check `.claude/skills/create-implementation-plan` as well.
- [ ] `task fmt:md`.
- [ ] Leftover check:
      `git grep -n -iE 'jjw|\bcws\b|jjcw|workspace[-_ ]?service|cloud[ -]workspace|remote service'`
      returns only the allowlist: the Go module path and import paths containing
      `smukherj1/jj-workspaces`, the _Avoid_ entries in `GLOSSARY.md`, and this plan. Fix anything
      else.
