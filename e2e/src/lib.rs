//! End-to-end tests for jjc, srcd and source-api. Run with `task e2e`.
//!
//! The harness starts real `source-api` and `srcd` processes and runs `jjc` against them.
//! `task e2e` sets the binary paths in `JJC_BIN`, `SRCD_BIN` and `SOURCE_API_BIN`.

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

pub fn jjc_bin() -> PathBuf {
    bin_from_env("JJC_BIN")
}

pub fn srcd_bin() -> PathBuf {
    bin_from_env("SRCD_BIN")
}

pub fn source_api_bin() -> PathBuf {
    bin_from_env("SOURCE_API_BIN")
}

fn bin_from_env(var: &str) -> PathBuf {
    std::env::var_os(var)
        .unwrap_or_else(|| panic!("{var} is not set: run via task e2e"))
        .into()
}

/// A running `source-api`, killed on drop.
pub struct SourceApi {
    process: Process,
    addr: String,
    _data_dir: TempDir,
}

impl SourceApi {
    /// Starts source-api on a free port, with a new temp data dir.
    pub fn start() -> Self {
        let data_dir = TempDir::new().unwrap();
        let mut command = Command::new(source_api_bin());
        command
            .args(["--listen", "127.0.0.1:0"])
            .arg("--data-dir")
            .arg(data_dir.path());
        let (process, addr) = Process::spawn("source-api", command);
        Self {
            process,
            addr,
            _data_dir: data_dir,
        }
    }

    /// The `host:port` source-api listens on.
    pub fn addr(&self) -> &str {
        &self.addr
    }

    /// Kills source-api and waits for it to exit.
    pub fn kill(&mut self) {
        self.process.kill();
    }
}

/// An in-process gRPC health server that reports `NOT_SERVING`, standing in for an unhealthy
/// `source-api`. It runs until the test process exits.
pub struct UnhealthySourceApi {
    addr: String,
}

impl UnhealthySourceApi {
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

/// A running `srcd`, killed on drop.
pub struct Daemon {
    _process: Process,
    socket: PathBuf,
    _socket_dir: TempDir,
}

impl Daemon {
    /// Starts a daemon that talks to source-api at `source_api_addr`, bounding each call to it by
    /// `source_api_timeout`.
    pub fn start(source_api_addr: &str, source_api_timeout: Duration) -> Self {
        let socket_dir = short_tempdir();
        let socket = socket_dir.path().join("srcd.sock");
        let mut command = Command::new(srcd_bin());
        command
            .arg("--socket")
            .arg(&socket)
            .args(["--source-api-addr", source_api_addr])
            .arg(format!(
                "--source-api-timeout={}ms",
                source_api_timeout.as_millis()
            ));
        let (process, listening_on) = Process::spawn("srcd", command);
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
        .prefix("jjc-e2e-")
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

/// An isolated environment for running `jjc` against a daemon socket.
pub struct TestEnv {
    home: TempDir,
    socket: PathBuf,
}

/// The result of a `jjc` run.
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
    /// Creates an environment whose `jjc` talks to the daemon at `socket`.
    pub fn new(socket: &Path) -> Self {
        let home = TempDir::new().unwrap();
        std::fs::create_dir(home.path().join("config")).unwrap();
        std::fs::create_dir(home.path().join("work")).unwrap();
        Self {
            home,
            socket: socket.to_owned(),
        }
    }

    /// The directory `jjc` runs in.
    pub fn work_dir(&self) -> PathBuf {
        self.home.path().join("work")
    }

    /// Runs `jjc args` in [`Self::work_dir`].
    pub fn jjc(&self, args: &[&str]) -> Output {
        let output = Command::new(jjc_bin())
            .current_dir(self.work_dir())
            .env("HOME", self.home.path())
            .env("JJ_CONFIG", self.home.path().join("config"))
            .env("JJ_USER", "Test User")
            .env("JJ_EMAIL", "test.user@example.com")
            .arg("--color=never")
            .arg("--config")
            .arg(format!("cloud.daemon-socket={}", self.socket.display()))
            .args(args)
            .stdin(Stdio::null())
            .output()
            .unwrap();
        Output {
            code: output.status.code().expect("jjc was killed by a signal"),
            stdout: String::from_utf8(output.stdout).unwrap(),
            stderr: String::from_utf8(output.stderr).unwrap(),
        }
    }

    /// Insta settings that replace this environment's socket path, source-api addresses and the
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
