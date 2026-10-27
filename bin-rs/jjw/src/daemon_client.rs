//! gRPC client for `jjw-daemon` over its Unix socket.

use std::error::Error;
use std::ffi::OsString;
use std::path::Path;
use std::path::PathBuf;
use std::time::Duration;

use hyper_util::rt::TokioIo;
use jj_cli::command_error::CommandError;
use jj_cli::command_error::user_error;
use jj_lib::config::ConfigGetResultExt as _;
use jj_lib::config::StackedConfig;
use jj_lib::settings::UserSettings;
use tokio::net::UnixStream;
use tokio::runtime::Runtime;
use tonic::transport::Channel;
use tonic::transport::Endpoint;
use tonic::transport::Uri;
use tower::service_fn;

use crate::proto::GetStatusRequest;
use crate::proto::GetStatusResponse;
use crate::proto::daemon_client;

/// Config key for the daemon's socket path.
const SOCKET_CONFIG_KEY: &str = "cws.daemon-socket";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
/// Deadline for each RPC. It is longer than the daemon's own timeout for calls to the service, so
/// the daemon reports a slow service instead of the CLI timing out.
const RPC_TIMEOUT: Duration = Duration::from_secs(30);

/// A connection to `jjw-daemon`.
///
/// jj runs commands on `pollster`, not tokio, so the client owns a current-thread tokio runtime and
/// blocks on each call.
pub struct DaemonClient {
    runtime: Runtime,
    socket: PathBuf,
    daemon: daemon_client::DaemonClient<Channel>,
}

impl DaemonClient {
    /// Connects to the daemon at the socket configured by `cws.daemon-socket`, or at
    /// `$XDG_RUNTIME_DIR/jjw/daemon.sock`.
    pub fn connect(settings: &UserSettings) -> Result<Self, CommandError> {
        let socket = socket_path(settings.config(), std::env::var_os("XDG_RUNTIME_DIR"))?;
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()?;
        let channel = runtime
            .block_on(connect_channel(socket.clone()))
            .map_err(|err| unreachable_error(&socket, &*err))?;
        Ok(Self {
            runtime,
            socket,
            daemon: daemon_client::DaemonClient::new(channel),
        })
    }

    pub fn socket(&self) -> &Path {
        &self.socket
    }

    pub fn get_status(&mut self) -> Result<GetStatusResponse, CommandError> {
        self.runtime
            .block_on(self.daemon.get_status(GetStatusRequest {}))
            .map(tonic::Response::into_inner)
            .map_err(|status| self.rpc_error(status))
    }

    fn rpc_error(&self, status: tonic::Status) -> CommandError {
        match status.code() {
            tonic::Code::Unavailable => unreachable_error(&self.socket, &status),
            _ => user_error(format!(
                "jjw-daemon at {} failed: {}",
                self.socket.display(),
                status.message()
            )),
        }
    }
}

async fn connect_channel(socket: PathBuf) -> Result<Channel, Box<dyn Error + Send + Sync>> {
    // The URI is required by tonic but unused: the connector always dials the socket.
    let channel = Endpoint::from_static("http://[::]:0")
        .connect_timeout(CONNECT_TIMEOUT)
        .timeout(RPC_TIMEOUT)
        .connect_with_connector(service_fn(move |_: Uri| {
            let socket = socket.clone();
            async move { Ok::<_, std::io::Error>(TokioIo::new(UnixStream::connect(socket).await?)) }
        }))
        .await?;
    Ok(channel)
}

fn unreachable_error(socket: &Path, err: &(dyn Error + 'static)) -> CommandError {
    user_error(format!(
        "can't reach jjw-daemon at {}: {}",
        socket.display(),
        root_cause(err)
    ))
    .hinted("Start it with: jjw-daemon --service-addr <host:port>")
}

/// Returns the innermost error, which names the actual problem (e.g. "Connection refused") rather
/// than tonic's generic "transport error".
fn root_cause<'a>(mut err: &'a (dyn Error + 'static)) -> &'a (dyn Error + 'static) {
    while let Some(source) = err.source() {
        err = source;
    }
    err
}

/// Resolves the daemon socket path from config, falling back to `$XDG_RUNTIME_DIR`.
fn socket_path(
    config: &StackedConfig,
    xdg_runtime_dir: Option<OsString>,
) -> Result<PathBuf, CommandError> {
    if let Some(path) = config.get::<PathBuf>(SOCKET_CONFIG_KEY).optional()? {
        return Ok(path);
    }
    match xdg_runtime_dir {
        Some(dir) if !dir.is_empty() => Ok(Path::new(&dir).join("jjw").join("daemon.sock")),
        _ => Err(
            user_error("can't find the jjw-daemon socket: XDG_RUNTIME_DIR is not set").hinted(
                format!("Set the socket path with: --config {SOCKET_CONFIG_KEY}=<path>"),
            ),
        ),
    }
}

#[cfg(test)]
mod tests {
    use jj_lib::config::ConfigLayer;
    use jj_lib::config::ConfigSource;

    use super::*;

    fn config(text: &str) -> StackedConfig {
        let mut config = StackedConfig::empty();
        config.add_layer(ConfigLayer::parse(ConfigSource::User, text).unwrap());
        config
    }

    #[test]
    fn socket_path_from_config() {
        let config = config(r#"cws.daemon-socket = "/tmp/d.sock""#);
        let path = socket_path(&config, Some("/run/user/1000".into())).unwrap();
        assert_eq!(path, Path::new("/tmp/d.sock"));
    }

    #[test]
    fn socket_path_from_xdg_runtime_dir() {
        let path = socket_path(&config(""), Some("/run/user/1000".into())).unwrap();
        assert_eq!(path, Path::new("/run/user/1000/jjw/daemon.sock"));
    }

    #[test]
    fn socket_path_missing() {
        assert!(socket_path(&config(""), None).is_err());
        assert!(socket_path(&config(""), Some("".into())).is_err());
    }
}
