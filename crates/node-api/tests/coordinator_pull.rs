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

/// Design test 6a-10 — **federated failover (e2e, in-process)**.
///
/// The node pins *two* coordinators (A, then B). While A is alive a job
/// enqueued on A is pulled, dispatched, and acked. A is then killed
/// (router shutdown + endpoint close); a job enqueued on B still runs, the
/// process stays alive (the degradation chain A→?→B never fails closed into
/// a hard 503), and a coordinator's death ejects nothing globally — B keeps
/// serving the node. Per-coordinator lease receipts are covered at the
/// lib level (`crates/coordinator` tests).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn node_fails_over_from_dead_coordinator_a_to_b() {
    let dir = std::env::temp_dir().join(format!("vtessera_node_failover_{}", std::process::id()));
    let _ = fs::remove_dir_all(&dir);
    fs::create_dir_all(&dir).expect("create temp dir");
    let state = dir.join("state");
    fs::create_dir_all(&state).expect("create state dir");
    write_matching_key_and_offer(&dir);

    // --- Two in-process coordinators: A (preferred) then B (fallback) -----
    let spawn_coord = |seed: [u8; 32]| async move {
        let key = iroh::SecretKey::from_bytes(&seed);
        let endpoint = iroh::Endpoint::builder(iroh::endpoint::presets::N0)
            .secret_key(key.clone())
            .bind()
            .await
            .expect("bind coordinator endpoint");
        let handler = vtessera_coordinator::iroh::CoordinatorQueueHandler::new(
            vtessera_settlement::SigningKey::from_bytes(&key.to_bytes()),
        );
        let router = iroh::protocol::Router::builder(endpoint.clone())
            .accept(vtessera_transport::iroh_sidecar::VTESSERA_ALPN, handler)
            .spawn();
        (key, endpoint, router)
    };
    let (_key_a, endpoint_a, router_a) = spawn_coord([0x61; 32]).await;
    let (_key_b, endpoint_b, _router_b) = spawn_coord([0x62; 32]).await;

    // Persist both coordinator addrs where the node binary expects them.
    tokio::time::sleep(Duration::from_millis(500)).await;
    let addr_a = loopback_addr(&endpoint_a);
    let addr_b = loopback_addr(&endpoint_b);
    let addr_file_a = dir.join("coord-a.json");
    let addr_file_b = dir.join("coord-b.json");
    fs::write(
        &addr_file_a,
        serde_json::to_vec(&addr_a).expect("serialize endpoint addr"),
    )
    .expect("write coordinator A addr");
    fs::write(
        &addr_file_b,
        serde_json::to_vec(&addr_b).expect("serialize endpoint addr"),
    )
    .expect("write coordinator B addr");

    // --- Spawn the node pinned to [A, B] (federation order = preference) ---
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
        .arg(&addr_file_a)
        .arg("--coordinator-addr")
        .arg(&addr_file_b)
        .arg("--coordinator-poll")
        .arg("1")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .expect("spawn vtessera-node");

    // Give the node time to boot its outbound endpoint + both pull tasks.
    tokio::time::sleep(Duration::from_secs(3)).await;
    assert!(
        child.try_wait().expect("try_wait").is_none(),
        "vtessera-node exited during failover test"
    );

    // --- Stage 1: coordinator A serves a job while both are alive ----------
    let agent_key = iroh::SecretKey::from_bytes(&[0x53; 32]);
    let agent_endpoint = iroh::Endpoint::builder(iroh::endpoint::presets::N0)
        .secret_key(agent_key)
        .bind()
        .await
        .expect("bind agent endpoint");
    let client_a =
        vtessera_coordinator::iroh::QueueClient::new(agent_endpoint.clone(), addr_a.clone());
    let client_b = vtessera_coordinator::iroh::QueueClient::new(agent_endpoint, addr_b.clone());

    let job_a = "coord-failover-a";
    client_a
        .enqueue(job_json(job_a))
        .await
        .expect("enqueue job at coordinator A");
    wait_for_receipt(&state, job_a, &mut child).await;
    assert!(
        client_a.reserve().await.expect("reserve").is_none(),
        "coordinator A queue not drained after node ack"
    );

    // --- Stage 2: kill coordinator A (router + endpoint) -------------------
    router_a.shutdown().await.ok();
    endpoint_a.close().await;
    // Give the (now-dead) dial a beat to be observed as unreachable.
    tokio::time::sleep(Duration::from_millis(500)).await;

    // --- Stage 3: B keeps serving; the node never fails closed --------------
    let job_b = "coord-failover-b";
    client_b
        .enqueue(job_json(job_b))
        .await
        .expect("enqueue job at coordinator B");
    wait_for_receipt(&state, job_b, &mut child).await;
    assert!(
        client_b.reserve().await.expect("reserve").is_none(),
        "coordinator B queue not drained after failover"
    );

    child.kill().ok();
    let _ = child.wait();
    let _ = fs::remove_dir_all(&dir);
}

fn job_json(job_id: &str) -> serde_json::Value {
    serde_json::json!({
        "job_id": job_id,
        "image": "ghcr.io/example/echo:latest",
        "command": [],
        "env": [],
        "devices": {"class": {"kind": "cpu"}, "vcpus": 1, "mem_kb": 1024, "min_vram_mb": 0},
        "max_duration_secs": 60
    })
}

async fn wait_for_receipt(state: &std::path::Path, job_id: &str, child: &mut std::process::Child) {
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

    let raw_receipt = fs::read_to_string(&receipt).expect("read receipt");
    let signed: vtessera_settlement::SignedJobReceipt =
        serde_json::from_str(&raw_receipt).expect("parse receipt");
    vtessera_settlement::verify_signed_job_receipt(&signed).expect("receipt must verify");
}
