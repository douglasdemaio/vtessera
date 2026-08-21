#![allow(dead_code)]
/// Hand-rolled CIDR parser — no new dependencies (BUILD.md §1.3).
///
/// Parses `x.x.x.x/N` notation and checks whether an IPv4 address falls
/// within any of a set of CIDR ranges. The executor crate has similar
/// machinery for guest VM network policy; this is the same pattern without
/// pulling in the `ipnet` crate.
use std::fmt;

/// A parsed IPv4 CIDR range: base address + prefix length.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct IpNet {
    addr: u32,
    prefix_len: u8,
}

impl IpNet {
    /// Parse a CIDR string like `"10.0.0.0/8"`.
    pub fn parse(s: &str) -> Result<Self, CidrError> {
        let (addr_str, prefix_str) = s
            .split_once('/')
            .ok_or_else(|| CidrError::InvalidFormat(s.to_string()))?;

        let addr = parse_ipv4(addr_str)?;
        let prefix_len: u8 = prefix_str
            .parse()
            .map_err(|_| CidrError::InvalidPrefix(s.to_string()))?;

        if prefix_len > 32 {
            return Err(CidrError::InvalidPrefix(s.to_string()));
        }

        Ok(IpNet { addr, prefix_len })
    }

    /// Check whether an IPv4 address (as u32) falls within this CIDR.
    pub fn contains(&self, ip: u32) -> bool {
        if self.prefix_len == 0 {
            return true; // /0 matches everything
        }
        let mask = !0u32 << (32 - self.prefix_len);
        (ip & mask) == (self.addr & mask)
    }
}

/// Parse a dotted-decimal IPv4 address to a u32.
fn parse_ipv4(s: &str) -> Result<u32, CidrError> {
    let parts: Vec<&str> = s.split('.').collect();
    if parts.len() != 4 {
        return Err(CidrError::InvalidAddress(s.to_string()));
    }
    let mut addr = 0u32;
    for part in parts {
        let octet: u8 = part
            .parse()
            .map_err(|_| CidrError::InvalidAddress(s.to_string()))?;
        addr = (addr << 8) | octet as u32;
    }
    Ok(addr)
}

/// Convert an IPv4 address string to a u32 for matching.
pub fn ipv4_to_u32(s: &str) -> Result<u32, CidrError> {
    parse_ipv4(s)
}

/// Check whether an IPv4 address (as dotted-decimal string) falls within
/// any of the given CIDR ranges. An empty `cidrs` list returns `true`
/// (no restriction — private mode without CIDR enforcement).
pub fn ip_in_cidrs(ip: &str, cidrs: &[IpNet]) -> bool {
    if cidrs.is_empty() {
        return true;
    }
    let ip_u32 = match ipv4_to_u32(ip) {
        Ok(v) => v,
        Err(_) => return false,
    };
    cidrs.iter().any(|cidr| cidr.contains(ip_u32))
}

/// Parse a list of CIDR strings, returning the first error encountered.
pub fn parse_cidr_list(ss: &[String]) -> Result<Vec<IpNet>, CidrError> {
    ss.iter().map(|s| IpNet::parse(s)).collect()
}

#[derive(Debug, Clone, PartialEq, Eq)]
#[allow(clippy::enum_variant_names)]
pub enum CidrError {
    InvalidFormat(String),
    InvalidAddress(String),
    InvalidPrefix(String),
}

impl fmt::Display for CidrError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            CidrError::InvalidFormat(s) => {
                write!(f, "invalid CIDR format (expected x.x.x.x/N): {s}")
            }
            CidrError::InvalidAddress(s) => write!(f, "invalid IPv4 address: {s}"),
            CidrError::InvalidPrefix(s) => write!(f, "invalid prefix length: {s}"),
        }
    }
}

impl std::error::Error for CidrError {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_simple_cidr() {
        let net = IpNet::parse("10.0.0.0/8").unwrap();
        assert!(net.contains(ipv4_to_u32("10.1.2.3").unwrap()));
        assert!(!net.contains(ipv4_to_u32("192.168.1.1").unwrap()));
    }

    #[test]
    fn parse_class_c() {
        let net = IpNet::parse("192.168.1.0/24").unwrap();
        assert!(net.contains(ipv4_to_u32("192.168.1.0").unwrap()));
        assert!(net.contains(ipv4_to_u32("192.168.1.255").unwrap()));
        assert!(!net.contains(ipv4_to_u32("192.168.2.0").unwrap()));
    }

    #[test]
    fn parse_slash_32() {
        let net = IpNet::parse("10.0.0.1/32").unwrap();
        assert!(net.contains(ipv4_to_u32("10.0.0.1").unwrap()));
        assert!(!net.contains(ipv4_to_u32("10.0.0.2").unwrap()));
    }

    #[test]
    fn parse_slash_0() {
        let net = IpNet::parse("0.0.0.0/0").unwrap();
        assert!(net.contains(ipv4_to_u32("1.2.3.4").unwrap()));
        assert!(net.contains(ipv4_to_u32("255.255.255.255").unwrap()));
    }

    #[test]
    fn reject_missing_slash() {
        assert!(IpNet::parse("10.0.0.0").is_err());
    }

    #[test]
    fn reject_bad_octet() {
        assert!(IpNet::parse("256.0.0.0/8").is_err());
    }

    #[test]
    fn reject_prefix_too_large() {
        assert!(IpNet::parse("10.0.0.0/33").is_err());
    }

    #[test]
    fn ip_in_cidrs_empty_list_matches() {
        assert!(ip_in_cidrs("10.0.0.1", &[]));
    }

    #[test]
    fn ip_in_cidrs_match() {
        let cidrs = parse_cidr_list(&["10.0.0.0/8".into(), "172.16.0.0/12".into()]).unwrap();
        assert!(ip_in_cidrs("10.1.2.3", &cidrs));
        assert!(ip_in_cidrs("172.16.0.1", &cidrs));
        assert!(!ip_in_cidrs("192.168.1.1", &cidrs));
    }

    #[test]
    fn parse_multiple_cidrs() {
        let cidrs = parse_cidr_list(&["10.0.0.0/8".into(), "192.168.0.0/16".into()]).unwrap();
        assert_eq!(cidrs.len(), 2);
    }

    #[test]
    fn parse_rejects_invalid_in_list() {
        let err = parse_cidr_list(&["10.0.0.0/8".into(), "bad".into()]).unwrap_err();
        assert!(matches!(err, CidrError::InvalidFormat(_)));
    }
}
