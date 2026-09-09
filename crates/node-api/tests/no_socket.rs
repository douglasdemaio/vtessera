//! Node-level no-socket invariant (design `zero-config-connectivity.md`
//! P1.3, test 6a-1): `vtessera-node --connectivity outbound-only` opens no
//! listening sockets. Mirrors `crates/vtesserad/tests/no_socket.rs` — a diff
//! of LISTEN entries in this network namespace, because the child shares it.
//!
//! The `vtessera-node` binary (and the iroh/executor deps it links) only
//! exist behind the `serve` feature, so this test is compiled only when CI
//! runs `cargo test -p vtessera-node-api --features serve`.

#![cfg(feature = "serve")]

use std::collections::HashSet;
use std::fs;
use std::process::{Command, Stdio};
use std::sync::{Mutex, MutexGuard};
use std::time::Duration;

/// The two tests below share this process's network namespace, and
/// `listeners()` sees every socket in it. Cargo runs tests in threads, so
/// the namespace-visible diff must not overlap between tests.
static NAMESPACE_LOCK: Mutex<()> = Mutex::new(());

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

fn write_matching_key_and_offer(dir: &std::path::Path) {
    // Deterministic key so the test is reproducible; raw 32-byte seed is what
    // load_node_key expects, mode 0600.
    let key = ed25519_dalek::SigningKey::from_bytes(&[0x42; 32]);
    let key_path = dir.join("identity.key");
    fs::write(&key_path, key.to_bytes()).expect("write identity key");
    let mut perms = fs::metadata(&key_path).expect("key metadata").permissions();
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        perms.set_mode(0o600);
    }
    fs::set_permissions(&key_path, perms).expect("chmod 0600 identity key");

    let node_id = vtessera_offer::derive_node_id(&key.verifying_key().to_bytes());
    let body = vtessera_offer::OfferBody {
        schema_ver: vtessera_offer::OFFER_SCHEMA_VER,
        node_id,
        endpoint_id: hex::encode(key.verifying_key().to_bytes()),
        endpoint: vec!["http://127.0.0.1:8402".into()],
        device: vtessera_offer::AdvertisedDevice::Cpu {
            vcpus: 2,
            mem_mb: 4096,
        },
        price: vtessera_offer::PriceQuote::Free,
        issued_unix: 1_700_000_000,
        expires_unix: 1_700_777_777,
    };
    let signed = vtessera_offer::sign(body, &key);
    fs::write(dir.join("offer.json"), vtessera_offer::to_json(&signed)).expect("write offer");
}

#[test]
fn vtessera_node_outbound_only_opens_no_sockets() {
    let _guard: MutexGuard<()> = NAMESPACE_LOCK.lock().unwrap();
    let dir = std::env::temp_dir().join(format!("vtessera_node_no_socket_{}", std::process::id()));
    let _ = fs::remove_dir_all(&dir);
    fs::create_dir_all(&dir).expect("create temp dir");
    let state = dir.join("state");
    fs::create_dir_all(&state).expect("create state dir");
    write_matching_key_and_offer(&dir);

    let before = listeners();

    let mut child = Command::new(env!("CARGO_BIN_EXE_vtessera-node"))
        .arg("--bind")
        .arg("127.0.0.1:1")
        .arg("--offer")
        .arg(dir.join("offer.json"))
        .arg("--escrow")
        .arg("escrow")
        .arg("--network")
        .arg("solana-devnet")
        .arg("--key")
        .arg(dir.join("identity.key"))
        .arg("--state-dir")
        .arg(&state)
        .arg("--connectivity")
        .arg("outbound-only")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .expect("spawn vtessera-node");

    std::thread::sleep(Duration::from_secs(2));

    assert!(
        child.try_wait().expect("try_wait").is_none(),
        "vtessera-node exited during the test — the no-socket check was moot"
    );

    let after = listeners();
    let new_listeners: Vec<&String> = after.difference(&before).collect();

    child.kill().ok();
    let _ = child.wait();
    let _ = fs::remove_dir_all(&dir);

    assert!(
        new_listeners.is_empty(),
        "vtessera-node outbound-only opened listening socket(s): {new_listeners:?}"
    );
}

/// Flag matrix (design P1.3, test 6a-4): `inbound+dialable` keeps a
/// listener, `outbound-only` skips it — verified end-to-end through the
/// built binary, not just the parser.
#[test]
fn vtessera_node_inbound_dialable_opens_listener() {
    let _guard: MutexGuard<()> = NAMESPACE_LOCK.lock().unwrap();
    let dir = std::env::temp_dir().join(format!(
        "vtessera_node_inbound_socket_{}",
        std::process::id()
    ));
    let _ = fs::remove_dir_all(&dir);
    fs::create_dir_all(&dir).expect("create temp dir");
    let state = dir.join("state");
    fs::create_dir_all(&state).expect("create state dir");
    write_matching_key_and_offer(&dir);

    let before = listeners();

    let port = 39000 + (std::process::id() as u16 % 1000);
    let bind = format!("127.0.0.1:{port}");
    let mut child = Command::new(env!("CARGO_BIN_EXE_vtessera-node"))
        .arg("--bind")
        .arg(&bind)
        .arg("--offer")
        .arg(dir.join("offer.json"))
        .arg("--escrow")
        .arg("escrow")
        .arg("--network")
        .arg("solana-devnet")
        .arg("--key")
        .arg(dir.join("identity.key"))
        .arg("--state-dir")
        .arg(&state)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .expect("spawn vtessera-node");

    std::thread::sleep(Duration::from_secs(2));

    assert!(
        child.try_wait().expect("try_wait").is_none(),
        "vtessera-node exited during the inbound-listener check"
    );

    let after = listeners();
    let new_listeners: Vec<String> = after.difference(&before).cloned().collect();

    child.kill().ok();
    let _ = child.wait();
    let _ = fs::remove_dir_all(&dir);

    // We expect exactly one new TCP listener, scoped to the bind address.
    // /proc/self/net/tcp renders the local address as `HEXIP:HEXPORT`
    // (e.g. 0100007F:1F90 for 127.0.0.1:8402).
    assert_eq!(new_listeners.len(), 1, "listeners: {new_listeners:?}");
    assert!(
        new_listeners[0].ends_with(&format!(":{port:04X}")),
        "expected a listener on :{port} (hex {:04X}), got {new_listeners:?}",
        port
    );
}
