mod block_repo_creation;
mod commands;
mod daemon_client;
mod proto;

use jj_cli::cli_util::CliRunner;

fn main() -> std::process::ExitCode {
    let version = concat!(
        env!("CARGO_PKG_VERSION"),
        " (jj ",
        env!("JJ_LIB_VERSION"),
        ")"
    );
    CliRunner::init()
        .name("jjw")
        .version(version)
        .add_dispatch_hook(block_repo_creation::block_repo_creation)
        .add_subcommand(commands::run_custom_command)
        .run()
        .into()
}
