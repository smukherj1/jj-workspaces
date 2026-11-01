mod cloud;

use jj_cli::cli_util::CommandHelper;
use jj_cli::command_error::CommandError;
use jj_cli::ui::Ui;

/// Subcommands `jjc` adds to stock jj.
#[derive(clap::Subcommand, Clone, Debug)]
pub enum CustomCommand {
    #[command(subcommand)]
    Cloud(cloud::CloudCommand),
}

pub async fn run_custom_command(
    ui: &mut Ui,
    command_helper: &CommandHelper,
    command: CustomCommand,
) -> Result<(), CommandError> {
    match command {
        CustomCommand::Cloud(command) => cloud::run(ui, command_helper, command),
    }
}
