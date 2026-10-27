use clap::ArgMatches;
use jj_cli::cli_util::CommandHelper;
use jj_cli::command_error::CommandError;
use jj_cli::command_error::user_error;
use jj_cli::ui::Ui;

/// Commands that create a repo, as paths of subcommand names.
const REPO_CREATION_COMMANDS: &[&[&str]] = &[
    &["git", "init"],
    &["git", "clone"],
    &["debug", "init-simple"],
];

/// Dispatch hook that rejects the stock repo creation commands before they touch disk, because
/// `jjw` can't create cloud repos yet.
pub async fn block_repo_creation(
    ui: &mut Ui,
    command_helper: &CommandHelper,
    dispatch: jj_cli::cli_util::BoxedAsyncCliDispatch<'_>,
) -> Result<(), CommandError> {
    if creates_repo(command_helper.matches()) {
        return Err(user_error("jjw can't create repositories yet"));
    }
    dispatch.call(ui, command_helper).await
}

fn creates_repo(matches: &ArgMatches) -> bool {
    let path = subcommand_path(matches);
    REPO_CREATION_COMMANDS
        .iter()
        .any(|command| path.starts_with(command))
}

fn subcommand_path(mut matches: &ArgMatches) -> Vec<&str> {
    let mut path = vec![];
    while let Some((name, sub_matches)) = matches.subcommand() {
        path.push(name);
        matches = sub_matches;
    }
    path
}
