//! End-to-end P1.7b: `vtessera-node --connectivity outbound-only
//! --coordinator-addr <addr.json>` pulls work from the coordinator's queue
//! over loopback iroh QUIC, verifies the signed `DispatchOffer`, dispatches
//! it through the normal HTTP path, runs it, and acks.
//!
//! This drives the *built binary* (not just lib internals): the observable
//! success signal is the signed per-job receipt the node persists to
//! `<state-dir>/job-receipts/<job_id>.json` after execution.
//!
//! Only compiled under `serve` (the binary only exists then).

#![cfg(feature = "serve")]

use std::collections::BTreeSet;
use std::fs;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use iroh::TransportAddr;

const POLL_IDLE_INTERVAL: Duration = Duration::from_millis(500);
const PULL_WINDOW: Duration = Duration::from_secs(30);

/// Build a loopback-only `EndpointAddr` for `ep` (direct QUIC, no relay —
/// offline-CI-safe, mirrors the coordinator's own tests).
fn loopback_addr(ep: &iroh::Endpoint) -> iroh::EndpointAddr {
    let addrs = ep
        .addr()
        .addrs
        .iter()
        .filter(|a| matches!(a, TransportAddr::Ip(_)))
        .cloned()
        .collect::<BTreeSet<_>>();
    assert!(!addrs.is_empty(), "endpoint published no IP addresses");
    iroh::EndpointAddr { id: ep.id(), addrs }
}

fn write_matching_key_and_offer(dir: &std::path::Path) {
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

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn node_pulls_free_job_from_coordinator_queue() {
    let dir = std::env::temp_dir().join(format!(
        "vtessera_node_coordinator_pull_{}",
        std::process::id()
    ));
    let _ = fs::remove_dir_all(&dir);
    fs::create_dir_all(&dir).expect("create temp dir");
    let state = dir.join("state");
    fs::create_dir_all(&state).expect("create state dir");
    write_matching_key_and_offer(&dir);

    // --- Coordinator (in-process): iroh endpoint + queue handler ----------
    let coord_key = iroh::SecretKey::from_bytes(&[0x51; 32]);
    let coord_endpoint = iroh::Endpoint::builder(iroh::endpoint::presets::N0)
        .secret_key(coord_key.clone())
        .bind()
        .await
        .expect("bind coordinator endpoint");
    let handler = vtessera_coordinator::iroh::CoordinatorQueueHandler::new(
        vtessera_settlement::SigningKey::from_bytes(&coord_key.to_bytes()),
    );
    let _router = iroh::protocol::Router::builder(coord_endpoint.clone())
        .accept(vtessera_transport::iroh_sidecar::VTESSERA_ALPN, handler)
        .spawn();

    // Let iroh discover loopback addrs, then persist the coordinator's
    // EndpointAddr where the node binary expects it.
    tokio::time::sleep(Duration::from_millis(500)).await;
    let coord_addr = loopback_addr(&coord_endpoint);
    let addr_file = dir.join("coordinator-addr.json");
    fs::write(
        &addr_file,
        serde_json::to_vec(&coord_addr).expect("serialize endpoint addr"),
    )
    .expect("write coordinator addr");

    // --- Spawn the real node, outbound-only + coordinator pull ------------
    let mut child = Command::new(env!("CARGO_BIN_EXE_vtessera-node"))
        .arg("--bind")
        .arg("127.0.0.1:1") // never bound in outbound-only
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
        .arg("--coordinator-addr")
        .arg(&addr_file)
        .arg("--coordinator-poll")
        .arg("1")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .expect("spawn vtessera-node");

    // Give the node time to boot its outbound endpoint + pull loop.
    tokio::time::sleep(Duration::from_secs(3)).await;
    assert!(
        child.try_wait().expect("try_wait").is_none(),
        "vtessera-node exited during coordinator-pull test"
    );

    // --- Enqueue a job through a second endpoint's QueueClient -----------
    let agent_key = iroh::SecretKey::from_bytes(&[0x52; 32]);
    let agent_endpoint = iroh::Endpoint::builder(iroh::endpoint::presets::N0)
        .secret_key(agent_key)
        .bind()
        .await
        .expect("bind agent endpoint");
    let client =
        vtessera_coordinator::iroh::QueueClient::new(agent_endpoint.clone(), coord_addr.clone());

    let job_id = "coord-pull-e2e-001";
    let job_json = serde_json::json!({
        "job_id": job_id,
        "image": "ghcr.io/example/echo:latest",
        "command": [],
        "env": [],
        "devices": {"class": {"kind": "cpu"}, "vcpus": 1, "mem_kb": 1024, "min_vram_mb": 0},
        "max_duration_secs": 60
    });
    client
        .enqueue(job_json)
        .await
        .expect("enqueue job at coordinator");

    // --- Observe: node pulls, runs, persists the signed receipt ------------
    let receipt = state.join("job-receipts").join(format!("{job_id}.json"));
    let deadline = Instant::now() + PULL_WINDOW;
    while Instant::now() < deadline {
        if receipt.exists() {
            break;
        }
        tokio::time::sleep(POLL_IDLE_INTERVAL).await;
    }

    assert!(
        child.try_wait().expect("try_wait").is_none(),
        "vtessera-node exited during the run"
    );
    assert!(
        receipt.exists(),
        "node never pulled+ran coordinator job {job_id} ({} waited)",
        PULL_WINDOW.as_secs()
    );

    // The receipt must be a signed JobReceipt that verifies against the node.
    let raw_receipt = fs::read_to_string(&receipt).expect("read receipt");
    let signed: vtessera_settlement::SignedJobReceipt =
        serde_json::from_str(&raw_receipt).expect("parse receipt");
    vtessera_settlement::verify_signed_job_receipt(&signed).expect("receipt must verify");

    // The queue entry must have been acked (reserve comes back empty).
    assert!(
        client.reserve().await.expect("reserve").is_none(),
        "coordinator queue not drained after node ack"
    );

    child.kill().ok();
    let _ = child.wait();
    let _ = fs::remove_dir_all(&dir);
}
