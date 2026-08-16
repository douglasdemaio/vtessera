//! No-socket invariant (docs/CONSENT.md §1.8, BUILD.md hard rule 5):
//! the default `vtesserad` build opens no listening sockets. We snapshot
//! the network namespace's LISTEN entries before spawning the daemon and
//! assert that no new one appears while it runs. Because the child shares
//! this test's network namespace, `/proc/self/net/tcp*` (in either process)
//! shows the whole namespace — so a diff of LISTEN entries is exact.

use std::collections::HashSet;
use std::fs;
use std::process::{Command, Stdio};
use std::time::Duration;

/// `"<file>:<local_address>"` for every socket in LISTEN state (st `0A`)
/// in the current network namespace.
fn listeners() -> HashSet<String> {
    let mut out = HashSet::new();
    for f in ["/proc/self/net/tcp", "/proc/self/net/tcp6"] {
        let Ok(raw) = fs::read_to_string(f) else {
            continue;
        };
        for line in raw.lines().skip(1) {
            let fields: Vec<&str> = line.split_whitespace().collect();
            if fields.len() > 3 && fields[3] == "0A" {
                out.insert(format!("{f}:{}", fields[1]));
            }
        }
    }
    out
}

#[test]
fn vtesserad_default_build_opens_no_sockets() {
    let dir = std::env::temp_dir().join(format!("vtesserad_no_socket_{}", std::process::id()));
    let _ = fs::remove_dir_all(&dir);
    fs::create_dir_all(&dir).expect("create temp dir");

    let state = dir.join("state");
    fs::create_dir_all(&state).expect("create state dir");

    // A minimal free-mode config: free mode needs no payout address, and a
    // long sample interval keeps the daemon's window loop alive while the
    // test watches the namespace.
    let cfg = dir.join("vtessera.toml");
    fs::write(
        &cfg,
        format!(
            "sample_interval_secs = 3600\nstate_dir = \"{}\"\nkey_path = \"{}\"\n\
             mode = \"free\"\nresource_caps = {{ max_cpus = 1 }}\n",
            state.display(),
            dir.join("identity.key").display(),
        ),
    )
    .expect("write config");

    let before = listeners();

    let mut child = Command::new(env!("CARGO_BIN_EXE_vtesserad"))
        .arg("--config")
        .arg(&cfg)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .expect("spawn vtesserad");

    std::thread::sleep(Duration::from_secs(2));

    assert!(
        child.try_wait().expect("try_wait").is_none(),
        "vtesserad exited during the test — the no-socket check was moot"
    );

    let after = listeners();
    let new_listeners: Vec<&String> = after.difference(&before).collect();

    child.kill().ok();
    let _ = child.wait();
    let _ = fs::remove_dir_all(&dir);

    assert!(
        new_listeners.is_empty(),
        "vtesserad opened listening socket(s): {new_listeners:?}"
    );
}
