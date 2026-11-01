use std::io::Write as _;

use jj_cli::cli_util::CommandHelper;
use jj_cli::command_error::CommandError;
use jj_cli::command_error::user_error;
use jj_cli::ui::Ui;

use crate::daemon_client::DaemonClient;
use crate::proto::SourceApiStatusCode;

/// Show whether srcd and source-api are up
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
    let status = response.source_api_status.unwrap_or_default();
    let name = match status.code() {
        SourceApiStatusCode::Ok => "SERVING",
        SourceApiStatusCode::Unhealthy => "UNHEALTHY",
        SourceApiStatusCode::Unreachable => "UNREACHABLE",
        SourceApiStatusCode::Unspecified => "UNKNOWN",
    };
    let mut stdout = ui.stdout();
    writeln!(
        stdout,
        "daemon: ok (version {}, socket {})",
        response.daemon_version,
        client.socket().display()
    )?;
    write!(stdout, "source-api {}: {name}", response.source_api_address)?;
    if !status.details.is_empty() {
        write!(stdout, " ({})", status.details)?;
    }
    writeln!(stdout)?;
    if status.code() != SourceApiStatusCode::Ok {
        return Err(user_error(format!("source-api is {name}")));
    }
    Ok(())
}
