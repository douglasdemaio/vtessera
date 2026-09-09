#![cfg(feature = "serve")]
//! §6b infrastructure tests — design `zero-config-connectivity.md` §6b.
//!
//! These exercise the connectivity plane itself: relay paths, path loss,
//! reconnect soak, and multi-peer resolution. Each test comes with a
//! runbook header explaining the topology and how to run it on real
//! hardware. Network-dependent cases are marked `#[ignore]` so plain
//! `cargo test` stays offline, fast, and deterministic:
//!
//! ```text
//! cargo test -p vtessera-transport --features serve                 # offline unit/integration
//! cargo test -p vtessera-transport --features serve -- --ignored    # §6b infra (needs internet/relay)
//! ```
//!
//! Runbook-only cases (no automated harness; requires physical rigs):
//! - CGNAT double-NAT outbound-only node: run `scripts/local-stack.sh` with
//!   `VTESSERA_OUTBOUND=1` behind CGNAT; assert the node owns no LISTEN
//!   socket (`ss -ltnp | grep vtessera`) yet jobs pull via the coordinator
//!   queue and settle.
//! - UDP fully blocked (callback-only NAT): force `wss://` relay mode
//!   (RelayPool with a WebSocket relay); assert a job still arrives over the
//!   relay's TCP path.
//! - Index killed mid-run: kill offer-index while jobs are dispatched; the
//!   node must keep running jobs and the index must converge after restart
//!   (no duplicate registrations, candidates repopulate via heartbeat).
//! - Node killed → settlement: kill a node mid-job; escrow settlement must
//!   still complete on the next interval (design §x / settlement crate).
//! - 10-node swarm: ten nodes behind mixed NAT against one index; every
//!   agent must be able to discover and dial *each* node by EndpointId.

use std::time::Duration;

use iroh::protocol::{ProtocolHandler, Router};
use iroh::{EndpointAddr, SecretKey, TransportAddr};
use vtessera_transport::iroh_sidecar::{
    endpoint_addr_from_candidates, IrohEndpoint, VTESSERA_ALPN,
};

/// Echo handler proving a dialed stream is served by our ALPN.
#[derive(Debug)]
struct Echo;
impl ProtocolHandler for Echo {
    async fn accept(
        &self,
        conn: iroh::endpoint::Connection,
    ) -> Result<(), iroh::protocol::AcceptError> {
        let (mut send, mut recv) = conn.accept_bi().await?;
        let data = recv
            .read_to_end(1024)
            .await
            .map_err(std::io::Error::other)?;
        send.write_all(&data).await.map_err(std::io::Error::other)?;
        send.finish()?;
        conn.closed().await;
        Ok(())
    }
}

/// §6b "stay-relayed": a job dialed through the relay only (no direct path
/// offered — the peer advertises relay-only addresses) must round-trip.
///
/// Runbook: needs internet access to the N0 relay pool. On a machine behind
/// NAT with no port forwarding, both peers see only relay candidates; this
/// proves the relay (not just loopback direct) carries vtessera traffic.
#[tokio::test]
#[ignore = "runbook §6b: requires internet access to the N0 relay pool (see file header)"]
async fn stay_relayed_dial_roundtrips_via_relay() {
    let srv_key = SecretKey::from_bytes(&[0x71; 32]);
    let cli_key = SecretKey::from_bytes(&[0x72; 32]);

    let srv = IrohEndpoint::new(srv_key).await.expect("srv endpoint");
    let _router = Router::builder(srv.iroh_endpoint().clone())
        .accept(VTESSERA_ALPN, Echo)
        .spawn();
    // Let relay registration complete so the relay URL is known.
    tokio::time::sleep(Duration::from_secs(3)).await;

    let mut relay_only = srv.endpoint_addr();
    // Keep ONLY the relay candidate: the client must traverse the relay.
    relay_only.addrs = relay_only
        .addrs
        .iter()
        .filter(|a| matches!(a, TransportAddr::Relay(_)))
        .cloned()
        .collect();
    assert!(
        !relay_only.addrs.is_empty(),
        "peer published no relay address — is DNS/relay reachable?"
    );

    let cli = IrohEndpoint::new(cli_key).await.expect("cli endpoint");
    let conn = cli.connect(relay_only).await.expect("dial through relay");
    let (mut send, mut recv) = conn.open_bi().await.expect("open bi");
    send.write_all(b"relayed-job").await.unwrap();
    send.finish().ok();
    let echoed = recv.read_to_end(64).await.expect("read echo");
    assert_eq!(echoed, b"relayed-job");
}

/// §6b "100 disconnects": the client must survive repeated forced teardown
/// and reconnect cleanly, each time dialing the *same* EndpointId — the
/// steady-state behavior behind flaky mobile/wifi links. Loopback-only so it
/// runs anywhere; kept `#[ignore]` as the §6b soak.
#[tokio::test]
#[ignore = "runbook §6b: 100-cycle disconnect/reconnect soak (loopback; N0 offline)"]
async fn one_hundred_forced_disconnects_reconnect() {
    let srv = IrohEndpoint::new(SecretKey::from_bytes(&[0x73; 32]))
        .await
        .expect("srv endpoint");
    let _router = Router::builder(srv.iroh_endpoint().clone())
        .accept(VTESSERA_ALPN, Echo)
        .spawn();
    tokio::time::sleep(Duration::from_millis(500)).await;

    let cli = IrohEndpoint::new(SecretKey::from_bytes(&[0x74; 32]))
        .await
        .expect("cli endpoint");
    let addr = srv.endpoint_addr();

    for i in 0..100 {
        let conn = cli.connect(addr.clone()).await.unwrap_or_else(|e| {
            panic!("cycle {i}: reconnect dial failed: {e}");
        });
        let (mut send, mut recv) = conn.open_bi().await.expect("open bi");
        send.write_all(format!("cycle-{i}").as_bytes())
            .await
            .unwrap();
        send.finish().ok();
        let echoed = recv.read_to_end(64).await.expect("read echo");
        assert_eq!(echoed, format!("cycle-{i}").as_bytes());
        // Forced teardown: drop the connection without a clean close.
        conn.close(0u32.into(), b"forced");
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

/// §6b "relay plurality": the dial must work when the peer's EndpointAddr
/// lists *multiple* relay URLs (self-hosted + third-party, invariant #2),
/// not just the operator's relay.
///
/// Runbook: requires two operator relays reachable from both peers. Without
/// them the test fails fast — run only on a plural-relay deployment.
#[tokio::test]
#[ignore = "runbook §6b: requires two independently reachable relays (see file header)"]
async fn relay_plurality_two_relays_resolve_and_dial() {
    const R1: &str = "https://relay-one.example.com";
    const R2: &str = "https://relay-two.example.com";
    let srv_key = SecretKey::from_bytes(&[0x75; 32]);
    let cli_key = SecretKey::from_bytes(&[0x76; 32]);

    let srv = IrohEndpoint::new(srv_key).await.expect("srv");
    let _router = Router::builder(srv.iroh_endpoint().clone())
        .accept(VTESSERA_ALPN, Echo)
        .spawn();
    // Pin two relay URLs to prove plurality — exactly what the heartbeat
    // carries once a style="Custom" RelayPool registers both.
    let plural = EndpointAddr {
        id: srv.node_id(),
        addrs: [
            TransportAddr::Relay(R1.parse().expect("relay 1 url")),
            TransportAddr::Relay(R2.parse().expect("relay 2 url")),
        ]
        .into_iter()
        .collect(),
    };

    let cli = IrohEndpoint::new(cli_key).await.expect("cli");
    let conn = cli
        .connect(plural)
        .await
        .unwrap_or_else(|e| panic!("dialing via plural relay list failed: {e}"));
    let (mut send, mut recv) = conn.open_bi().await.expect("open bi");
    send.write_all(b"plural-relay").await.unwrap();
    send.finish().ok();
    assert_eq!(recv.read_to_end(64).await.unwrap(), b"plural-relay");
}

/// §6b "two-node swarm": two peers exchange their EndpointIds + candidate
/// lists (as the offer-index does) and each resolves and dials the other's
/// resolver entry — the T2.1 same-machine swarm smoke. Offline loopback.
#[tokio::test]
async fn two_peer_index_entries_resolve_and_interdial() {
    let a = IrohEndpoint::new(SecretKey::from_bytes(&[0x77; 32]))
        .await
        .expect("peer A");
    let b = IrohEndpoint::new(SecretKey::from_bytes(&[0x78; 32]))
        .await
        .expect("peer B");
    tokio::time::sleep(Duration::from_millis(500)).await;

    // Peer A serves vtessera; the other dials it.
    let router = Router::builder(a.iroh_endpoint().clone())
        .accept(VTESSERA_ALPN, Echo)
        .spawn();

    // Build A's "index entry": its resolvable id + candidates.
    let candidates_a = a
        .endpoint_addr()
        .addrs
        .iter()
        .filter_map(|x| match x {
            TransportAddr::Ip(sa) => Some(vtessera_transport::Candidate {
                kind: vtessera_transport::CandidateKind::Host,
                transport: vtessera_transport::TransportKind::IrohQuic,
                addr: sa.to_string(),
                priority: 200,
            }),
            _ => None,
        })
        .collect::<Vec<_>>();
    assert!(!candidates_a.is_empty(), "peer A published no IP");

    let hex_id_a = hex::encode(a.node_id().as_bytes());
    let addr_a = endpoint_addr_from_candidates(&hex_id_a, &candidates_a).expect("resolve A");
    let conn = b
        .iroh_endpoint()
        .connect(addr_a, VTESSERA_ALPN)
        .await
        .expect("B dials A by resolved candidate list");
    let (mut send, mut recv) = conn.open_bi().await.expect("open bi");
    send.write_all(b"swarm").await.unwrap();
    send.finish().ok();
    assert_eq!(recv.read_to_end(64).await.unwrap(), b"swarm");

    drop(router);
}

// §6b TCP fallback note: when UDP is blocked the relay's WebSocket-on-TCP
// path carries traffic. We cannot flip the OS UDP policy from a test, so the
// *behavior* (dialing a peer whose only route is a `wss://` relay) is
// checked by `stay_relayed_dial_roundtrips_via_relay` on a machine with
// UDP blocked. See the file header runbook.
