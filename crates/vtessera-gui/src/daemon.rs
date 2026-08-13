//! Child-process management for the GUI.
//!
//! Starting the node = two long-running binaries:
//!
//! * `vtesserad` — metering daemon (samples usage, writes receipts).
//! * `vtessera-node` — agent-facing HTTP server (offer + jobs + 402/x402).
//!
//! Both are shipped in the Flatpak (`/app/bin`) and looked up on `PATH`.
//! `VTESSERA_BIN_DIR` overrides the lookup for local (non-Flatpak) runs.

use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};

/// Rust's std doesn't expose `killpg`/process groups portably; `Child::kill`
/// sends SIGKILL to the direct child only. For v0 that's fine — killing the
/// daemon stops it writing receipts, killing the node stops it serving. We
/// drop `stdin` so children never block on a tty, and pipe stdout/stderr to
/// the GUI's log sink.
pub struct Daemons {
    pub meter: Child,
    /// `None` when an already-running node was reused (see [`start`]).
    pub node: Option<Child>,
    /// True when the node wasn't spawned because one was already serving.
    pub node_reused: bool,
}

/// Options for [`start`].
pub struct StartOptions<'a> {
    /// Explicit binary dir (e.g. `target/release`); falls back to the
    /// `VTESSERA_BIN_DIR` env var, then `PATH`.
    pub bin_dir: Option<&'a Path>,
    pub daemon_config: &'a Path,
    pub offer: &'a Path,
    pub bind: String,
    pub escrow: &'a str,
    pub network: &'a str,
}

/// Directory holding the built binaries, when set (local runs). In the
/// Flatpak the binaries are on `PATH`, so `None` is the normal case.
pub fn bin_dir() -> Option<PathBuf> {
    std::env::var_os("VTESSERA_BIN_DIR").map(Into::into)
}

fn build_command(bin: &str, extra: Option<&Path>) -> Command {
    let mut cmd = match extra {
        Some(dir) => Command::new(dir.join(bin)),
        None => Command::new(bin),
    };
    cmd.stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    cmd
}

/// Probe `GET /healthz` on `host:port`. Returns true when an HTTP server
/// answers 200 — used to detect an already-running `vtessera-node` before
/// spawning a second one (which would fail to bind and exit immediately).
fn healthz_up(host: &str, port: u16) -> bool {
    use std::io::{Read, Write};
    use std::net::TcpStream;
    use std::time::Duration;

    let Ok(mut stream) = TcpStream::connect((host, port)) else {
        return false;
    };
    let _ = stream.set_read_timeout(Some(Duration::from_secs(2)));
    let _ = stream.set_write_timeout(Some(Duration::from_secs(2)));
    let req = format!("GET /healthz HTTP/1.1\r\nhost: {host}\r\nconnection: close\r\n\r\n");
    if stream.write_all(req.as_bytes()).is_err() {
        return false;
    }
    let mut buf = [0u8; 32];
    let Ok(n) = stream.read(&mut buf) else {
        return false;
    };
    buf[..n].starts_with(b"HTTP/1.1 200")
}

/// Spawn both daemons. Caller must already have written the config + offer.
pub fn start(opts: &StartOptions) -> Result<Daemons, String> {
    let env_dir = bin_dir();
    let extra: Option<&Path> = match (opts.bin_dir, env_dir.as_deref()) {
        (Some(dir), _) => Some(dir),
        (None, Some(dir)) => Some(dir),
        (None, None) => None,
    };

    let mut meter_cmd = build_command("vtesserad", extra);
    meter_cmd
        .arg("--config")
        .arg(opts.daemon_config.as_os_str());
    let meter = meter_cmd
        .spawn()
        .map_err(|e| format!("spawn vtesserad: {e}"))?;

    // If a node is already answering on the configured port (e.g. leaked by
    // a hard-killed previous session), reusing it beats spawning a second
    // one that dies on bind. The served offer may be stale — the caller
    // surfaces that to the user.
    let port = opts
        .bind
        .rsplit_once(':')
        .and_then(|(_, p)| p.parse::<u16>().ok())
        .unwrap_or(0);
    let node_reused = port != 0 && healthz_up("127.0.0.1", port);

    let node = if node_reused {
        None
    } else {
        let mut node_cmd = build_command("vtessera-node", extra);
        node_cmd
            .args(["--bind", &opts.bind])
            .args(["--offer", opts.offer.to_str().unwrap_or_default()])
            .args(["--escrow", opts.escrow])
            .args(["--network", opts.network]);
        Some(
            node_cmd
                .spawn()
                .map_err(|e| format!("spawn vtessera-node: {e}"))?,
        )
    };

    Ok(Daemons {
        meter,
        node,
        node_reused,
    })
}

/// Pipe each child's stdout/stderr lines to `on_line`, called on worker
/// threads (which each own a clone of the callback). Threads exit when the
/// corresponding stream closes.
pub fn pump_output<F>(daemons: &mut Daemons, on_line: F)
where
    F: Fn(String) + Send + Clone + 'static,
{
    if let Some(stdout) = daemons.meter.stdout.take() {
        spawn_reader(stdout, "vtesserad", on_line.clone());
    }
    if let Some(stderr) = daemons.meter.stderr.take() {
        spawn_reader(stderr, "vtesserad", on_line.clone());
    }
    if let Some(node) = daemons.node.as_mut() {
        if let Some(stdout) = node.stdout.take() {
            spawn_reader(stdout, "vtessera-node", on_line.clone());
        }
        if let Some(stderr) = node.stderr.take() {
            spawn_reader(stderr, "vtessera-node", on_line);
        }
    }
}

fn spawn_reader<R, F>(stream: R, tag: &'static str, on_line: F)
where
    R: std::io::Read + Send + 'static,
    F: Fn(String) + Send + Clone + 'static,
{
    std::thread::spawn(move || {
        let reader = BufReader::new(stream);
        for line in reader.lines() {
            match line {
                Ok(l) => on_line(format!("[{tag}] {l}")),
                Err(_) => break,
            }
        }
    });
}

/// Kill both children and reap them. Synchronous; call on the main thread.
pub fn stop(daemons: &mut Daemons, on_line: &impl Fn(String)) {
    let mut killed = false;
    if let Err(e) = daemons.meter.kill() {
        if e.kind() != std::io::ErrorKind::InvalidInput {
            on_line(format!("kill: {e}"));
        }
    } else {
        killed = true;
    }
    let _ = daemons.meter.wait();
    if let Some(node) = daemons.node.as_mut() {
        if let Err(e) = node.kill() {
            if e.kind() != std::io::ErrorKind::InvalidInput {
                on_line(format!("kill: {e}"));
            }
        } else {
            killed = true;
        }
        let _ = node.wait();
    }
    if killed {
        on_line("node stopped".into());
    }
}
