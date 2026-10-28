//! `jjw` commands that don't need a daemon.

use std::process::Command;

use e2e::TestEnv;
use e2e::VERSION;
use e2e::daemon_bin;
use e2e::service_bin;
use e2e::short_tempdir;
use insta::assert_snapshot;

#[test]
fn repo_creation_blocked() {
    let socket_dir = short_tempdir();
    let env = TestEnv::new(&socket_dir.path().join("daemon.sock"));
    let source = tempfile::TempDir::new().unwrap();
    let source = source.path().to_str().unwrap();

    for args in [
        &["git", "init"][..],
        &["git", "init", "repo"],
        &["git", "clone", source, "clone"],
        &["debug", "init-simple"],
    ] {
        let output = env.jjw(args);
        insta::allow_duplicates! {
            assert_snapshot!(output, @r"
            exit code: 1
            --- stdout
            --- stderr
            Error: jjw can't create repositories yet
            ");
        }
    }
    let entries: Vec<_> = std::fs::read_dir(env.work_dir()).unwrap().collect();
    assert!(entries.is_empty(), "work dir isn't empty: {entries:?}");
}

#[test]
fn version() {
    let socket_dir = short_tempdir();
    let env = TestEnv::new(&socket_dir.path().join("daemon.sock"));

    let output = env.jjw(&["--version"]);
    assert_eq!(output.code, 0, "{output}");
    // The jj version is checked by jjw's own build, which reads it from Cargo.lock.
    let jj_version = output
        .stdout
        .strip_prefix(&format!("jjw {VERSION} (jj "))
        .and_then(|rest| rest.strip_suffix(")\n"));
    assert!(jj_version.is_some(), "{output}");

    for (name, bin) in [
        ("jjw-daemon", daemon_bin()),
        ("jjw-workspace-service", service_bin()),
    ] {
        let output = Command::new(bin).arg("--version").output().unwrap();
        assert!(output.status.success(), "{name} --version failed");
        assert_eq!(
            String::from_utf8(output.stdout).unwrap(),
            format!("{name} {VERSION}\n")
        );
    }
}
