#![forbid(unsafe_code)]

//! Vtessera coordinator — an opt-in queue/rendezvous for outbound-only nodes.
//!
//! Design: `docs/design/zero-config-connectivity.md`, **P1.5** and §4a/§4b/§4c.
//! The coordinator routes work; it is one of *many* (federation), it is
//! addressed by its Ed25519 public key, it signs what it emits, and — by
//! invariant 5b — it **can never touch money**. The message types in this
//! crate carry no escrow/payment/price fields and `serde` rejects any
//! unknown field (including a tampered-in `escrow`/`payout_id`), enforced by
//! [`CoordinatorPayload`]'s `deny_unknown_fields` and the `payloads_carry_no_
//! money_fields` test (design test 6a-7).
//!
//! Trust is opt-in: a node defaults to *no coordinator* and direct dial
//! (design §7c); pinning a coordinator here is an explicit operator choice.

use ed25519_dalek::{Signature, Signer, SigningKey, VerifyingKey};
use serde::{de::DeserializeOwned, Deserialize, Serialize};

/// iroh QUIC binding for the queue (feature-gated; `vtessera/0` ALPN).
#[cfg(feature = "serve")]
pub mod iroh;

/// Wire schema version for signed coordinator messages.
pub const COORDINATOR_SCHEMA_VER: u8 = 1;

/// Default lease duration granted to a registered node, in seconds.
pub const DEFAULT_LEASE_SECS: u64 = 900;

/// A coordinator is addressed by its Ed25519 public key, hex-encoded (§4b-1).
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
pub struct CoordinatorId(pub String);

impl CoordinatorId {
    /// Derive the coordinator identity from the same kind of 32-byte seeded
    /// key that the node's FR-M2 identity uses (`SecretKey::from_bytes(...)
    /// .public()`, §3).
    pub fn from_seed(seed: &[u8; 32]) -> Self {
        let pubkey = SigningKey::from_bytes(seed).verifying_key();
        CoordinatorId(hex::encode(pubkey.to_bytes()))
    }
}

/// Unique-within-one-coordinator id for jobs, leases, and registrations.
///
/// Per-coordinator namespace (§4b-3): two coordinators may independently
/// assign the same opaque id; the pair is disambiguated by the coordinator
/// pubkey, and `(coordinator, opaque)` is the only thing that may be treated
/// as unique.
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
pub struct ScopedId {
    pub coordinator: CoordinatorId,
    pub opaque: String,
}

impl ScopedId {
    pub fn new(coordinator: CoordinatorId, opaque: impl Into<String>) -> Self {
        Self {
            coordinator,
            opaque: opaque.into(),
        }
    }

    /// `"<coordinator_hex>:<opaque>"` — the canonical external form.
    pub fn display(&self) -> String {
        format!("{}:{}", self.coordinator.0, self.opaque)
    }
}

/// Per-coordinator opaque-id allocator (advisory uniqueness; §4b-3).
#[derive(Debug, Clone)]
pub struct IdAllocator {
    coordinator: CoordinatorId,
    next: u64,
}

impl IdAllocator {
    pub fn new(coordinator: CoordinatorId) -> Self {
        Self {
            coordinator,
            next: 1,
        }
    }

    /// Allocate the next opaque id tagged with `kind` (e.g. `"job"`, `"lease"`).
    pub fn allocate(&mut self, kind: &str) -> ScopedId {
        let opaque = format!("{kind}-{}", self.next);
        self.next += 1;
        ScopedId::new(self.coordinator.clone(), opaque)
    }
}

/// A coordinator-signed emission (§4b-2): payload bytes + the coordinator's
/// signature, so a node can attribute and audit any coordinator message.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct SignedMsg {
    pub schema_ver: u8,
    pub payload: Vec<u8>,
    pub sig_hex: String,
}

impl SignedMsg {
    pub fn sign(payload: &[u8], key: &SigningKey) -> Self {
        let sig = key.sign(payload);
        Self {
            schema_ver: COORDINATOR_SCHEMA_VER,
            payload: payload.to_vec(),
            sig_hex: hex::encode(sig.to_bytes()),
        }
    }

    /// Verify the signature against a coordinator's pubkey.
    pub fn verify(&self, pubkey_hex: &str) -> Result<(), VerifyError> {
        let pubkey_bytes: [u8; 32] = decode_fixed_hex(pubkey_hex, 32)
            .map_err(|_| VerifyError::BadPubkeyHex)?
            .try_into()
            .map_err(|_| VerifyError::BadPubkeyHex)?;
        let pubkey = VerifyingKey::from_bytes(&pubkey_bytes).map_err(|_| VerifyError::BadPubkey)?;
        let sig_bytes: [u8; 64] = decode_fixed_hex(&self.sig_hex, 64)
            .map_err(|_| VerifyError::BadSigHex)?
            .try_into()
            .map_err(|_| VerifyError::BadSigHex)?;
        pubkey
            .verify_strict(&self.payload, &Signature::from_bytes(&sig_bytes))
            .map_err(|_| VerifyError::SignatureMismatch)
    }

    /// Verify, then deserialize the payload.
    pub fn decode<T: DeserializeOwned>(&self, pubkey_hex: &str) -> Result<T, VerifyError> {
        self.verify(pubkey_hex)?;
        self.decode_unverified().ok().ok_or(VerifyError::BadPayload)
    }

    /// Deserialize the payload without verifying (audit/display consumers).
    pub fn decode_unverified<T: DeserializeOwned>(&self) -> Result<T, serde_json::Error> {
        serde_json::from_slice(&self.payload)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum VerifyError {
    BadPayload,
    BadPubkeyHex,
    BadPubkey,
    BadSigHex,
    SignatureMismatch,
}

/// Coordinator message payloads.
///
/// **No money, by construction (invariant 5b):** none of these variants can
/// carry a payment/escrow/price field, and `deny_unknown_fields` makes serde
/// reject any payload that tries to smuggle one in. Payment verification
/// stays node-local; settlement inputs are never parsed here.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case", deny_unknown_fields)]
pub enum CoordinatorPayload {
    /// Queue assignment: agents POST jobs here (§4a).
    QueueAssignment {
        queue_id: ScopedId,
        node: CoordinatorId,
    },
    /// A job routed to the node for dispatch. `job` is opaque to the
    /// coordinator — it never introspects job contents.
    DispatchOffer {
        job_id: ScopedId,
        job: serde_json::Value,
    },
    /// Node's acceptance of a dispatched job.
    DispatchAck {
        job_id: ScopedId,
        accepted: bool,
        reason: Option<String>,
    },
    /// Lease grant to a registered node (per-coordinator; T3.2).
    LeaseGrant {
        lease_id: ScopedId,
        node: CoordinatorId,
        expires_at_unix: u64,
    },
    /// Lease expiry / node-death receipt (per-coordinator, §4b-4).
    LeaseExpiry {
        lease_id: ScopedId,
        node: CoordinatorId,
        reason: String,
    },
    /// Advisory gating (§4b-5/6): this coordinator's own view, never global.
    Advisory {
        node: CoordinatorId,
        action: AdvisoryAction,
        reason: String,
    },
}

/// Advisory actions a coordinator may take against a node — all scoped to
/// that coordinator's own index; nothing is a global delist (§4b-5/6).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AdvisoryAction {
    /// Stop routing work to this node.
    RefuseWork,
    /// Revoke one of this coordinator's leases.
    RevokeLease { lease_id: ScopedId },
    /// Remove the node's offer from this coordinator's index.
    Delist,
}

/// Which dispatch path a node should take (§4c).
///
/// Deliberately has **no** failure/error variant — degradation cascades
/// A→B→direct and never fails closed (design test 6a-10: never a hard 503).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DispatchPath {
    /// Route through this coordinator.
    Relayed { via: CoordinatorId },
    /// Direct EndpointId-to-EndpointId dial (Phase 1 fallback; §4b-8).
    Direct,
}

/// Choose the first healthy registered coordinator; otherwise fall back to
/// direct dial. `healthy` lets a caller apply advisory gating (a coordinator
/// that refuses work is not "healthy").
pub fn choose_dispatch_path<F>(registered: &[CoordinatorId], healthy: &F) -> DispatchPath
where
    F: Fn(&CoordinatorId) -> bool,
{
    for coordinator in registered {
        if healthy(coordinator) {
            return DispatchPath::Relayed {
                via: coordinator.clone(),
            };
        }
    }
    DispatchPath::Direct
}

/// A coordinator's own, per-coordinator view of a node (§4b-5).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CoordinatorView {
    pub coordinator: CoordinatorId,
    /// This coordinator has stopped routing work to the node.
    pub refuses_work: bool,
    /// This coordinator removed the node's offer from its own index.
    pub delisted_here: bool,
}

impl CoordinatorView {
    /// A coordinator is a viable dispatch hop iff it is not refusing work
    /// for us (a delist from its own index is irrelevant to routing to it).
    pub fn healthy(&self) -> bool {
        !self.refuses_work
    }
}

/// Lifecycle of a per-coordinator lease (T3.2 / §4b-4).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum LeaseState {
    Granted { until_unix: u64 },
    Expired,
    Revoked,
}

/// A lease a coordinator grants a node for a slice of capacity.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Lease {
    pub id: ScopedId,
    pub coordinator: CoordinatorId,
    pub node: CoordinatorId,
    pub state: LeaseState,
}

/// A slice of node capacity a lease covers (node-side semantics).
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct NodeSlice(pub String);

/// The node is the sole holder of its execution state (§4b-4): a slice can be
/// reserved by exactly one active job, even when several coordinators
/// independently advertise the same node. This ledger is node-local.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct CapacityLedger {
    active: Vec<CapacityReservation>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CapacityReservation {
    pub job_id: ScopedId,
    pub lease_id: ScopedId,
    pub slice: NodeSlice,
    pub until_unix: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum CapacityError {
    /// The slice is already committed to another job — the double-commit guard
    /// across coordinators (§4b-4). The honest refusal, not a 503.
    SliceBusy(NodeSlice, ScopedId),
}

impl CapacityLedger {
    pub fn new() -> Self {
        Self { active: Vec::new() }
    }

    /// Reserve `slice` for `job_id` under `lease_id`. Fails iff the slice is
    /// already active for a different job (regardless of which coordinator
    /// issued the job or the lease).
    pub fn reserve(
        &mut self,
        job_id: ScopedId,
        lease_id: ScopedId,
        slice: NodeSlice,
        until_unix: u64,
    ) -> Result<(), CapacityError> {
        if let Some(existing) = self.active.iter().find(|r| r.slice == slice) {
            return Err(CapacityError::SliceBusy(slice, existing.job_id.clone()));
        }
        self.active.push(CapacityReservation {
            job_id,
            lease_id,
            slice,
            until_unix,
        });
        Ok(())
    }

    /// Release a reservation once its job finishes.
    pub fn release(&mut self, job_id: &ScopedId) {
        self.active.retain(|r| r.job_id != *job_id);
    }

    /// Drop reservations whose leases have lapsed.
    pub fn expire(&mut self, now: u64) {
        self.active.retain(|r| r.until_unix > now);
    }

    pub fn is_reserved(&self, slice: &NodeSlice) -> bool {
        self.active.iter().any(|r| r.slice == *slice)
    }
}

fn decode_fixed_hex(s: &str, len: usize) -> Result<Vec<u8>, ()> {
    let bytes = hex::decode(s).map_err(|_| ())?;
    if bytes.len() != len {
        return Err(());
    }
    Ok(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key_a() -> SigningKey {
        SigningKey::from_bytes(&[0x11; 32])
    }
    fn key_b() -> SigningKey {
        SigningKey::from_bytes(&[0x22; 32])
    }
    fn id_a() -> CoordinatorId {
        CoordinatorId::from_seed(&[0x11; 32])
    }
    fn id_b() -> CoordinatorId {
        CoordinatorId::from_seed(&[0x22; 32])
    }

    /// All payload variants, used by the no-money ser-de sweep (6a-7).
    fn all_payloads() -> Vec<CoordinatorPayload> {
        let node = id_a();
        let mut alloc = IdAllocator::new(id_b());
        let job_id = alloc.allocate("job");
        let lease_id = alloc.allocate("lease");
        vec![
            CoordinatorPayload::QueueAssignment {
                queue_id: alloc.allocate("queue"),
                node: node.clone(),
            },
            CoordinatorPayload::DispatchOffer {
                job_id: job_id.clone(),
                job: serde_json::json!({"image": "busybox"}),
            },
            CoordinatorPayload::DispatchAck {
                job_id: job_id.clone(),
                accepted: true,
                reason: None,
            },
            CoordinatorPayload::LeaseGrant {
                lease_id: lease_id.clone(),
                node: node.clone(),
                expires_at_unix: 1000,
            },
            CoordinatorPayload::LeaseExpiry {
                lease_id,
                node: node.clone(),
                reason: "node gone".into(),
            },
            CoordinatorPayload::Advisory {
                node,
                action: AdvisoryAction::RefuseWork,
                reason: "policy".into(),
            },
        ]
    }

    /// Test 6a-7 (ser-de half): no coordinator message serializes to any
    /// payment/escrow token, and unknown fields (incl. a smuggled `escrow`)
    /// are rejected rather than ignored.
    #[test]
    fn payloads_carry_no_money_fields() {
        const FORBIDDEN: &[&str] = &[
            "escrow",
            "payout",
            "payment",
            "amount_micros",
            "currency",
            "price",
            "money",
            "eurc",
            "usdc",
        ];
        for payload in all_payloads() {
            let json = serde_json::to_value(&payload).unwrap();
            let text = serde_json::to_string(&json).unwrap().to_ascii_lowercase();
            for token in FORBIDDEN {
                assert!(
                    !text.contains(token),
                    "payload {payload:?} leaked payment token {token:?}"
                );
            }
        }
    }

    #[test]
    fn smuggled_payment_fields_are_rejected_by_serde() {
        let payload = CoordinatorPayload::DispatchOffer {
            job_id: ScopedId::new(id_b(), "job-1"),
            job: serde_json::json!({"image": "busybox"}),
        };
        let mut json = serde_json::to_value(&payload).unwrap();
        if let serde_json::Value::Object(map) = &mut json {
            map.insert("escrow_account".into(), serde_json::json!("0xbeef"));
            map.insert("payout_id".into(), serde_json::json!("0xcafe"));
        }
        let res: Result<CoordinatorPayload, _> = serde_json::from_value(json);
        assert!(
            res.is_err(),
            "serde must reject a payload smuggling escrow/payout fields"
        );
    }

    /// Test 6a-7 (federation half): two coordinators can independently assign
    /// the same opaque id without collision (§4b-3).
    #[test]
    fn coordinators_independently_assign_same_opaque_id() {
        let mut a = IdAllocator::new(id_a());
        let mut b = IdAllocator::new(id_b());
        let job_a = a.allocate("job");
        let job_b = b.allocate("job");
        assert_eq!(job_a.opaque, job_b.opaque);
        assert_ne!(job_a.coordinator, job_b.coordinator);
        assert_ne!(job_a, job_b);
        assert_eq!(job_a.display(), format!("{}:job-1", id_a().0));
        assert_eq!(job_b.display(), format!("{}:job-1", id_b().0));
    }

    #[test]
    fn allocator_scopes_opaque_ids_per_coordinator() {
        let mut a = IdAllocator::new(id_a());
        // Same coordinator + same opaque id -> same ScopedId.
        let mut fresh_a = IdAllocator::new(id_a());
        assert_eq!(a.allocate("job"), fresh_a.allocate("job"));
        // Opaque types stay distinct within one coordinator.
        let mut b = IdAllocator::new(id_a());
        assert_ne!(b.allocate("job"), b.allocate("lease"));
    }

    /// Test 6a-3-style sign/verify for coordinator emissions (§4b-2).
    #[test]
    fn signed_msg_roundtrip_and_forgery_rejection() {
        let key = key_b();
        let payload = serde_json::to_vec(&CoordinatorPayload::LeaseGrant {
            lease_id: ScopedId::new(id_b(), "lease-1"),
            node: id_a(),
            expires_at_unix: 1000,
        })
        .unwrap();
        let signed = SignedMsg::sign(&payload, &key);
        assert!(signed.verify(&id_b().0).is_ok());

        let decoded: CoordinatorPayload = signed.decode(&id_b().0).unwrap();
        assert!(matches!(decoded, CoordinatorPayload::LeaseGrant { .. }));

        // Wrong pubkey must fail.
        assert_eq!(
            signed.verify(&id_a().0),
            Err(VerifyError::SignatureMismatch)
        );

        // Tampered payload must fail.
        let mut forged = signed.clone();
        forged.payload[0] ^= 0xff;
        assert_eq!(
            forged.verify(&id_b().0),
            Err(VerifyError::SignatureMismatch)
        );

        // Bad hex must fail with the right error.
        assert_eq!(signed.verify("zzz"), Err(VerifyError::BadPubkeyHex));
    }

    /// Test 6a-10 (dispatch half): degradation A→B→direct, never a 503.
    /// `DispatchPath` has no error variant, and every input combination below
    /// yields a valid path.
    #[test]
    fn degradation_a_to_b_to_direct_never_errors() {
        let both = [id_a(), id_b()];
        fn healthy_a(who: &CoordinatorId) -> bool {
            who == &id_a()
        }
        fn healthy_b(who: &CoordinatorId) -> bool {
            who == &id_b()
        }
        fn healthy_none(_: &CoordinatorId) -> bool {
            false
        }
        assert_eq!(
            choose_dispatch_path(&both, &healthy_a),
            DispatchPath::Relayed { via: id_a() }
        );

        assert_eq!(
            choose_dispatch_path(&both, &healthy_b),
            DispatchPath::Relayed { via: id_b() }
        );

        assert_eq!(
            choose_dispatch_path(&both, &healthy_none),
            DispatchPath::Direct
        );

        assert_eq!(
            choose_dispatch_path(&[], &healthy_none),
            DispatchPath::Direct
        );
        // The classic "single coordinator available" degrade.
        assert_eq!(
            choose_dispatch_path(&[id_a()], &healthy_none),
            DispatchPath::Direct
        );
    }

    /// Test 6a-10 (advisory half): a coordinator refusing/delisting does not
    /// eject the node globally and does not affect a second coordinator.
    #[test]
    fn advisory_gating_is_per_coordinator() {
        let a = CoordinatorView {
            coordinator: id_a(),
            refuses_work: true,
            delisted_here: true,
        };
        let b = CoordinatorView {
            coordinator: id_b(),
            refuses_work: false,
            delisted_here: false,
        };

        // A's refusal only affects choosing A.
        assert!(!a.healthy());
        assert!(b.healthy());

        let registered = [a.coordinator.clone(), b.coordinator.clone()];
        let healthy = |who: &CoordinatorId| {
            let view = if who == &a.coordinator { &a } else { &b };
            view.healthy()
        };
        // Node still routes through B, whose index still lists it.
        assert_eq!(
            choose_dispatch_path(&registered, &healthy),
            DispatchPath::Relayed { via: id_b() }
        );

        // Gating is the node's call, not a global ejection: the node decides.
        assert!(b.healthy());
    }

    /// Test 6a-10 (lease half): lease-expiry receipts are per-coordinator
    /// and cannot travel across coordinators.
    #[test]
    fn lease_expiry_receipt_is_per_coordinator() {
        let key_a = key_a();
        let receipt_issue = |key: &SigningKey, coordinator: &CoordinatorId| {
            SignedMsg::sign(
                &serde_json::to_vec(&CoordinatorPayload::LeaseExpiry {
                    lease_id: ScopedId::new(coordinator.clone(), "lease-9"),
                    node: id_a(),
                    reason: "node gone".into(),
                })
                .unwrap(),
                key,
            )
        };
        let from_a = receipt_issue(&key_a, &id_a());
        // Verifies under A; fails under B — A's receipt can't be presented as B's.
        assert!(from_a.verify(&id_a().0).is_ok());
        assert_eq!(
            from_a.verify(&id_b().0),
            Err(VerifyError::SignatureMismatch)
        );
    }

    /// §4b-4 / 6a-7 capacity: one slice, one job — even across coordinators.
    #[test]
    fn capacity_has_no_double_commit_across_coordinators() {
        let mut ledger = CapacityLedger::new();
        let mut a = IdAllocator::new(id_a());
        let mut b = IdAllocator::new(id_b());
        let job_a = a.allocate("job");
        let lease_a = a.allocate("lease");
        let job_b = b.allocate("job");
        let lease_b = b.allocate("lease");

        ledger
            .reserve(
                job_a.clone(),
                lease_a.clone(),
                NodeSlice("s1".into()),
                10_000,
            )
            .unwrap();
        // Same slice, different coordinator, different job/lease → rejected.
        assert_eq!(
            ledger.reserve(job_b, lease_b, NodeSlice("s1".into()), 10_000),
            Err(CapacityError::SliceBusy(
                NodeSlice("s1".into()),
                job_a.clone()
            ))
        );
        // A different slice is fine.
        ledger
            .reserve(
                job_a.clone(),
                lease_a.clone(),
                NodeSlice("s2".into()),
                10_000,
            )
            .unwrap();

        // Lease expiry clears the capacity.
        ledger.expire(20_000);
        assert!(!ledger.is_reserved(&NodeSlice("s1".into())));

        // Release also frees.
        ledger
            .reserve(
                job_a.clone(),
                lease_a.clone(),
                NodeSlice("s1".into()),
                10_000,
            )
            .unwrap();
        ledger.release(&job_a);
        assert!(!ledger.is_reserved(&NodeSlice("s1".into())));
    }
}
