use std::io::Write as _;

use jj_cli::cli_util::CommandHelper;
use jj_cli::command_error::CommandError;
use jj_cli::command_error::user_error;
use jj_cli::ui::Ui;

use crate::daemon_client::DaemonClient;
use crate::proto::ServiceStatusCode;

/// Show whether jjw-daemon and the workspace service are up
///
/// Exits with an error unless both are up.
#[derive(clap::Args, Clone, Debug)]
pub struct StatusArgs {}

pub fn run(
    ui: &mut Ui,
    command_helper: &CommandHelper,
    _args: &StatusArgs,
) -> Result<(), CommandError> {
    let mut client = DaemonClient::connect(command_helper.settings())?;
    let response = client.get_status()?;
    writeln!(
        ui.stdout(),
        "daemon: ok (version {}, socket {})",
        response.daemon_version,
        client.socket().display()
    )?;
    let status = response.service_status.unwrap_or_default();
    let name = match status.code() {
        ServiceStatusCode::Ok => "OK",
        ServiceStatusCode::Unhealthy => "UNHEALTHY",
        ServiceStatusCode::Unreachable => "UNREACHABLE",
        ServiceStatusCode::Unspecified => "UNKNOWN",
    };
    write!(ui.stdout(), "service {}: {name}", response.service_address)?;
    if !status.details.is_empty() {
        write!(ui.stdout(), " ({})", status.details)?;
    }
    writeln!(ui.stdout())?;
    if status.code() != ServiceStatusCode::Ok {
        return Err(user_error(format!("workspace service is {name}")));
    }
    Ok(())
}
