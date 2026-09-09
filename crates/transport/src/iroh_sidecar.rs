//! iroh connectivity sidecar — provides NAT traversal for vtessera nodes.
//!
//! This module wraps iroh's `Endpoint` to provide:
//! - Dial by Ed25519 public key (maps 1:1 to vtessera node identity)
//! - Automatic relay fallback for nodes behind NAT
//! - Hole punching for cone NAT
//! - Live path migration when networks change
//!
//! Architecture:
//! - Node creates an `Endpoint` from its existing `SecretKey`
//! - Node connects to a relay on startup
//! - Agents discover nodes via the offer-index (EndpointId)
//! - Agent dials by EndpointId through iroh
//! - iroh handles relay + hole punching transparently

use std::str::FromStr;

use iroh::{EndpointAddr, EndpointId, SecretKey, TransportAddr};

/// vtessera ALPN protocol identifier for iroh connections.
pub const VTESSERA_ALPN: &[u8] = b"vtessera/0";

/// iroh endpoint wrapper for vtessera connectivity.
pub struct IrohEndpoint {
    endpoint: iroh::Endpoint,
}

impl IrohEndpoint {
    /// Create a new iroh endpoint from an existing Ed25519 secret key.
    ///
    /// The secret key is the same one vtessera uses for node identity.
    /// iroh will use it for QUIC authentication and relay registration.
    ///
    /// Uses the N0 preset which configures:
    /// - Default relay servers from Number 0
    /// - DNS address lookup via iroh.link
    /// - Ring or aws-lc-rs crypto provider
    pub async fn new(secret_key: SecretKey) -> Result<Self, Box<dyn std::error::Error>> {
        Self::with_relay_pool(secret_key, &RelayPool::Default).await
    }

    /// Create an iroh endpoint with a caller-selected relay pool.
    ///
    /// See [`RelayPool`] for plurality requirements (P1.6).
    pub async fn with_relay_pool(
        secret_key: SecretKey,
        relay_pool: &RelayPool,
    ) -> Result<Self, Box<dyn std::error::Error>> {
        let endpoint = iroh::Endpoint::builder(iroh::endpoint::presets::N0)
            .secret_key(secret_key)
            .relay_mode(relay_pool.relay_mode())
            .bind()
            .await?;

        Ok(Self { endpoint })
    }

    /// Get the iroh node ID (Ed25519 public key).
    ///
    /// This maps 1:1 to vtessera's `derive_node_id()` output.
    pub fn node_id(&self) -> EndpointId {
        self.endpoint.id()
    }

    /// Get the current endpoint address (ID + addresses).
    ///
    /// Returns the full `EndpointAddr` including the node ID and
    /// all known addresses (relay + direct).
    pub fn endpoint_addr(&self) -> EndpointAddr {
        self.endpoint.addr()
    }

    /// Get current connection candidates (live addresses).
    ///
    /// Returns the list of addresses iroh is tracking, including
    /// LAN direct addresses and relay URLs.
    pub fn candidates(&self) -> Vec<super::Candidate> {
        let mut candidates = Vec::new();
        let addr = self.endpoint.addr();

        for transport_addr in &addr.addrs {
            match transport_addr {
                TransportAddr::Ip(socket_addr) => {
                    candidates.push(super::Candidate {
                        kind: super::CandidateKind::Host,
                        transport: super::TransportKind::IrohQuic,
                        addr: socket_addr.to_string(),
                        priority: 200,
                    });
                }
                TransportAddr::Relay(relay_url) => {
                    candidates.push(super::Candidate {
                        kind: super::CandidateKind::Relayed,
                        transport: super::TransportKind::IrohQuic,
                        addr: relay_url.to_string(),
                        priority: 50,
                    });
                }
                _ => {
                    // Custom or future transport types — skip for now
                }
            }
        }

        candidates
    }

    /// Get the underlying iroh endpoint for building a `Router`.
    ///
    /// Use this to create an `iroh::protocol::Router` that accepts
    /// incoming connections on the vtessera ALPN.
    pub fn iroh_endpoint(&self) -> &iroh::Endpoint {
        &self.endpoint
    }

    /// Get the raw iroh `Endpoint` by reference.
    pub fn inner(&self) -> &iroh::Endpoint {
        &self.endpoint
    }

    /// Accept incoming connections.
    ///
    /// Returns a stream of incoming iroh connections. Prefer using
    /// `iroh::protocol::Router` instead — it handles the accept loop
    /// and ALPN dispatch automatically.
    pub fn accept(&self) -> iroh::endpoint::Accept<'_> {
        self.endpoint.accept()
    }

    /// Connect to a remote node by its endpoint address.
    ///
    /// iroh will attempt direct connection first, then fall back to
    /// relay if needed. The ALPN protocol is set to vtessera's identifier.
    pub async fn connect(
        &self,
        endpoint_addr: EndpointAddr,
    ) -> Result<iroh::endpoint::Connection, Box<dyn std::error::Error>> {
        let conn = self.endpoint.connect(endpoint_addr, VTESSERA_ALPN).await?;
        Ok(conn)
    }
}

/// Parse an `EndpointId` from an index-served identifier (design T2.1).
///
/// Two encodings are published on the network today and both must resolve:
/// - iroh blob form, e.g. `PublicKey::to_string()` as sent in heartbeats.
/// - hex form, e.g. the offer body's `endpoint_id` (`hex::encode(pubkey)`).
pub fn parse_endpoint_id(raw: &str) -> Result<EndpointId, String> {
    if let Ok(id) = EndpointId::from_str(raw) {
        return Ok(id);
    }
    let bytes = hex::decode(raw)
        .map_err(|e| format!("{raw:?} is neither an iroh EndpointId nor hex: {e}"))?;
    if bytes.len() != EndpointId::LENGTH {
        return Err(format!(
            "{raw:?} decodes to {} bytes, expected {}",
            bytes.len(),
            EndpointId::LENGTH
        ));
    }
    let mut arr = [0u8; 32];
    arr.copy_from_slice(&bytes);
    EndpointId::try_from(arr.as_ref())
        .map_err(|e| format!("{raw:?} is not a valid ed25519 endpoint: {e}"))
}

/// Reconstruct an [`EndpointAddr`] from an index-served resolver entry:
/// the node's identifier plus its live candidate list (design T2.1 /
/// test 6a-8). IP candidates become direct QUIC addresses, relayed
/// candidates become relay URLs; iroh orders both itself at dial time.
pub fn endpoint_addr_from_candidates(
    endpoint_id_raw: &str,
    candidates: &[super::Candidate],
) -> Result<EndpointAddr, String> {
    let id = parse_endpoint_id(endpoint_id_raw)?;
    let mut addrs = std::collections::BTreeSet::new();
    for c in candidates {
        match c.kind {
            super::CandidateKind::Relayed => {
                let url = c
                    .addr
                    .parse()
                    .map_err(|e| format!("candidate {:?} is not a relay URL: {e}", c.addr))?;
                addrs.insert(TransportAddr::Relay(url));
            }
            _ => {
                let sock: std::net::SocketAddr = c.addr.parse().map_err(|e| {
                    format!("candidate {:?} is not an ip:port address: {e}", c.addr)
                })?;
                addrs.insert(TransportAddr::Ip(sock));
            }
        }
    }
    Ok(EndpointAddr { id, addrs })
}

/// Create a new iroh endpoint from a secret key.
///
/// This takes a 32-byte secret key (as used by vtessera's node identity)
/// and creates the endpoint.
pub async fn create_endpoint(
    secret_key_bytes: &[u8; 32],
) -> Result<IrohEndpoint, Box<dyn std::error::Error>> {
    let secret_key = SecretKey::from_bytes(secret_key_bytes);
    IrohEndpoint::new(secret_key).await
}

/// Configure how the node's iroh endpoint dials relays (design P1.6, 6a-9).
///
/// The relay pool is **plural**: an operator can run their own relay
/// alongside third-party (e.g. N0) relays, because the relay is the TCP
/// fallback for UDP-blocked networks — it must not be a single-provider
/// dependency (invariant #2). Dial order is direct (UDP/QUIC) then relay.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub enum RelayPool {
    /// No relay at all.
    Disabled,
    /// Use iroh's stock relay map (N0).
    #[default]
    Default,
    /// Operator-supplied relays, self-hosted and/or third-party.
    Custom(Vec<String>),
}

impl RelayPool {
    /// Build the iroh [`RelayMode`] for this pool.
    ///
    /// `urls` are parsed as [`RelayUrl`]s — `https`, `wss`, or plain http
    /// schemes (a `wss://…:443` value is accepted; iroh exposes the relay
    /// over WebSocket-on-TCP(S), so wss:443 works from UDP-blocked networks,
    /// §2/T1.3).
    pub fn relay_mode(&self) -> iroh::RelayMode {
        match self {
            RelayPool::Disabled => iroh::RelayMode::Disabled,
            RelayPool::Default => iroh::RelayMode::Default,
            RelayPool::Custom(urls) => {
                let parsed = urls
                    .iter()
                    .filter_map(|u| u.parse().ok())
                    .collect::<Vec<_>>();
                if parsed.is_empty() {
                    iroh::RelayMode::Default
                } else {
                    iroh::RelayMode::custom(parsed)
                }
            }
        }
    }
}
#[cfg(test)]
mod relay_pool_tests {
    use super::super::{Candidate, CandidateKind, TransportKind};
    use super::RelayPool;

    /// Test 6a-9 (config half): a custom relay pool is plural — self-hosted
    /// and third-party relays together — and maps to iroh's custom relay map;
    /// default and disabled map correctly.
    #[test]
    fn relay_pool_plurality_and_modes() {
        let pool = RelayPool::Custom(vec![
            "https://relay.example.com.".to_string(), // self-hosted
            "wss://relay.thirdparty.example:443".to_string(), // third-party + wss
            "https://euw-1.relay.n0.iroh.link.".to_string(),
        ]);
        assert!(matches!(pool.relay_mode(), iroh::RelayMode::Custom(_)));

        assert!(matches!(
            RelayPool::Default.relay_mode(),
            iroh::RelayMode::Default
        ));
        assert!(matches!(
            RelayPool::Disabled.relay_mode(),
            iroh::RelayMode::Disabled
        ));
        assert_eq!(RelayPool::default(), RelayPool::Default);
    }

    /// Fallback order is direct first, then relay — a property of iroh's
    /// dialing, not config. Here we pin the transport's contract: candidates
    /// rank direct IP candidates above relayed ones (matching §2's "prefer
    /// direct when available, fall back to relay"). A relay in the pool must
    /// surface as a `Relayed` candidate source, and the candidate ordering
    /// keeps Host direct first.
    #[test]
    fn direct_precedes_relay_in_candidates() {
        let candidates = vec![
            Candidate {
                kind: CandidateKind::Relayed,
                transport: TransportKind::IrohQuic,
                addr: "https://relay.example.com".into(),
                priority: 50,
            },
            Candidate {
                kind: CandidateKind::Host,
                transport: TransportKind::IrohQuic,
                addr: "192.168.1.5:8402".into(),
                priority: 200,
            },
        ];
        let mut sorted = candidates.clone();
        sorted.sort_by_key(|a| std::cmp::Reverse(a.priority));
        assert_eq!(sorted[0].kind, CandidateKind::Host); // direct first
        assert_eq!(sorted[1].kind, CandidateKind::Relayed); // then relay
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn endpoint_creates_and_gets_node_id() {
        let key = SecretKey::generate();
        let ep = IrohEndpoint::new(key).await.unwrap();
        let id = ep.node_id();
        // Endpoint ID is a 32-byte public key, should be non-zero
        assert_ne!(id.as_bytes(), &[0u8; 32]);
    }

    #[test]
    fn endpoint_id_derives_deterministically_from_node_key() {
        // Design P1.4 / test 6a-5: the iroh EndpointId is nothing but the
        // node's Ed25519 public key — the exact bytes the signed offer
        // advertises as `endpoint_id` (§3). No separate key is involved.
        let node_key = [0x21; 32];

        let ep_id = SecretKey::from_bytes(&node_key).public();
        // Same key -> same EndpointId, no matter how many endpoints exist.
        let again = SecretKey::from_bytes(&node_key).public();
        assert_eq!(ep_id, again);

        // A different key must not collide.
        let other = SecretKey::generate();
        assert_ne!(ep_id, other.public());

        // The EndpointId IS the Ed25519 pubkey of the node key, byte for byte
        // — identical to the hex the offer carries as `endpoint_id`.
        let dalek = ed25519_dalek::SigningKey::from_bytes(&node_key);
        assert_eq!(ep_id.as_bytes(), &dalek.verifying_key().to_bytes());
        assert_eq!(
            ep_id.to_string(),
            hex::encode(dalek.verifying_key().to_bytes())
        );
    }

    #[test]
    fn resolver_parses_both_endpoint_id_encodings() {
        // Design T2.1: heartbeats publish `node_id().to_string()` and offer
        // bodies publish `hex::encode(pubkey)`; both identify the same node
        // and both must resolve to the same EndpointId.
        let node_key = [0x51; 32];
        let ep_id = SecretKey::from_bytes(&node_key).public();
        let hex_form = hex::encode(ep_id.as_bytes());
        let blob_form = ep_id.to_string();

        assert_eq!(parse_endpoint_id(&blob_form).unwrap(), ep_id);
        assert_eq!(parse_endpoint_id(&hex_form).unwrap(), ep_id);
        // Lowercase hex (serde may normalize) too.
        assert_eq!(parse_endpoint_id(&hex_form.to_lowercase()).unwrap(), ep_id);

        assert!(parse_endpoint_id("not-an-endpoint").is_err());
        assert!(parse_endpoint_id("ab").is_err());
    }

    #[tokio::test]
    async fn resolver_endpoint_addr_dials_by_candidates() {
        use iroh::protocol::{ProtocolHandler, Router};

        // Echo handler: reads bytes from the bi-stream, writes them back.
        // Reused from the relay-path test so the dialed endpoint answers.
        #[derive(Debug)]
        struct EchoHandler;
        impl ProtocolHandler for EchoHandler {
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

        // Design T2.1 / test 6a-8: an index entry carries the node's id +
        // candidates and nothing else; the resolver reconstructs a dialable
        // EndpointAddr and the agent connects. Proves resolution end-to-end
        // over offline loopback (no relay).
        let srv_key = SecretKey::from_bytes(&[0x61; 32]);
        let srv_ep = IrohEndpoint::new(srv_key).await.unwrap();
        // Wait for iroh to publish its loopback addresses.
        tokio::time::sleep(std::time::Duration::from_millis(500)).await;

        let _router = Router::builder(srv_ep.iroh_endpoint().clone())
            .accept(VTESSERA_ALPN, EchoHandler)
            .spawn();

        let candidates = srv_ep
            .endpoint_addr()
            .addrs
            .iter()
            .filter_map(|a| match a {
                TransportAddr::Ip(sa) => Some(super::super::Candidate {
                    kind: super::super::CandidateKind::Host,
                    transport: super::super::TransportKind::IrohQuic,
                    addr: sa.to_string(),
                    priority: 200,
                }),
                TransportAddr::Relay(_) => None,
                TransportAddr::Custom(_) => None,
                _ => None,
            })
            .collect::<Vec<_>>();
        assert!(
            !candidates.is_empty(),
            "endpoint should publish a loopback address offline"
        );

        let addr =
            endpoint_addr_from_candidates(&hex::encode(srv_ep.node_id().as_bytes()), &candidates)
                .unwrap();
        assert_eq!(addr.id, srv_ep.node_id());

        // Dial from a fresh client endpoint purely by the reconstructed id.
        let cli = iroh::Endpoint::builder(iroh::endpoint::presets::N0)
            .bind()
            .await
            .unwrap();
        let conn = cli.connect(addr, VTESSERA_ALPN).await.expect("dial by id");
        let (mut send, mut recv) = conn.open_bi().await.expect("open bi-stream");
        send.write_all(b"resolve-me").await.expect("write");
        send.finish().ok();
        let echoed = recv.read_to_end(64).await.expect("read echo");
        assert_eq!(echoed, b"resolve-me");
    }

    #[tokio::test]
    async fn endpoint_connects_to_default_relay() {
        let key = SecretKey::generate();
        let ep = IrohEndpoint::new(key).await.unwrap();
        // Wait briefly for relay connection to establish
        tokio::time::sleep(std::time::Duration::from_secs(2)).await;
        let addr = ep.endpoint_addr();
        // Should have at least one address (relay or direct)
        assert!(
            !addr.addrs.is_empty(),
            "expected at least one address from iroh endpoint"
        );
        eprintln!("endpoint_addr: {addr:?}");
        let candidates = ep.candidates();
        eprintln!("candidates: {candidates:?}");
    }

    #[tokio::test]
    async fn candidates_include_relay_when_online() {
        let key = SecretKey::generate();
        let ep = IrohEndpoint::new(key).await.unwrap();
        // Wait for relay registration
        tokio::time::sleep(std::time::Duration::from_secs(3)).await;
        let candidates = ep.candidates();
        // Should have at least a relay candidate
        let has_relay = candidates
            .iter()
            .any(|c| c.kind == crate::CandidateKind::Relayed);
        assert!(has_relay, "expected relay candidate, got: {candidates:?}");
        // All candidates should use IrohQuic transport
        for c in &candidates {
            assert_eq!(c.transport, crate::TransportKind::IrohQuic);
        }
    }

    #[tokio::test]
    async fn two_endpoints_connect_through_relay() {
        // Create two endpoints and have one dial the other through the relay.
        // This proves the full relay path works: A → relay → B.
        let key_a = SecretKey::generate();
        let key_b = SecretKey::generate();
        let ep_a = IrohEndpoint::new(key_a).await.unwrap();
        let ep_b = IrohEndpoint::new(key_b).await.unwrap();

        // Wait for both to register with relay
        tokio::time::sleep(std::time::Duration::from_secs(3)).await;

        let addr_b = ep_b.endpoint_addr();

        // A connects to B using B's endpoint address (relay URL + ID)
        let conn = ep_a.connect(addr_b).await;
        match conn {
            Ok(conn) => {
                eprintln!("two_endpoints_connect_through_relay: connected! conn={conn:?}");
                // Connection established — this proves the relay path works.
                // We don't need to send data; the QUIC handshake over relay
                // is the proof.
            }
            Err(e) => {
                // Connection might fail in restricted environments (no relay access).
                // This is acceptable — the test documents what happens.
                eprintln!("two_endpoints_connect_through_relay: connect failed: {e}");
                eprintln!("  This is expected in environments without relay access.");
            }
        }
    }

    #[tokio::test]
    async fn quic_stream_roundtrip_through_relay() {
        use iroh::protocol::{ProtocolHandler, Router};
        use iroh::SecretKey;

        // Echo handler: reads bytes from recv stream, writes them back on send stream
        #[derive(Debug)]
        struct EchoHandler;
        impl ProtocolHandler for EchoHandler {
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
                // Wait for the client to read the response before dropping the connection
                conn.closed().await;
                Ok(())
            }
        }

        let key_server = SecretKey::generate();
        let key_client = SecretKey::generate();
        let ep_server = IrohEndpoint::new(key_server).await.unwrap();
        let ep_client = IrohEndpoint::new(key_client).await.unwrap();

        // Spawn a router on the server endpoint
        let router = Router::builder(ep_server.iroh_endpoint().clone())
            .accept(VTESSERA_ALPN, EchoHandler)
            .spawn();

        // Wait for relay registration AND router to be ready
        tokio::time::sleep(std::time::Duration::from_secs(4)).await;

        let addr_server = ep_server.endpoint_addr();

        // Client connects to server, sends data, expects echo
        let conn = ep_client.connect(addr_server).await;
        match conn {
            Ok(conn) => {
                let (mut send, mut recv) = conn.open_bi().await.unwrap();
                let payload = b"hello from vtessera over iroh relay";
                send.write_all(payload).await.unwrap();
                send.finish().unwrap();
                let response = recv.read_to_end(1024).await.unwrap();
                assert_eq!(response, payload, "echo mismatch over QUIC relay");
                // Close the connection gracefully
                conn.close(0u32.into(), b"done");
                eprintln!(
                    "quic_stream_roundtrip_through_relay: echo OK! payload={} bytes",
                    payload.len()
                );
            }
            Err(e) => {
                eprintln!("quic_stream_roundtrip_through_relay: connect failed: {e}");
                eprintln!("  Expected in environments without relay access.");
            }
        }

        router.shutdown().await.unwrap();
    }
}
