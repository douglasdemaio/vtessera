#![forbid(unsafe_code)]

//! Vtessera transport layer — pluggable connectivity for internet-scale discovery.
//!
//! With iroh integration, connectivity is handled by the iroh endpoint.
//! This crate provides the type definitions that the offer-index and
//! node-api use to describe how to reach a node.

use serde::{Deserialize, Serialize};

/// How to reach a node over the network.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum TransportKind {
    /// iroh QUIC connection (preferred for internet).
    IrohQuic,
    /// Tailscale WireGuard tunnel.
    Tailscale,
    /// HTTP/HTTPS fallback (LAN).
    Https,
    /// LAN only (no internet).
    LocalOnly,
}

/// Where a candidate address was discovered.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum CandidateKind {
    /// Local interface address.
    Host,
    /// Discovered during connectivity check.
    PeerReflexive,
    /// iroh relay address.
    Relayed,
}

/// A connection candidate — one possible way to reach a node.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct Candidate {
    pub kind: CandidateKind,
    pub transport: TransportKind,
    /// IP:port or relay URL to try.
    pub addr: String,
    /// Higher = try first. LAN(200) > Iroh(150) > Relay(50).
    pub priority: u32,
}

/// Default heartbeat interval in seconds.
pub const DEFAULT_HEARTBEAT_SECS: u64 = 30;

/// Default entry TTL in the index (must be > heartbeat interval).
pub const DEFAULT_ENTRY_TTL_SECS: u64 = 120;

/// iroh connectivity sidecar (feature-gated).
#[cfg(feature = "serve")]
pub mod iroh_sidecar;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn candidate_serializes_flat() {
        let c = Candidate {
            kind: CandidateKind::Relayed,
            transport: TransportKind::IrohQuic,
            addr: "https://relay.example.com".into(),
            priority: 50,
        };
        let json = serde_json::to_string(&c).unwrap();
        assert!(json.contains("\"kind\":\"relayed\""));
        assert!(json.contains("\"transport\":\"iroh_quic\""));
        assert!(json.contains("\"addr\":\"https://relay.example.com\""));
    }

    #[test]
    fn candidate_roundtrips_through_json() {
        let c = Candidate {
            kind: CandidateKind::Host,
            transport: TransportKind::Tailscale,
            addr: "100.64.0.1:8402".into(),
            priority: 150,
        };
        let json = serde_json::to_string(&c).unwrap();
        let back: Candidate = serde_json::from_str(&json).unwrap();
        assert_eq!(c, back);
    }
}
