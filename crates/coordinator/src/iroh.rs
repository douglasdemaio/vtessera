#![cfg(feature = "serve")]

//! iroh QUIC binding for the coordinator — the queue rides inside iroh,
//! end-to-end encrypted to the peer with the coordinator's Ed25519 key
//! pinned (invariant #2 / §4a), reusing the node's `vtessera/0` ALPN.
//!
//! The wire protocol is JSON over the iroh bi-directional stream: the caller
//! writes a [`QueueRequest`], the handler replies with a [`QueueResponse`].
//! Coordinator-produced responses are signed ([`SignedMsg`]), so a node can
//! audit them (§4b-2).

use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

use ed25519_dalek::SigningKey;
use iroh::protocol::ProtocolHandler;
use serde::{Deserialize, Serialize};

use vtessera_transport::iroh_sidecar::VTESSERA_ALPN;

use crate::{CoordinatorId, CoordinatorPayload, ScopedId, SignedMsg};

/// In-memory coordinator work queue. Jobs are opaque to the coordinator.
#[derive(Debug, Default)]
pub struct WorkQueue {
    jobs: VecDeque<(ScopedId, serde_json::Value)>,
    reserved: Vec<ScopedId>,
}

impl WorkQueue {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn len(&self) -> usize {
        self.jobs.len()
    }

    pub fn is_empty(&self) -> bool {
        self.jobs.is_empty()
    }

    /// Queue a job; returns the coordinator-scoped job id.
    pub fn enqueue(
        &mut self,
        allocator: &mut crate::IdAllocator,
        job: serde_json::Value,
    ) -> ScopedId {
        let job_id = allocator.allocate("job");
        self.jobs.push_back((job_id.clone(), job));
        job_id
    }

    /// Hand the next queued job to a dispatcher, marking it reserved.
    pub fn reserve(&mut self) -> Option<(ScopedId, serde_json::Value)> {
        let (job_id, job) = self.jobs.pop_front()?;
        self.reserved.push(job_id.clone());
        Some((job_id, job))
    }

    /// Node reports the job handled/done.
    pub fn ack(&mut self, job_id: &ScopedId) -> bool {
        let before = self.reserved.len();
        self.reserved.retain(|r| r != job_id);
        self.reserved.len() != before
    }
}

/// Client → coordinator request (JSON over the QUIC stream).
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "op", rename_all = "snake_case", deny_unknown_fields)]
pub enum QueueRequest {
    /// Agent/operator POSTs a job to the queue (§4a).
    Enqueue { job: serde_json::Value },
    /// Node pulls the next job.
    Reserve,
    /// Node reports a dispatched job accepted/completed.
    Ack { job_id: ScopedId },
    /// Node requests a per-coordinator lease.
    RequestLease { node: CoordinatorId },
}

/// Coordinator → client response. Coordinator-signed emissions are
/// [`SignedMsg`] envelopes.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum QueueResponse {
    Enqueued {
        job_id: ScopedId,
    },
    /// A `DispatchOffer` emission, signed by the coordinator (§4b-2).
    Reserved {
        offer: Option<SignedMsg>,
    },
    Acked {
        ok: bool,
    },
    LeaseGranted {
        lease: SignedMsg,
    },
}

/// Reusable queue **client** (node pull loop, coordinator `submit`, tests).
///
/// Holds an outbound [`iroh::Endpoint`] and the coordinator's [`EndpointAddr`]
/// so it can dial the coordinator over `VTESSERA_ALPN` on demand. Each call
/// opens a fresh connection and bi-stream (the coordinator handler is
/// connection-per-`accept_bi`), so the client is cheap to share across a
/// pull loop.
#[derive(Clone, Debug)]
pub struct QueueClient {
    endpoint: iroh::Endpoint,
    coordinator_addr: iroh::EndpointAddr,
}

impl QueueClient {
    pub fn new(endpoint: iroh::Endpoint, coordinator_addr: iroh::EndpointAddr) -> Self {
        Self {
            endpoint,
            coordinator_addr,
        }
    }

    /// The coordinator's EndpointId, hex-encoded (its Ed25519 pubkey). This is
    /// the identity a node pins to verify signed emissions (§4b-2).
    pub fn coordinator_id_hex(&self) -> String {
        hex::encode(self.coordinator_addr.id.as_bytes())
    }

    /// Prove the coordinator is reachable with a real QUIC handshake
    /// (design §4b-7: "reachability = proved by the RPC round-trip").
    ///
    /// Dialing opens a connection and completes the handshake; no user
    /// traffic is exchanged, just liveness. Used by the agent's queue
    /// health/offer path so it never reports a coordinator as reachable
    /// it has not actually dialed. Returns an error (with context) when
    /// the coordinator is dead, not listening on the ALPN, or the dial
    /// exceeds `timeout`.
    pub async fn probe(&self, timeout: std::time::Duration) -> Result<(), String> {
        let conn = tokio::time::timeout(
            timeout,
            self.endpoint
                .connect(self.coordinator_addr.clone(), VTESSERA_ALPN),
        )
        .await
        .map_err(|_| {
            format!(
                "dial coordinator: timed out after {:?} (no relay/DIRECT path)",
                timeout
            )
        })?
        .map_err(|e| format!("dial coordinator: {e}"))?;
        conn.close(0u32.into(), b"probe");
        Ok(())
    }

    /// One JSON round-trip over a fresh QUIC bi-stream.
    async fn rpc(&self, request: QueueRequest) -> Result<QueueResponse, String> {
        let conn = self
            .endpoint
            .connect(self.coordinator_addr.clone(), VTESSERA_ALPN)
            .await
            .map_err(|e| format!("dial coordinator: {e}"))?;
        let (mut send, mut recv) = conn.open_bi().await.map_err(|e| format!("open_bi: {e}"))?;
        let req = serde_json::to_vec(&request).map_err(|e| format!("serialize request: {e}"))?;
        send.write_all(&req)
            .await
            .map_err(|e| format!("send request: {e}"))?;
        send.finish().map_err(|e| format!("finish request: {e}"))?;
        let body = recv
            .read_to_end(256 * 1024)
            .await
            .map_err(|e| format!("read response: {e}"))?;
        conn.close(0u32.into(), b"done");
        serde_json::from_slice(&body).map_err(|e| format!("parse response: {e}"))
    }

    /// Place a job on the queue; returns the coordinator-scoped job id.
    pub async fn enqueue(&self, job: serde_json::Value) -> Result<ScopedId, String> {
        match self.rpc(QueueRequest::Enqueue { job }).await? {
            QueueResponse::Enqueued { job_id } => Ok(job_id),
            other => Err(format!("unexpected response: {other:?}")),
        }
    }

    /// Pull the next job. `Ok(None)` means the queue is empty.
    pub async fn reserve(&self) -> Result<Option<SignedMsg>, String> {
        match self.rpc(QueueRequest::Reserve).await? {
            QueueResponse::Reserved { offer } => Ok(offer),
            other => Err(format!("unexpected response: {other:?}")),
        }
    }

    /// Acknowledge a dispatched job (removes it from the coordinator's
    /// reserved set).
    pub async fn ack(&self, job_id: &ScopedId) -> Result<bool, String> {
        match self
            .rpc(QueueRequest::Ack {
                job_id: job_id.clone(),
            })
            .await?
        {
            QueueResponse::Acked { ok } => Ok(ok),
            other => Err(format!("unexpected response: {other:?}")),
        }
    }

    /// Request a per-coordinator lease for a registered node.
    pub async fn request_lease(&self, node: CoordinatorId) -> Result<SignedMsg, String> {
        match self.rpc(QueueRequest::RequestLease { node }).await? {
            QueueResponse::LeaseGranted { lease } => Ok(lease),
            other => Err(format!("unexpected response: {other:?}")),
        }
    }
}

/// iroh `ProtocolHandler` serving the queue on `vtessera/0`.
#[derive(Debug)]
pub struct CoordinatorQueueHandler {
    queue: Arc<Mutex<WorkQueue>>,
    allocator: Arc<Mutex<crate::IdAllocator>>,
    signing_key: SigningKey,
}

impl CoordinatorQueueHandler {
    pub fn new(signing_key: SigningKey) -> Self {
        Self {
            queue: Arc::new(Mutex::new(WorkQueue::new())),
            allocator: Arc::new(Mutex::new(crate::IdAllocator::new(
                CoordinatorId::from_seed(&signing_key.to_bytes()),
            ))),
            signing_key,
        }
    }

    /// Process one request synchronously (shared by the iroh accept loop and
    /// deterministic tests).
    pub fn handle(&self, request: QueueRequest) -> QueueResponse {
        match request {
            QueueRequest::Enqueue { job } => {
                let job_id = self
                    .queue
                    .lock()
                    .unwrap()
                    .enqueue(&mut self.allocator.lock().unwrap(), job);
                QueueResponse::Enqueued { job_id }
            }
            QueueRequest::Reserve => {
                let reserved = self.queue.lock().unwrap().reserve();
                match reserved {
                    Some((job_id, job)) => {
                        let payload = serde_json::to_vec(&CoordinatorPayload::DispatchOffer {
                            job_id: job_id.clone(),
                            job,
                        })
                        .unwrap();
                        let signed = SignedMsg::sign(&payload, &self.signing_key);
                        QueueResponse::Reserved {
                            offer: Some(signed),
                        }
                    }
                    None => QueueResponse::Reserved { offer: None },
                }
            }
            QueueRequest::Ack { job_id } => {
                let ok = self.queue.lock().unwrap().ack(&job_id);
                QueueResponse::Acked { ok }
            }
            QueueRequest::RequestLease { node } => {
                let lease_id = self.allocator.lock().unwrap().allocate("lease");
                let payload = serde_json::to_vec(&CoordinatorPayload::LeaseGrant {
                    lease_id: lease_id.clone(),
                    node,
                    expires_at_unix: crate::DEFAULT_LEASE_SECS,
                })
                .unwrap();
                let signed = SignedMsg::sign(&payload, &self.signing_key);
                QueueResponse::LeaseGranted { lease: signed }
            }
        }
    }
}

impl ProtocolHandler for CoordinatorQueueHandler {
    async fn accept(
        &self,
        connection: iroh::endpoint::Connection,
    ) -> Result<(), iroh::protocol::AcceptError> {
        let (mut send, mut recv) = match connection.accept_bi().await {
            Ok(s) => s,
            Err(e) => {
                eprintln!("vtessera-coordinator: accept_bi failed: {e}");
                return Ok(());
            }
        };
        let bytes = match tokio::time::timeout(
            std::time::Duration::from_secs(10),
            recv.read_to_end(256 * 1024),
        )
        .await
        {
            Ok(Ok(b)) => b,
            _ => {
                let _ = send.finish();
                return Ok(());
            }
        };
        let response = match serde_json::from_slice::<QueueRequest>(&bytes) {
            Ok(request) => self.handle(request),
            Err(e) => {
                eprintln!("vtessera-coordinator: rejecting bad request: {e}");
                let body = serde_json::to_vec(&QueueResponse::Acked { ok: false }).unwrap();
                let _ = send.write_all(&body).await;
                let _ = send.finish();
                return Ok(());
            }
        };
        let body = serde_json::to_vec(&response).unwrap();
        let _ = send.write_all(&body).await;
        let _ = send.finish();
        // Wait for the client to read the reply before the router drops the
        // connection (mirrors vtessera-transport's echo handler).
        connection.closed().await;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use iroh::protocol::Router;
    use iroh::{Endpoint, EndpointAddr, TransportAddr};
    use std::collections::BTreeSet;
    use std::time::Duration;

    /// Build a keyed loopback-only address for `ep` so dials never touch a
    /// relay (offline-CI-safe).
    fn loopback_addr(ep: &Endpoint) -> EndpointAddr {
        let addrs = ep
            .addr()
            .addrs
            .iter()
            .filter(|a| matches!(a, TransportAddr::Ip(_)))
            .cloned()
            .collect::<BTreeSet<_>>();
        assert!(!addrs.is_empty(), "endpoint published no IP addresses");
        EndpointAddr { id: ep.id(), addrs }
    }

    /// Deterministic offline roundtrip: queue a job on the coordinator's
    /// endpoint, pull it from a second endpoint over loopback QUIC, and
    /// verify the dispatch offer is a signed, auditable emission.
    #[tokio::test]
    async fn queue_roundtrips_over_loopback_quic() {
        let key_srv = iroh::SecretKey::from_bytes(&[0x31; 32]);
        let key_cli = iroh::SecretKey::from_bytes(&[0x32; 32]);

        let srv = Endpoint::builder(iroh::endpoint::presets::N0)
            .secret_key(key_srv.clone())
            .bind()
            .await
            .unwrap();

        let handler = CoordinatorQueueHandler::new(SigningKey::from_bytes(&key_srv.to_bytes()));
        let _router = Router::builder(srv.clone())
            .accept(VTESSERA_ALPN, handler)
            .spawn();

        let cli = Endpoint::builder(iroh::endpoint::presets::N0)
            .secret_key(key_cli.clone())
            .bind()
            .await
            .unwrap();

        // Wait for iroh to discover its local (loopback) addresses, then dial
        // by that keyed address — relay optional, direct QUIC preferred.
        tokio::time::sleep(Duration::from_millis(500)).await;
        let addr_srv = loopback_addr(&srv);
        let cli_id_hex = hex::encode(key_cli.public().as_bytes());

        // Client enqueues a job at the coordinator.
        let client = QueueClient::new(cli.clone(), addr_srv.clone());
        let job_id = client
            .enqueue(serde_json::json!({"image": "busybox", "command": ["echo", "hi"]}))
            .await
            .expect("enqueue");
        assert_eq!(
            job_id.coordinator.0,
            hex::encode(key_srv.public().as_bytes())
        );

        // Pull the job back out and check the offer is signed by the
        // coordinator (auditable, per §4b-2).
        let offer = client.reserve().await.expect("reserve").expect("one job");
        assert_eq!(
            client.coordinator_id_hex(),
            hex::encode(key_srv.public().as_bytes())
        );
        let pubkey_hex = hex::encode(key_srv.public().as_bytes());
        offer.verify(&pubkey_hex).unwrap();

        // Ack; a second reserve must come back empty.
        assert!(client.ack(&job_id).await.expect("ack"));
        assert!(client.reserve().await.expect("reserve").is_none());

        // Lease path is signed and per-coordinator too.
        let lease = client
            .request_lease(CoordinatorId(cli_id_hex))
            .await
            .expect("lease");
        lease.verify(&pubkey_hex).unwrap();
    }

    /// Honest-reachability probe (design §4b-7): a real QUIC handshake must
    /// succeed against a live coordinator and fail against one that is not
    /// answer on the ALPN.
    #[tokio::test]
    async fn probe_verifies_live_coordinator_roundtrip() {
        let srv_key = iroh::SecretKey::from_bytes(&[0x41; 32]);
        let cli_key = iroh::SecretKey::from_bytes(&[0x42; 32]);

        let srv = Endpoint::builder(iroh::endpoint::presets::N0)
            .secret_key(srv_key.clone())
            .bind()
            .await
            .unwrap();
        let handler = CoordinatorQueueHandler::new(SigningKey::from_bytes(&srv_key.to_bytes()));
        let _router = Router::builder(srv.clone())
            .accept(VTESSERA_ALPN, handler)
            .spawn();

        let cli = Endpoint::builder(iroh::endpoint::presets::N0)
            .secret_key(cli_key.clone())
            .bind()
            .await
            .unwrap();
        tokio::time::sleep(Duration::from_millis(500)).await;

        let client = QueueClient::new(cli.clone(), loopback_addr(&srv));
        client
            .probe(Duration::from_secs(5))
            .await
            .expect("live coordinator must answer the dial");
    }

    #[tokio::test]
    async fn probe_errors_on_dead_coordinator() {
        let cli_key = iroh::SecretKey::from_bytes(&[0x44; 32]);
        let cli = Endpoint::builder(iroh::endpoint::presets::N0)
            .secret_key(cli_key)
            .bind()
            .await
            .unwrap();
        // 127.0.0.1:1 is closed — nothing answers the ALPN there. A tight
        // 500ms budget keeps the failing case fast offline.
        let dead = EndpointAddr {
            id: iroh::SecretKey::from_bytes(&[0x43; 32]).public(),
            addrs: [TransportAddr::Ip("127.0.0.1:1".parse().unwrap())]
                .into_iter()
                .collect(),
        };
        let client = QueueClient::new(cli, dead);
        assert!(client.probe(Duration::from_millis(500)).await.is_err());
    }
}
