//! End-to-end tests for jjw, jjw-daemon and jjw-workspace-service. Run with `task e2e`.
//!
//! The harness starts real `jjw-workspace-service` and `jjw-daemon` processes and runs `jjw`
//! against them. `task e2e` sets the binary paths in `JJW_BIN`, `JJW_DAEMON_BIN` and
//! `JJW_SERVICE_BIN`.

use std::fmt;
use std::fs::File;
use std::io::BufRead as _;
use std::io::BufReader;
use std::path::Path;
use std::path::PathBuf;
use std::process::Child;
use std::process::Command;
use std::process::Stdio;
use std::sync::mpsc;
use std::thread;
use std::time::Duration;

use tempfile::TempDir;

/// Bound on how long a child process may take to print its `listening on` line.
const STARTUP_TIMEOUT: Duration = Duration::from_secs(10);

/// The project version, which all three binaries report.
pub const VERSION: &str = env!("CARGO_PKG_VERSION");

pub fn jjw_bin() -> PathBuf {
    bin_from_env("JJW_BIN")
}

pub fn daemon_bin() -> PathBuf {
    bin_from_env("JJW_DAEMON_BIN")
}

pub fn service_bin() -> PathBuf {
    bin_from_env("JJW_SERVICE_BIN")
}

fn bin_from_env(var: &str) -> PathBuf {
    std::env::var_os(var)
        .unwrap_or_else(|| panic!("{var} is not set: run via task e2e"))
        .into()
}

/// A running `jjw-workspace-service`, killed on drop.
pub struct Service {
    process: Process,
    addr: String,
}

impl Service {
    /// Starts the service on a free port.
    pub fn start() -> Self {
        let mut command = Command::new(service_bin());
        command.args(["--listen", "127.0.0.1:0"]);
        let (process, addr) = Process::spawn("jjw-workspace-service", command);
        Self { process, addr }
    }

    /// The `host:port` the service listens on.
    pub fn addr(&self) -> &str {
        &self.addr
    }

    /// Kills the service and waits for it to exit.
    pub fn kill(&mut self) {
        self.process.kill();
    }
}

/// An in-process gRPC health server that reports `NOT_SERVING`, standing in for an unhealthy
/// `jjw-workspace-service`. It runs until the test process exits.
pub struct UnhealthyService {
    addr: String,
}

impl UnhealthyService {
    /// Starts the server on a free port.
    pub fn start() -> Self {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap().to_string();
        listener.set_nonblocking(true).unwrap();
        thread::spawn(move || {
            let runtime = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap();
            runtime.block_on(async {
                let (reporter, health) = tonic_health::server::health_reporter();
                reporter
                    .set_service_status("", tonic_health::ServingStatus::NotServing)
                    .await;
                let listener = tokio::net::TcpListener::from_std(listener).unwrap();
                tonic::transport::Server::builder()
                    .add_service(health)
                    .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
                    .await
                    .unwrap();
            });
        });
        Self { addr }
    }

    /// The `host:port` the server listens on.
    pub fn addr(&self) -> &str {
        &self.addr
    }
}

/// A running `jjw-daemon`, killed on drop.
pub struct Daemon {
    _process: Process,
    socket: PathBuf,
    _socket_dir: TempDir,
}

impl Daemon {
    /// Starts a daemon that talks to the service at `service_addr`, bounding each call to it by
    /// `service_timeout`.
    pub fn start(service_addr: &str, service_timeout: Duration) -> Self {
        let socket_dir = short_tempdir();
        let socket = socket_dir.path().join("daemon.sock");
        let mut command = Command::new(daemon_bin());
        command
            .arg("--socket")
            .arg(&socket)
            .args(["--service-addr", service_addr])
            .arg(format!(
                "--service-timeout={}ms",
                service_timeout.as_millis()
            ));
        let (process, listening_on) = Process::spawn("jjw-daemon", command);
        assert_eq!(Path::new(&listening_on), socket);
        Self {
            _process: process,
            socket,
            _socket_dir: socket_dir,
        }
    }

    pub fn socket(&self) -> &Path {
        &self.socket
    }
}

/// Returns a tempdir under `/tmp`, so Unix socket paths in it stay under the 108-byte limit even
/// when `TMPDIR` is long.
pub fn short_tempdir() -> TempDir {
    tempfile::Builder::new()
        .prefix("jjw-e2e-")
        .tempdir_in("/tmp")
        .unwrap()
}

/// A child process that printed `listening on <addr>`, killed on drop.
struct Process {
    child: Child,
    // Holds the stderr log until the process is gone.
    _log_dir: TempDir,
}

impl Process {
    /// Spawns `command` and waits for its `listening on <addr>` line. Returns the process and
    /// `<addr>`.
    fn spawn(name: &str, mut command: Command) -> (Self, String) {
        let log_dir = TempDir::new().unwrap();
        let log_path = log_dir.path().join("stderr.log");
        let mut child = command
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(File::create(&log_path).unwrap())
            .spawn()
            .unwrap_or_else(|err| panic!("can't start {name}: {err}"));
        let stdout = child.stdout.take().unwrap();
        let (tx, rx) = mpsc::channel();
        // Keeps draining stdout after the first line, so the process never blocks on a full pipe.
        thread::spawn(move || {
            for line in BufReader::new(stdout).lines().map_while(Result::ok) {
                let _ = tx.send(line);
            }
        });
        let process = Self {
            child,
            _log_dir: log_dir,
        };
        let line = rx.recv_timeout(STARTUP_TIMEOUT).unwrap_or_else(|err| {
            let log = std::fs::read_to_string(&log_path).unwrap_or_default();
            panic!("{name} didn't print its address ({err}). stderr:\n{log}")
        });
        let addr = line
            .strip_prefix("listening on ")
            .unwrap_or_else(|| panic!("unexpected first line from {name}: {line}"))
            .to_owned();
        (process, addr)
    }

    fn kill(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

impl Drop for Process {
    fn drop(&mut self) {
        self.kill();
    }
}

/// An isolated environment for running `jjw` against a daemon socket.
pub struct TestEnv {
    home: TempDir,
    socket: PathBuf,
}

/// The result of a `jjw` run.
#[derive(Debug)]
pub struct Output {
    pub code: i32,
    pub stdout: String,
    pub stderr: String,
}

/// Formats the output for snapshots.
impl fmt::Display for Output {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        writeln!(f, "exit code: {}", self.code)?;
        writeln!(f, "--- stdout")?;
        write!(f, "{}", self.stdout)?;
        writeln!(f, "--- stderr")?;
        write!(f, "{}", self.stderr)
    }
}

impl TestEnv {
    /// Creates an environment whose `jjw` talks to the daemon at `socket`.
    pub fn new(socket: &Path) -> Self {
        let home = TempDir::new().unwrap();
        std::fs::create_dir(home.path().join("config")).unwrap();
        std::fs::create_dir(home.path().join("work")).unwrap();
        Self {
            home,
            socket: socket.to_owned(),
        }
    }

    /// The directory `jjw` runs in.
    pub fn work_dir(&self) -> PathBuf {
        self.home.path().join("work")
    }

    /// Runs `jjw args` in [`Self::work_dir`].
    pub fn jjw(&self, args: &[&str]) -> Output {
        let output = Command::new(jjw_bin())
            .current_dir(self.work_dir())
            .env("HOME", self.home.path())
            .env("JJ_CONFIG", self.home.path().join("config"))
            .env("JJ_USER", "Test User")
            .env("JJ_EMAIL", "test.user@example.com")
            .arg("--color=never")
            .arg("--config")
            .arg(format!("cws.daemon-socket={}", self.socket.display()))
            .args(args)
            .stdin(Stdio::null())
            .output()
            .unwrap();
        Output {
            code: output.status.code().expect("jjw was killed by a signal"),
            stdout: String::from_utf8(output.stdout).unwrap(),
            stderr: String::from_utf8(output.stderr).unwrap(),
        }
    }

    /// Insta settings that replace this environment's socket path, service addresses and the
    /// project version with placeholders.
    pub fn insta_settings(&self) -> insta::Settings {
        let mut settings = insta::Settings::clone_current();
        settings.add_filter(
            &regex_escape(&self.socket.display().to_string()),
            "[SOCKET]",
        );
        settings.add_filter(r"127\.0\.0\.1:\d+", "[ADDR]");
        settings.add_filter(&format!(r"\b{}\b", regex_escape(VERSION)), "[VERSION]");
        settings
    }
}

fn regex_escape(text: &str) -> String {
    let mut escaped = String::with_capacity(text.len());
    for c in text.chars() {
        if r"\.+*?()|[]{}^$#&-~".contains(c) {
            escaped.push('\\');
        }
        escaped.push(c);
    }
    escaped
}
