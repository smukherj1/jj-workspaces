mod cws;

use jj_cli::cli_util::CommandHelper;
use jj_cli::command_error::CommandError;
use jj_cli::ui::Ui;

/// Subcommands `jjw` adds to stock jj.
#[derive(clap::Subcommand, Clone, Debug)]
pub enum CustomCommand {
    #[command(subcommand)]
    Cws(cws::CwsCommand),
}

pub async fn run_custom_command(
    ui: &mut Ui,
    command_helper: &CommandHelper,
    command: CustomCommand,
) -> Result<(), CommandError> {
    match command {
        CustomCommand::Cws(command) => cws::run(ui, command_helper, command),
    }
}
