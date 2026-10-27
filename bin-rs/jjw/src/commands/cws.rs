mod status;

use jj_cli::cli_util::CommandHelper;
use jj_cli::command_error::CommandError;
use jj_cli::ui::Ui;

/// Commands for cloud workspaces
#[derive(clap::Subcommand, Clone, Debug)]
pub enum CwsCommand {
    Status(status::StatusArgs),
}

pub fn run(
    ui: &mut Ui,
    command_helper: &CommandHelper,
    command: CwsCommand,
) -> Result<(), CommandError> {
    match command {
        CwsCommand::Status(args) => status::run(ui, command_helper, &args),
    }
}
