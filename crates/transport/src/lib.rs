#![forbid(unsafe_code)]

//! Vtessera transport layer — pluggable connectivity for internet-scale discovery.
//!
//! Nodes advertise which transports they support. Agents pick the best available.
//! The index stores transport information; agents and nodes negotiate directly.

use serde::{Deserialize, Serialize};

/// How to reach a node over the network.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum TransportKind {
    /// Direct QUIC connection (preferred for internet).
    QuicDirect,
    /// Tailscale WireGuard tunnel.
    Tailscale,
    /// HTTP/HTTPS fallback.
    Https,
    /// LAN only (mDNS, no internet).
    LocalOnly,
}

/// Where a candidate address was discovered.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum CandidateKind {
    /// Local interface address.
    Host,
    /// STUN-derived public IP:port.
    ServerReflexive,
    /// Discovered during connectivity check.
    PeerReflexive,
    /// TURN relay address.
    Relayed,
}

/// A connection candidate — one possible way to reach a node.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct Candidate {
    pub kind: CandidateKind,
    pub transport: TransportKind,
    /// IP:port to try.
    pub addr: String,
    /// Higher = try first. LAN(200) > Tailscale(150) > STUN(100) > Relay(50).
    pub priority: u32,
}

/// Default STUN servers for v0 (public, free).
pub const DEFAULT_STUN_SERVERS: &[&str] = &["stun.l.google.com:19302", "stun.cloudflare.com:3478"];

/// Default heartbeat interval in seconds.
pub const DEFAULT_HEARTBEAT_SECS: u64 = 30;

/// Default entry TTL in the index (must be > heartbeat interval).
pub const DEFAULT_ENTRY_TTL_SECS: u64 = 120;

/// Probe a STUN server to discover the reflexive (public) address.
///
/// Sends a STUN Binding Request (RFC 8489 §15) and parses the
/// XOR-MAPPED-ADDRESS from the response. Returns the public IP:port.
///
/// This is a minimal STUN client — just enough for reflexive address
/// discovery. Full ICE/TURN lives in a future version.
pub fn stun_probe(server: &str) -> Result<String, StunError> {
    use std::net::UdpSocket;

    let socket = UdpSocket::bind("0.0.0.0:0").map_err(StunError::Io)?;
    socket
        .set_read_timeout(Some(std::time::Duration::from_secs(3)))
        .map_err(StunError::Io)?;
    socket.connect(server).map_err(StunError::Io)?;

    // STUN Binding Request (RFC 8489 §6)
    let transaction_id: [u8; 12] = rand::random();
    let msg_type: u16 = 0x0001; // Binding Request
    let msg_len: u16 = 0; // no attributes
    let magic_cookie: u32 = 0x2112A442;

    let mut req = Vec::with_capacity(20);
    req.extend_from_slice(&msg_type.to_be_bytes());
    req.extend_from_slice(&msg_len.to_be_bytes());
    req.extend_from_slice(&magic_cookie.to_be_bytes());
    req.extend_from_slice(&transaction_id);

    socket.send(&req).map_err(StunError::Io)?;

    let mut resp = [0u8; 128];
    let n = socket.recv(&mut resp).map_err(StunError::Io)?;

    if n < 20 {
        return Err(StunError::ResponseTooShort);
    }

    // Verify magic cookie
    let resp_magic = u32::from_be_bytes(resp[4..8].try_into().unwrap());
    if resp_magic != magic_cookie {
        return Err(StunError::BadMagicCookie);
    }

    // Verify transaction ID matches
    if resp[8..20] != transaction_id {
        return Err(StunError::TransactionMismatch);
    }

    // Check message type (Binding Response = 0x0101)
    let resp_type = u16::from_be_bytes(resp[0..2].try_into().unwrap());
    if resp_type != 0x0101 {
        let error_code = u16::from_be_bytes(resp[2..4].try_into().unwrap());
        return Err(StunError::ServerError(error_code));
    }

    // Parse attributes to find XOR-MAPPED-ADDRESS (0x0020)
    let resp_len = u16::from_be_bytes(resp[2..4].try_into().unwrap()) as usize;
    let mut offset = 20;
    while offset + 4 <= 20 + resp_len && offset + 4 <= n {
        let attr_type = u16::from_be_bytes(resp[offset..offset + 2].try_into().unwrap());
        let attr_len =
            u16::from_be_bytes(resp[offset + 2..offset + 4].try_into().unwrap()) as usize;

        if attr_type == 0x0020 {
            // XOR-MAPPED-ADDRESS
            if offset + 4 + attr_len > n {
                return Err(StunError::AttributeTruncated);
            }
            let family = resp[offset + 5];
            if family == 0x01 {
                // IPv4
                let xored_port =
                    u16::from_be_bytes(resp[offset + 6..offset + 8].try_into().unwrap());
                let port = xored_port ^ (magic_cookie >> 16) as u16;
                let mut ip_bytes = [0u8; 4];
                ip_bytes.copy_from_slice(&resp[offset + 8..offset + 12]);
                // XOR with magic cookie
                let magic_bytes = magic_cookie.to_be_bytes();
                for (b, m) in ip_bytes.iter_mut().zip(magic_bytes.iter()) {
                    *b ^= m;
                }
                return Ok(format!(
                    "{}.{}.{}.{}:{}",
                    ip_bytes[0], ip_bytes[1], ip_bytes[2], ip_bytes[3], port
                ));
            }
        }

        // Advance to next attribute (padded to 4 bytes)
        let padded = (attr_len + 3) & !3;
        offset += 4 + padded;
    }

    Err(StunError::NoXorMappedAddress)
}

/// Probe multiple STUN servers and return the first successful reflexive address.
pub fn discover_reflexive_addr() -> Result<String, StunError> {
    for server in DEFAULT_STUN_SERVERS {
        match stun_probe(server) {
            Ok(addr) => return Ok(addr),
            Err(e) => {
                eprintln!("STUN probe {server} failed: {e}");
                continue;
            }
        }
    }
    Err(StunError::AllServersFailed)
}

/// Build the default candidate set for a node given its LAN IP and port.
/// Reflexive address is discovered via STUN; LAN is always included.
pub fn gather_candidates(lan_ip: &str, port: u16) -> Vec<Candidate> {
    let mut candidates = vec![
        Candidate {
            kind: CandidateKind::Host,
            transport: TransportKind::LocalOnly,
            addr: format!("{lan_ip}:{port}"),
            priority: 200,
        },
        Candidate {
            kind: CandidateKind::Host,
            transport: TransportKind::QuicDirect,
            addr: format!("{lan_ip}:{port}"),
            priority: 200,
        },
    ];

    if let Ok(reflexive) = discover_reflexive_addr() {
        eprintln!("STUN: reflexive address {reflexive}");
        candidates.push(Candidate {
            kind: CandidateKind::ServerReflexive,
            transport: TransportKind::QuicDirect,
            addr: reflexive,
            priority: 100,
        });
    }

    candidates
}

#[derive(Debug)]
pub enum StunError {
    Io(std::io::Error),
    ResponseTooShort,
    BadMagicCookie,
    TransactionMismatch,
    ServerError(u16),
    AttributeTruncated,
    NoXorMappedAddress,
    AllServersFailed,
}

impl std::fmt::Display for StunError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            StunError::Io(e) => write!(f, "IO: {e}"),
            StunError::ResponseTooShort => write!(f, "STUN response too short"),
            StunError::BadMagicCookie => write!(f, "STUN bad magic cookie"),
            StunError::TransactionMismatch => write!(f, "STUN transaction ID mismatch"),
            StunError::ServerError(code) => write!(f, "STUN server error: {code}"),
            StunError::AttributeTruncated => write!(f, "STUN attribute truncated"),
            StunError::NoXorMappedAddress => write!(f, "STUN no XOR-MAPPED-ADDRESS in response"),
            StunError::AllServersFailed => write!(f, "all STUN servers failed"),
        }
    }
}

impl std::error::Error for StunError {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn candidate_serializes_flat() {
        let c = Candidate {
            kind: CandidateKind::ServerReflexive,
            transport: TransportKind::QuicDirect,
            addr: "203.0.113.1:8402".into(),
            priority: 100,
        };
        let json = serde_json::to_string(&c).unwrap();
        assert!(json.contains("\"kind\":\"server_reflexive\""));
        assert!(json.contains("\"transport\":\"quic_direct\""));
        assert!(json.contains("\"addr\":\"203.0.113.1:8402\""));
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

    #[test]
    fn gather_candidates_includes_lan() {
        let candidates = gather_candidates("192.168.1.100", 8402);
        assert!(candidates.iter().any(|c| c.addr == "192.168.1.100:8402"));
    }
}
