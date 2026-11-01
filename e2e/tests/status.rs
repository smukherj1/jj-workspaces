//! `jjc cloud status` against a real srcd and source-api.

use std::net::TcpListener;
use std::thread;
use std::time::Duration;
use std::time::Instant;

use e2e::Daemon;
use e2e::SourceApi;
use e2e::TestEnv;
use e2e::UnhealthySourceApi;
use e2e::short_tempdir;
use insta::assert_snapshot;

const SOURCE_API_TIMEOUT: Duration = Duration::from_secs(10);

#[test]
fn status_all_up() {
    let source_api = SourceApi::start();
    let daemon = Daemon::start(source_api.addr(), SOURCE_API_TIMEOUT);
    let env = TestEnv::new(daemon.socket());

    let output = env.jjc(&["cloud", "status"]);
    env.insta_settings().bind(|| {
        assert_snapshot!(output, @r"
        exit code: 0
        --- stdout
        daemon: ok (version [VERSION], socket [SOCKET])
        source-api [ADDR]: SERVING
        --- stderr
        ");
    });
}

#[test]
fn status_source_api_down() {
    let mut source_api = SourceApi::start();
    let daemon = Daemon::start(source_api.addr(), SOURCE_API_TIMEOUT);
    source_api.kill();
    let env = TestEnv::new(daemon.socket());

    let output = env.jjc(&["cloud", "status"]);
    env.insta_settings().bind(|| {
        assert_snapshot!(output, @r#"
        exit code: 1
        --- stdout
        daemon: ok (version [VERSION], socket [SOCKET])
        source-api [ADDR]: UNREACHABLE (Unavailable: connection error: desc = "transport: Error while dialing: dial tcp [ADDR]: connect: connection refused")
        --- stderr
        Error: source-api is UNREACHABLE
        "#);
    });
}

#[test]
fn status_source_api_hanging() {
    // Accepts connections but never speaks HTTP/2.
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap().to_string();
    thread::spawn(move || {
        let _held: Vec<_> = listener.incoming().collect();
    });
    let daemon = Daemon::start(&addr, Duration::from_secs(1));
    let env = TestEnv::new(daemon.socket());

    let start = Instant::now();
    let output = env.jjc(&["cloud", "status"]);
    let elapsed = start.elapsed();
    env.insta_settings().bind(|| {
        assert_snapshot!(output, @r"
        exit code: 1
        --- stdout
        daemon: ok (version [VERSION], socket [SOCKET])
        source-api [ADDR]: UNREACHABLE (deadline exceeded after 1s)
        --- stderr
        Error: source-api is UNREACHABLE
        ");
    });
    assert!(elapsed < Duration::from_secs(3), "status took {elapsed:?}");
}

#[test]
fn status_daemon_down() {
    let socket_dir = short_tempdir();
    let env = TestEnv::new(&socket_dir.path().join("srcd.sock"));

    let output = env.jjc(&["cloud", "status"]);
    env.insta_settings().bind(|| {
        assert_snapshot!(output, @r"
        exit code: 1
        --- stdout
        --- stderr
        Error: can't reach srcd at [SOCKET]: No such file or directory (os error 2)
        Hint: Start it with: srcd --source-api-addr <host:port>
        ");
    });
}

#[test]
fn status_source_api_unhealthy() {
    let source_api = UnhealthySourceApi::start();
    let daemon = Daemon::start(source_api.addr(), SOURCE_API_TIMEOUT);
    let env = TestEnv::new(daemon.socket());

    let output = env.jjc(&["cloud", "status"]);
    env.insta_settings().bind(|| {
        assert_snapshot!(output, @r"
        exit code: 1
        --- stdout
        daemon: ok (version [VERSION], socket [SOCKET])
        source-api [ADDR]: UNHEALTHY (NOT_SERVING)
        --- stderr
        Error: source-api is UNHEALTHY
        ");
    });
}
