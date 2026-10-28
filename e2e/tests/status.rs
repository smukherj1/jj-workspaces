//! `jjw cws status` against a real daemon and service.

use std::net::TcpListener;
use std::thread;
use std::time::Duration;
use std::time::Instant;

use e2e::Daemon;
use e2e::Service;
use e2e::TestEnv;
use e2e::UnhealthyService;
use e2e::short_tempdir;
use insta::assert_snapshot;

const SERVICE_TIMEOUT: Duration = Duration::from_secs(10);

#[test]
fn status_all_up() {
    let service = Service::start();
    let daemon = Daemon::start(service.addr(), SERVICE_TIMEOUT);
    let env = TestEnv::new(daemon.socket());

    let output = env.jjw(&["cws", "status"]);
    env.insta_settings().bind(|| {
        assert_snapshot!(output, @r"
        exit code: 0
        --- stdout
        daemon: ok (version [VERSION], socket [SOCKET])
        service [ADDR]: SERVING
        --- stderr
        ");
    });
}

#[test]
fn status_service_down() {
    let mut service = Service::start();
    let daemon = Daemon::start(service.addr(), SERVICE_TIMEOUT);
    service.kill();
    let env = TestEnv::new(daemon.socket());

    let output = env.jjw(&["cws", "status"]);
    env.insta_settings().bind(|| {
        assert_snapshot!(output, @r#"
        exit code: 1
        --- stdout
        daemon: ok (version [VERSION], socket [SOCKET])
        service [ADDR]: UNREACHABLE (Unavailable: connection error: desc = "transport: Error while dialing: dial tcp [ADDR]: connect: connection refused")
        --- stderr
        Error: workspace service is UNREACHABLE
        "#);
    });
}

#[test]
fn status_service_hanging() {
    // Accepts connections but never speaks HTTP/2.
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap().to_string();
    thread::spawn(move || {
        let _held: Vec<_> = listener.incoming().collect();
    });
    let daemon = Daemon::start(&addr, Duration::from_secs(1));
    let env = TestEnv::new(daemon.socket());

    let start = Instant::now();
    let output = env.jjw(&["cws", "status"]);
    let elapsed = start.elapsed();
    env.insta_settings().bind(|| {
        assert_snapshot!(output, @r"
        exit code: 1
        --- stdout
        daemon: ok (version [VERSION], socket [SOCKET])
        service [ADDR]: UNREACHABLE (deadline exceeded after 1s)
        --- stderr
        Error: workspace service is UNREACHABLE
        ");
    });
    assert!(elapsed < Duration::from_secs(3), "status took {elapsed:?}");
}

#[test]
fn status_daemon_down() {
    let socket_dir = short_tempdir();
    let env = TestEnv::new(&socket_dir.path().join("daemon.sock"));

    let output = env.jjw(&["cws", "status"]);
    env.insta_settings().bind(|| {
        assert_snapshot!(output, @r"
        exit code: 1
        --- stdout
        --- stderr
        Error: can't reach jjw-daemon at [SOCKET]: No such file or directory (os error 2)
        Hint: Start it with: jjw-daemon --service-addr <host:port>
        ");
    });
}

#[test]
fn status_service_unhealthy() {
    let service = UnhealthyService::start();
    let daemon = Daemon::start(service.addr(), SERVICE_TIMEOUT);
    let env = TestEnv::new(daemon.socket());

    let output = env.jjw(&["cws", "status"]);
    env.insta_settings().bind(|| {
        assert_snapshot!(output, @r"
        exit code: 1
        --- stdout
        daemon: ok (version [VERSION], socket [SOCKET])
        service [ADDR]: UNHEALTHY (NOT_SERVING)
        --- stderr
        Error: workspace service is UNHEALTHY
        ");
    });
}
