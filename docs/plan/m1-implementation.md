# M1 Implementation Plan: Skeleton through the stack

Detailed plan for [M1](mvp-impl-roadmap.md#m1-skeleton-through-the-stack). The three binaries build,
start and call each other, with no repository functionality yet. See [DESIGN.md](../spec/DESIGN.md)
for the overall design.

**Status:** In progress. Sections 0, 1, 2 and 3 are done, except the first CI run.

## Outcome

- `jjw-workspace-service` serves gRPC health and an empty `WorkspaceService`.
- `jjw-daemon` serves `Daemon.GetStatus` and the empty `Store`, `OpStore`, `OpHeads`, `WorkingCopy`,
  `Workspace` and `Prefetch` services on a Unix socket. `GetStatus` checks the service's health
  with a bounded timeout.
- `jjw` is stock jj 0.46.0 plus a `cws` subcommand namespace. `jjw cws status` reports whether the
  daemon and the service are up. `jjw git init`, `jjw git clone` and `jjw debug init-simple` fail
  with a clear error.
- `task e2e` builds everything, starts a service and a daemon, and runs `jjw` against them.

Not in M1: store and working copy factories (M2 and M3), repo RPCs, auth and TLS, systemd, version
skew checks between the CLI and the daemon (M6).

## Decisions

- **Binaries:** `jjw` (Rust), `jjw-daemon` and `jjw-workspace-service` (Go).
- **jj pin:** `jj-cli` and `jj-lib` at `=0.46.0`. jj 0.46 needs Rust 1.97.1, so
  `rust-toolchain.toml` pins `1.97.1`.
- **Custom commands** live under `jjw cws <sub>`.
- **Protos** declare every service from the spec, with no RPCs. Each RPC is added in the milestone
  that first needs it. The one exception is `Daemon.GetStatus`, a permanent diagnostic RPC.
- **Repo creation is blocked** until a milestone supports it. A dispatch hook rejects the stock repo
  creation commands. `jjw` still loads existing stock repos, which is unsupported and untested but
  not blocked.
- **Transport:** CLI to daemon is gRPC over a Unix socket. Daemon to service is plaintext gRPC over
  HTTP/2, with no TLS or auth (out of scope for v1).
- **Versioning:** one project version, `version` in the root `Cargo.toml` (`0.1.0`). The Taskfile
  injects it into the Go binaries with `-ldflags -X`. `go build` without it reports `dev`.
- **Logging:** Go `log/slog` text handler on stderr. The CLI keeps jj's `tracing` setup.
- **Repo layout:** directories are split by kind and language: `bin-go/<binary>` and
  `bin-rs/<binary>` for binaries, `lib-go/` and `lib-rs/` for internal libraries, `gen-go/` for
  generated Go code. Generated Rust code isn't committed: `build.rs` generates it at build time.
  `proto/` and `e2e/` are shared and have no language suffix. Go's `internal/` enforcement isn't
  used.
- **Build output:** all three binaries go to `target/debug/`. Cargo puts `jjw` there and the Go
  build writes its binaries to the same directory.
- **Task runner:** [Task](https://taskfile.dev) (`Taskfile.yml`), pinned as a `tool` directive in
  `go.mod`. `task <name>` in this plan means `go tool task <name>`. It is pinned at `v3.53.1`, the
  last release that builds with Go 1.25 (`v3.54` needs Go 1.26), so `go.mod` says `go 1.25.10`.

## Repo layout

```
go.mod                          module github.com/smukherj1/jj-workspaces, go 1.25
buf.yaml, buf.gen.yaml
proto/jjcw/daemon/v1/daemon.proto
proto/jjcw/service/v1/workspace_service.proto
gen-go/jjcw/{daemon,service}/v1/   generated Go, committed
bin-go/jjw-daemon/main.go
bin-go/jjw-workspace-service/main.go
lib-go/version/                 Version string, set by -ldflags
lib-go/daemon/                  daemon gRPC servers, socket handling
lib-go/daemon/serviceclient/    connection to the service (grows into the M2 service client)
lib-go/service/                 service gRPC servers
Cargo.toml                      Cargo workspace (members: bin-rs/*, e2e), workspace.package.version
rust-toolchain.toml
bin-rs/jjw/                     crate `jjw`, binary `jjw`
lib-rs/                         Rust libraries; none in M1, add `lib-rs/*` to members with the first
e2e/                            crate `e2e`, end-to-end tests only
Taskfile.yml
.github/workflows/ci.yml
```

## 0. Scaffolding

Outcome: an empty build in both languages, proto generation, lint, and CI.

- [x] `go.mod`. grpc-go `v1.84.x` and `google.golang.org/protobuf`. `protoc-gen-go`,
      `protoc-gen-go-grpc` and Task (`github.com/go-task/task/v3/cmd/task`) are pinned as `tool`
      directives so `go tool` runs them at a fixed version.
- [x] Root `Cargo.toml` with `[workspace]` members `bin-rs/*` and `e2e`, `edition = "2024"`, and
      `workspace.package.version = "0.1.0"`. Add `rust-toolchain.toml` (`1.97.1`, with `rustfmt` and
      `clippy`). `bin-rs/jjw` and `e2e` start as placeholder crates so the workspace builds; sections
      3 and 4 fill them in.
- [x] `buf.yaml` (v2): module `proto`, lint `STANDARD` except `SERVICE_SUFFIX`, so service names match
      the spec (`Store`, `OpStore`, `Daemon`, ...). Breaking checks use `FILE`.
- [x] `buf.gen.yaml`: local plugins through `go tool protoc-gen-go` and `go tool protoc-gen-go-grpc`,
      output to `gen-go` with `paths=source_relative`.
- [x] `proto/jjcw/service/v1/workspace_service.proto`: `service WorkspaceService {}`.
- [x] `proto/jjcw/daemon/v1/daemon.proto`: empty `Store`, `OpStore`, `OpHeads`, `WorkingCopy`,
      `Workspace` and `Prefetch` services, plus:

      ```proto
              service Daemon {
                rpc GetStatus(GetStatusRequest) returns (GetStatusResponse);
              }
              message GetStatusRequest {}
              message GetStatusResponse {
                string daemon_version = 1;
                string service_address = 2;
                ServiceStatus service_status = 3;
              }

              message ServiceStatus {
                ServiceStatusCode code = 1;
                string details = 2;
              }

              enum ServiceStatusCode {
                SERVICE_STATUS_CODE_UNSPECIFIED = 0;
                SERVICE_STATUS_CODE_OK = 1;
                SERVICE_STATUS_CODE_UNHEALTHY = 2;
                SERVICE_STATUS_CODE_UNREACHABLE = 3;
              }
              ```

- [x] `Taskfile.yml` (schema `version: '3'`). `VERSION` is a dynamic variable read from
      `workspace.package.version` in the root `Cargo.toml`. Each task has a `desc`, so
      `task --list` documents them. Tasks:
  - `proto`: `buf generate`.
  - `build`: `go build -ldflags "-X .../lib-go/version.Version={{.VERSION}}" -o target/debug/ ./bin-go/...`
    and `cargo build -p jjw`.
  - `lint`: `buf lint`, `buf format --diff --exit-code`, `gofmt -l bin-go lib-go gen-go` (fails if
    output), `go vet ./...`, `cargo fmt --check`,
    `cargo clippy --workspace --all-targets -- -D warnings`.
  - `test`: `go test ./...` and `cargo test --workspace --exclude e2e`.
  - `e2e`: depends on `build`, then `cargo test -p e2e` with the binary paths in env vars (see
    section 4).
- [x] `.gitignore`: add `/target`.
- [x] `.github/workflows/ci.yml` on push and pull request, on `ubuntu-latest`:
  - `actions/setup-go` (1.25), rustup from `rust-toolchain.toml`, `bufbuild/buf-action`.
  - `task lint`, `task test` and `task e2e`.
  - `task proto && git diff --exit-code`, so committed generated code is current.
  - On pull requests: `buf breaking --against '.git#branch=main'`.

Done when `task build lint test` passes locally and in CI. Passes locally; CI hasn't run yet.

## 1. Service

Outcome: `jjw-workspace-service` starts, serves health and an empty `WorkspaceService`, and shuts down
cleanly.

- [x] `lib-go/service`: `NewServer() *Server` registers `WorkspaceService` (embedding
      `UnimplementedWorkspaceServiceServer`), `grpc.health.v1` with status `SERVING` for `""` and
      `jjcw.service.v1.WorkspaceService`, and server reflection. `Server` wraps the `*grpc.Server`
      and the health server, so `Serve(lis)` and `Shutdown(timeout)` (`NOT_SERVING`, bounded
      `GracefulStop`, then `Stop`) live in the library.
- [x] `bin-go/jjw-workspace-service`: flag `--listen` (default `localhost:7470`; port `0` picks a free
      port). After the listener binds, print `listening on <host:port>` to stdout with the resolved
      port. The harness relies on this line. On SIGINT or SIGTERM, set health to `NOT_SERVING`,
      `GracefulStop` with a 5 s bound, then `Stop`.
- [x] `--version` prints `jjw-workspace-service <version>`.

Tests (`go test`):

- [x] Start the server on `localhost:0` with bufconn or a real listener. A health `Check` returns
      `SERVING` for `""` and for `jjcw.service.v1.WorkspaceService`.
- [x] `Shutdown` sends `NOT_SERVING` to a health `Watch` stream and returns even though the open
      stream holds up `GracefulStop`.

Done when the test passes and `target/debug/jjw-workspace-service --listen localhost:0` prints its
address and answers `grpcurl -plaintext <addr> grpc.health.v1.Health/Check`.

## 2. Daemon

Outcome: `jjw-daemon` serves its API on a Unix socket and reports the service's health with a
bounded timeout.

- [x] `bin-go/jjw-daemon` flags:
  - `--socket` (default `$XDG_RUNTIME_DIR/jjw/daemon.sock`). If `XDG_RUNTIME_DIR` is unset and there is
    no flag, exit with an error that names `--socket`.
  - `--service-addr` (required).
  - `--service-timeout` (default `10s`). It bounds every call to the service.
  - `--version` prints `jjw-daemon <version>`.
- [x] Socket handling (`lib-go/daemon`):
  - Create the socket's parent directory with mode `0700` if it doesn't exist.
  - If the socket file exists, dial it. A successful connection means another daemon is running:
    exit with `jjw-daemon is already running on <socket>`. Connection refused means the file is
    stale, so remove it.
  - Listen, then `chmod 0600` the socket. Print `listening on <socket>` to stdout.
  - On SIGINT or SIGTERM, `GracefulStop` with a 5 s bound, then `Stop`. Closing the Go listener
    unlinks the socket.
- [x] `lib-go/daemon/serviceclient`: `grpc.NewClient(addr, insecure credentials)`, created once at
      startup. The daemon starts even if the service is down, because cache hits must work offline.
      `Health(ctx)` calls `grpc.health.v1.Health/Check` for `""` with a `--service-timeout` deadline.
- [x] `lib-go/daemon` servers:
  - `grpc.health.v1` for the daemon itself (`SERVING`).
  - Register the empty `Store`, `OpStore`, `OpHeads`, `WorkingCopy`, `Workspace` and `Prefetch`
    services. `protoc-gen-go-grpc` emits registration code for services without methods, so all
    six are registered.
  - Server reflection, so `grpcurl` can call the daemon over its socket.
  - `Daemon.GetStatus` returns `version.Version` and `--service-addr`, and maps `Health` to a
    `ServiceStatusCode`: `SERVING` to `OK`; `NOT_SERVING` or any other health status to `UNHEALTHY`,
    with the status name in `details`; an RPC error (unavailable, deadline exceeded) to
    `UNREACHABLE`, with `details` set to the error, e.g. `deadline exceeded after 10s`.

Tests (`go test`):

- [x] `GetStatus` with a real in-process service: `OK`.
- [x] `GetStatus` with nothing listening at the service address: `UNREACHABLE`, returned well within
      the timeout.
- [x] `GetStatus` against a TCP listener that accepts but never speaks HTTP/2, with a 500 ms
      timeout: `UNREACHABLE` with a deadline error, in about 500 ms.
- [x] `GetStatus` with a service health status of `NOT_SERVING`: `UNHEALTHY`.
- [x] Socket handling: a stale socket file is replaced, a live daemon blocks a second one, a path
      that isn't a socket is refused, the parent directory is `0700` and the socket `0600`.

Done when the tests pass and a manual
`target/debug/jjw-daemon --socket /tmp/d.sock --service-addr <addr>` answers `GetStatus` over the
socket.

## 3. CLI

Outcome: `jjw` behaves as stock jj 0.46.0, adds `jjw cws status`, and refuses to create repos.

- [x] `bin-rs/jjw` crate (`jjw`, binary `jjw`). Dependencies: `jj-cli = "=0.46.0"`,
      `jj-lib = "=0.46.0"`, `clap`, tonic `0.14`, `tonic-prost`, `prost`, `tokio` (`rt`, `net`),
      `hyper-util` and `tower` for the Unix socket connector. Build dependencies: `protox 0.10` and
      `tonic-prost-build 0.14`.
- [x] `build.rs`: compile `proto/jjcw/daemon/v1/daemon.proto` (`../../proto` from the crate) with
      protox, generate client code only with `tonic-prost-build` (`compile_fds`,
      `build_server(false)`), and set `JJ_LIB_VERSION` from `Cargo.lock`.
- [x] `main.rs`: `CliRunner::init().version("<our version> (jj <JJ_LIB_VERSION>)")`, then
      `.add_dispatch_hook(block_repo_creation)` and `.add_subcommand(run_cws_command)`. `.name("jjw")`
      is also needed, or clap prints `jj`. `jjw --version` prints `jjw 0.1.0 (jj 0.46.0)`.
- [x] `daemon_client.rs`: `DaemonClient::connect(settings)`.
  - The socket path comes from jj config `cws.daemon-socket`, falling back to
    `$XDG_RUNTIME_DIR/jjw/daemon.sock`. If neither is available, error and point to the config key.
  - jj runs commands with `pollster`, so the client owns a current-thread tokio runtime and
    `block_on`s each call. M2's store shims reuse this client.
  - It connects through a tonic `Endpoint` with a Unix socket connector and a 5 s connect timeout,
    and sets a 30 s deadline on each RPC (longer than the daemon's service timeout).
  - If the connection fails: user error `can't reach jjw-daemon at <socket>: <cause>`, with the hint
    `Start it with: jjw-daemon --service-addr <host:port>`.
- [x] `commands/cws/status.rs`: `jjw cws status` calls `GetStatus` and prints:

      ```
              daemon: ok (version 0.1.0, socket /run/user/1000/jjw/daemon.sock)
              service localhost:7470: SERVING
              ```

              For an `UNREACHABLE` service, the second line is
              `service <addr>: UNREACHABLE (<service_error>)`. The command exits 0 only when the service is
              `SERVING`. Otherwise it prints both lines and then returns user error
              `workspace service is <status>` (exit 1). An unreachable daemon is the user error above
              (exit 1).

- [x] `block_repo_creation.rs`: the dispatch hook checks `command_helper.matches()`. For `git init`,
      `git clone` or `debug init-simple`, it returns user error
      `jjw can't create repositories yet` before anything touches disk. Otherwise it calls the inner
      dispatch.

Tests (`cargo test -p jjw`):

- [x] Unit test: the socket path resolves from config first, then from `XDG_RUNTIME_DIR`.
- [x] Unit test: `GetStatusResponse` formats into the expected lines and exit outcome for each
      `ServiceStatus`.

Done when the tests pass and `jjw log` in a stock jj repo still works, i.e. the hook doesn't break
other commands.

## 4. Harness and end-to-end tests

Outcome: `task e2e` is the one command that builds the three binaries and runs `jjw` against a real
daemon and service.

- [ ] `task e2e` sets `JJW_BIN`, `JJW_DAEMON_BIN` and `JJW_SERVICE_BIN` to the built binaries in
      `target/debug/`. The tests panic with `run via task e2e` if any is unset.
- [ ] `e2e` crate (dev-only, `publish = false`), with `insta` and `tempfile`:
  - `Service::start()` spawns `jjw-workspace-service --listen 127.0.0.1:0` and reads the
    `listening on` line from stdout with a 10 s bound.
  - `Daemon::start(service_addr, timeout)` spawns `jjw-daemon` with its socket in a short tempdir
    under `/tmp`, because Unix socket paths are limited to 108 bytes. It reads its `listening on`
    line.
  - `TestEnv::jjw(args)` runs `jjw` with an isolated `HOME` and `JJ_CONFIG`, fixed `JJ_USER` and
    `JJ_EMAIL`, `--color=never`, and `--config cws.daemon-socket=<socket>`. It returns exit code,
    stdout and stderr.
  - Every child process is killed on `Drop`.
  - Insta filters replace socket paths, addresses and versions with placeholders.
- [ ] Tests:
  - [ ] `status_all_up`: exit 0, both lines snapshotted.
  - [ ] `status_service_down`: kill the service after the daemon starts, then run status:
        `UNREACHABLE`, exit 1.
  - [ ] `status_service_hanging`: the daemon points at a test TCP listener that never responds, with
        `--service-timeout 1s`. Status reports `UNREACHABLE` with a deadline error, exit 1, in under
        3 s.
  - [ ] `status_daemon_down`: no daemon is running. The `can't reach jjw-daemon` error and hint,
        exit 1.
  - [ ] `repo_creation_blocked`: `git init`, `git clone <path>` and `debug init-simple` each exit 1
        with the error, and no `.jj` directory is created.
  - [ ] `version`: `jjw --version`, `jjw-daemon --version` and `jjw-workspace-service --version` all
        report the same project version.

Done when `task e2e` passes locally and in CI.

## 5. Spec and roadmap updates

After the code is in:

- [x] [daemon.md](../spec/daemon.md):
  - Add `Daemon | GetStatus | diagnostics` to the API table.
  - Document flags, socket handling and graceful shutdown.
  - Note that RPCs are added per milestone.
- [x] [workspace-service.md](../spec/workspace-service.md): health, `--listen` and the
      `listening on` line.
- [x] [jj-cli.md](../spec/jj-cli.md):
  - The binary is `jjw` and custom commands go under `jjw cws`.
  - The `cws.daemon-socket` config key.
  - `jjw cws status`, the repo creation block, and the version string.
- [ ] [DESIGN.md](../spec/DESIGN.md): binary names in Components, one project version, and the
      plaintext daemon-to-service transport.
- [ ] Mark M1 and this plan done in the [roadmap](mvp-impl-roadmap.md).

## Open questions

None blocking M1.
