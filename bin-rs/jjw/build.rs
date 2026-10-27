use std::fs;

const PROTO_ROOT: &str = "../../proto";
const DAEMON_PROTO: &str = "jjcw/daemon/v1/daemon.proto";
const CARGO_LOCK: &str = "../../Cargo.lock";

fn main() -> Result<(), Box<dyn std::error::Error>> {
    println!("cargo:rerun-if-changed={PROTO_ROOT}");
    println!("cargo:rerun-if-changed={CARGO_LOCK}");

    let fds = protox::compile([DAEMON_PROTO], [PROTO_ROOT])?;
    tonic_prost_build::configure()
        .build_server(false)
        .compile_fds(fds)?;

    let lock = fs::read_to_string(CARGO_LOCK)?;
    let version = jj_lib_version(&lock).ok_or("jj-lib not found in Cargo.lock")?;
    println!("cargo:rustc-env=JJ_LIB_VERSION={version}");
    Ok(())
}

/// Returns the version of the `jj-lib` package in a `Cargo.lock`.
fn jj_lib_version(lock: &str) -> Option<&str> {
    let mut lines = lock
        .lines()
        .skip_while(|line| *line != r#"name = "jj-lib""#);
    lines.next()?;
    lines
        .next()?
        .strip_prefix(r#"version = ""#)?
        .strip_suffix('"')
}
