mod status;

use jj_cli::cli_util::CommandHelper;
use jj_cli::command_error::CommandError;
use jj_cli::ui::Ui;

/// Commands for JJ Cloud Source
#[derive(clap::Subcommand, Clone, Debug)]
pub enum CloudCommand {
    Status(status::StatusArgs),
}

pub fn run(
    ui: &mut Ui,
    command_helper: &CommandHelper,
    command: CloudCommand,
) -> Result<(), CommandError> {
    match command {
        CloudCommand::Status(args) => status::run(ui, command_helper, &args),
    }
}
