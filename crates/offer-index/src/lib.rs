//! Vtessera offer index — Module 2a (ROADMAP.md §2a).
//!
//! Agents can't know every node URL ahead of time, so a central index
//! answers "who is selling compute right now, and for what?" A node
//! publishes a signed offer (crates/offer); this index **verifies** the
//! signature and expiry before it holds the offer, keeps only current
//! offers, and serves them to agents.
//!
//! Pure dispatch: this lib opens no sockets and pulls no network stack.
//! The `serve` feature wires a tiny HTTP server + a seed poller in the
//! bin. A consumer can also drive [`dispatch`] directly.
//!
//! Trust model: the index does not trust the *content* of an offer, only
//! that it is correctly signed by the key whose node_id it claims. Nodes
//! register by POSTing their signed offer; the index verifies and stores.
//! Push is the primary path; pull (seed) mode lets a fresh index boot
//! from known nodes without waiting for them to push.

#![forbid(unsafe_code)]

use std::collections::BTreeMap;

use serde_json::{json, Value};
use vtessera_mini_http::{Method, Request, Response};
use vtessera_offer::{verify, AdvertisedDevice, PriceQuote, SignedOffer, VerifyError};

/// Default first-come-first-served claim lifetime, in seconds.
pub const DEFAULT_CLAIM_TTL_SECS: u64 = 60;

/// One verified, current offer in the index.
#[derive(Debug, Clone)]
pub struct IndexEntry {
    pub offer: SignedOffer,
    /// Where the offer came from, for debugging: a node URL (pull mode) or
    /// `"push"` (register API).
    pub source: String,
    /// UNIX epoch second the entry was (re)registered.
    pub fetched_at_unix: u64,
    /// Agent id that claimed this offer, or `None` when unclaimed.
    pub claimed_by: Option<String>,
    /// UNIX epoch second the current claim expires. `0` when unclaimed.
    pub claim_until_unix: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FilterMode {
    Free,
    Paid,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FilterDevice {
    Cpu,
    NvidiaGpu,
    NvidiaMig,
    AmdGpu,
}

#[derive(Debug, Clone, Default)]
pub struct OfferFilter {
    pub mode: Option<FilterMode>,
    pub device: Option<FilterDevice>,
    /// Only entries with no live claim.
    pub available: bool,
}

impl OfferFilter {
    fn matches(&self, entry: &IndexEntry, now_unix: u64) -> bool {
        let offer = &entry.offer;
        let mode_ok = match self.mode {
            None => true,
            Some(FilterMode::Free) => matches!(offer.body.price, PriceQuote::Free),
            Some(FilterMode::Paid) => matches!(offer.body.price, PriceQuote::Paid { .. }),
        };
        let device_ok = match self.device {
            None => true,
            Some(d) => device_kind(&offer.body.device) == d,
        };
        let available_ok = !self.available || entry.claim_until_unix < now_unix;
        mode_ok && device_ok && available_ok
    }
}

fn device_kind(d: &AdvertisedDevice) -> FilterDevice {
    match d {
        AdvertisedDevice::Cpu { .. } => FilterDevice::Cpu,
        AdvertisedDevice::NvidiaGpu { .. } => FilterDevice::NvidiaGpu,
        AdvertisedDevice::NvidiaMig { .. } => FilterDevice::NvidiaMig,
        AdvertisedDevice::AmdGpu { .. } => FilterDevice::AmdGpu,
    }
}

#[derive(Debug)]
pub enum RegisterError {
    /// Body wasn't valid offer JSON at all.
    BadJson(String),
    /// The offer didn't verify (see [`VerifyError`]).
    Verify(VerifyError),
}

impl std::fmt::Display for RegisterError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RegisterError::BadJson(e) => write!(f, "invalid signed offer JSON: {e}"),
            RegisterError::Verify(e) => write!(f, "offer rejected: {e}"),
        }
    }
}

impl std::error::Error for RegisterError {}

/// Why a claim operation failed.
#[derive(Debug)]
pub enum ClaimError {
    /// The node_id has no live offer (unknown or expired).
    NotFound,
    /// The offer is claimed by a different agent and the claim is live.
    Taken(String),
    /// The caller is not the current claimant and the claim is live.
    NotOwner,
}

impl std::fmt::Display for ClaimError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ClaimError::NotFound => write!(f, "no offer for this node_id"),
            ClaimError::Taken(agent) => write!(f, "claimed by {agent}"),
            ClaimError::NotOwner => write!(f, "claim held by a different agent"),
        }
    }
}

impl std::error::Error for ClaimError {}

/// The index. Keyed by `node_id` (a signed offer carries exactly one);
/// re-registering the same node id replaces the prior entry.
#[derive(Debug, Default)]
pub struct IndexState {
    entries: BTreeMap<String, IndexEntry>,
}

impl IndexState {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn count(&self) -> usize {
        self.entries.len()
    }

    pub fn get(&self, node_id: &str) -> Option<&IndexEntry> {
        self.entries.get(node_id)
    }

    /// Drop expired entries and clear expired claims; returns how many
    /// entries were removed (cleared claims are not counted).
    pub fn prune(&mut self, now_unix: u64) -> usize {
        let before = self.entries.len();
        self.entries
            .retain(|_, e| e.offer.body.expires_unix >= now_unix);
        for e in self.entries.values_mut() {
            if e.claim_until_unix != 0 && e.claim_until_unix < now_unix {
                e.claimed_by = None;
                e.claim_until_unix = 0;
            }
        }
        before - self.entries.len()
    }

    pub fn remove(&mut self, node_id: &str) -> bool {
        self.entries.remove(node_id).is_some()
    }

    /// Verify a signed offer against `now_unix` (signature + node_id +
    /// expiry) and insert it. Returns the `node_id` it's held under.
    ///
    /// Re-registering the same `node_id` (a node's publish refresh)
    /// **preserves** an active claim, so a refresh never wipes a claim.
    pub fn register(
        &mut self,
        offer: SignedOffer,
        source: String,
        now_unix: u64,
    ) -> Result<String, RegisterError> {
        verify(&offer, Some(now_unix)).map_err(RegisterError::Verify)?;
        let node_id = offer.body.node_id.clone();
        let prior = self.entries.get(&node_id);
        let (claimed_by, claim_until_unix) = match prior {
            Some(prev) if prev.claim_until_unix >= now_unix => {
                (prev.claimed_by.clone(), prev.claim_until_unix)
            }
            _ => (None, 0),
        };
        self.entries.insert(
            node_id.clone(),
            IndexEntry {
                offer,
                source,
                fetched_at_unix: now_unix,
                claimed_by,
                claim_until_unix,
            },
        );
        Ok(node_id)
    }

    /// [`Self::register`] from a JSON body.
    pub fn register_json(
        &mut self,
        body: &str,
        source: String,
        now_unix: u64,
    ) -> Result<String, RegisterError> {
        let offer: SignedOffer =
            serde_json::from_str(body).map_err(|e| RegisterError::BadJson(e.to_string()))?;
        self.register(offer, source, now_unix)
    }

    /// Live, pruned view of entries matching `filter`, newest first.
    pub fn list(&mut self, now_unix: u64, filter: &OfferFilter) -> Vec<&IndexEntry> {
        self.prune(now_unix);
        let mut out: Vec<&IndexEntry> = self
            .entries
            .values()
            .filter(|e| filter.matches(e, now_unix))
            .collect();
        out.sort_by_key(|e| std::cmp::Reverse(e.fetched_at_unix));
        out
    }

    /// First-come-first-served claim. Unknown or expired node → NotFound;
    /// claimed by a different agent with a live claim → Taken; otherwise
    /// claim (or renew) for `agent_id` until `now_unix + ttl_secs`.
    pub fn claim(
        &mut self,
        node_id: &str,
        agent_id: &str,
        now_unix: u64,
        ttl_secs: u64,
    ) -> Result<u64, ClaimError> {
        self.prune(now_unix);
        let Some(entry) = self.entries.get_mut(node_id) else {
            return Err(ClaimError::NotFound);
        };
        let live = entry.claim_until_unix >= now_unix;
        if live {
            if let Some(owner) = &entry.claimed_by {
                if owner != agent_id {
                    return Err(ClaimError::Taken(owner.clone()));
                }
            }
        }
        let until = now_unix + ttl_secs;
        entry.claimed_by = Some(agent_id.to_string());
        entry.claim_until_unix = until;
        Ok(until)
    }

    /// Release a claim. Only the current claimant may release while the
    /// claim is live.
    pub fn release(
        &mut self,
        node_id: &str,
        agent_id: &str,
        now_unix: u64,
    ) -> Result<(), ClaimError> {
        self.prune(now_unix);
        let Some(entry) = self.entries.get_mut(node_id) else {
            return Err(ClaimError::NotFound);
        };
        match &entry.claimed_by {
            None => Err(ClaimError::NotFound),
            Some(owner) if owner == agent_id => {
                entry.claimed_by = None;
                entry.claim_until_unix = 0;
                Ok(())
            }
            Some(_) => Err(ClaimError::NotOwner),
        }
    }
}

/// Route one HTTP request. `now_unix` is the index's clock — supplied by
/// the caller so the lib stays clock-free.
pub fn dispatch(state: &mut IndexState, req: Request, now_unix: u64) -> Response {
    let (path, query) = match req.path.split_once('?') {
        Some((p, q)) => (p, Some(q)),
        None => (req.path.as_str(), None),
    };

    match (req.method, path) {
        (Method::Get, "/healthz") => Response::text(200, "ok"),
        (Method::Get, "/offers") => handle_list(state, query, now_unix),
        (Method::Post, "/offers") => handle_register(state, &req.body, now_unix),
        _ => match path.strip_prefix("/offers/") {
            Some(rest) if !rest.is_empty() => match rest.strip_suffix("/claim") {
                Some(node_id) if !node_id.is_empty() => match req.method {
                    Method::Post => handle_claim(state, node_id, &req.body, now_unix),
                    Method::Delete => handle_release(state, node_id, &req.body, now_unix),
                    _ => Response::text(404, "not found"),
                },
                _ => match rest.split('/').next() {
                    Some(node_id) if !node_id.is_empty() => match req.method {
                        Method::Get => {
                            state.prune(now_unix);
                            match state.get(node_id) {
                                Some(entry) => Response::json(200, entry_to_json(entry)),
                                None => Response::text(404, "no offer for this node_id"),
                            }
                        }
                        Method::Delete => {
                            if state.remove(node_id) {
                                Response::json(200, r#"{"status":"removed"}"#.into())
                            } else {
                                Response::text(404, "no offer for this node_id")
                            }
                        }
                        _ => Response::text(404, "not found"),
                    },
                    _ => Response::text(404, "not found"),
                },
            },
            _ => Response::text(404, "not found"),
        },
    }
}

fn handle_list(state: &mut IndexState, query: Option<&str>, now_unix: u64) -> Response {
    let filter = parse_filter(query);
    let entries = state.list(now_unix, &filter);
    let offers: Vec<Value> = entries.iter().map(|e| entry_to_value(e)).collect();
    let body = serde_json::to_string(&json!({ "count": offers.len(), "offers": offers }))
        .unwrap_or_else(|_| r#"{"count":0,"offers":[]}"#.into());
    Response::json(200, body)
}

fn handle_register(state: &mut IndexState, body: &[u8], now_unix: u64) -> Response {
    let text = String::from_utf8_lossy(body).to_string();
    match state.register_json(&text, "push".into(), now_unix) {
        Ok(node_id) => Response::json(
            201,
            serde_json::to_string(&json!({ "status": "registered", "node_id": node_id }))
                .unwrap_or_else(|_| r#"{"status":"registered"}"#.into()),
        ),
        Err(e) => Response::json(
            400,
            serde_json::to_string(&json!({ "status": "rejected", "reason": e.to_string() }))
                .unwrap_or_else(|_| r#"{"status":"rejected"}"#.into()),
        ),
    }
}

fn agent_id_from_body(body: &[u8]) -> Option<String> {
    let text = String::from_utf8_lossy(body);
    let value: Value = serde_json::from_str(&text).ok()?;
    value
        .get("agent_id")
        .and_then(|a| a.as_str())
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
}

fn handle_claim(state: &mut IndexState, node_id: &str, body: &[u8], now_unix: u64) -> Response {
    let Some(agent_id) = agent_id_from_body(body) else {
        return Response::json(
            400,
            r#"{"status":"rejected","reason":"agent_id is required"}"#.into(),
        );
    };
    match state.claim(node_id, &agent_id, now_unix, DEFAULT_CLAIM_TTL_SECS) {
        Ok(until) => Response::json(
            201,
            serde_json::to_string(&json!({
                "status": "claimed",
                "claimed_by": agent_id,
                "claimed_until_unix": until,
            }))
            .unwrap_or_else(|_| r#"{"status":"claimed"}"#.into()),
        ),
        Err(ClaimError::NotFound) => Response::text(404, "no offer for this node_id"),
        Err(ClaimError::Taken(owner)) => Response::json(
            409,
            serde_json::to_string(
                &json!({ "status": "taken", "reason": format!("claimed by {owner}") }),
            )
            .unwrap_or_else(|_| r#"{"status":"taken"}"#.into()),
        ),
        Err(ClaimError::NotOwner) => unreachable!("claim() never returns NotOwner"),
    }
}

fn handle_release(state: &mut IndexState, node_id: &str, body: &[u8], now_unix: u64) -> Response {
    let Some(agent_id) = agent_id_from_body(body) else {
        return Response::json(
            400,
            r#"{"status":"rejected","reason":"agent_id is required"}"#.into(),
        );
    };
    match state.release(node_id, &agent_id, now_unix) {
        Ok(()) => Response::json(200, r#"{"status":"released"}"#.into()),
        Err(ClaimError::NotFound) => Response::text(404, "no offer or claim for this node_id"),
        Err(ClaimError::NotOwner) => Response::json(403, r#"{"status":"not-owner"}"#.into()),
        Err(ClaimError::Taken(_)) => unreachable!("release() never returns Taken"),
    }
}

fn parse_filter(query: Option<&str>) -> OfferFilter {
    let mut f = OfferFilter::default();
    if let Some(q) = query {
        for pair in q.split('&') {
            let (k, v) = match pair.split_once('=') {
                Some(kv) => kv,
                None => continue,
            };
            match k {
                "mode" => match v {
                    "free" => f.mode = Some(FilterMode::Free),
                    "paid" => f.mode = Some(FilterMode::Paid),
                    _ => {}
                },
                "device" => match v {
                    "cpu" => f.device = Some(FilterDevice::Cpu),
                    "nvidia_gpu" => f.device = Some(FilterDevice::NvidiaGpu),
                    "nvidia_mig" => f.device = Some(FilterDevice::NvidiaMig),
                    "amd_gpu" => f.device = Some(FilterDevice::AmdGpu),
                    _ => {}
                },
                "available" => f.available = matches!(v, "1" | "true"),
                _ => {}
            }
        }
    }
    f
}

fn entry_to_value(e: &IndexEntry) -> Value {
    let offer = serde_json::to_value(&e.offer).unwrap_or(Value::Null);
    json!({
        "offer": offer,
        "source": e.source,
        "fetched_at": e.fetched_at_unix,
        "claimed_by": e.claimed_by,
        "claimed_until_unix": e.claim_until_unix,
    })
}

fn entry_to_json(e: &IndexEntry) -> String {
    serde_json::to_string(&entry_to_value(e)).unwrap_or_else(|_| "{}".into())
}

#[cfg(test)]
mod tests {
    use super::*;
    use ed25519_dalek::SigningKey;
    use vtessera_offer::{
        derive_node_id, sign, AdvertisedDevice, Currency, OfferBody, OFFER_SCHEMA_VER,
    };

    const NOW: u64 = 1_700_000_000;
    const EXPIRES: u64 = 1_700_010_000;

    fn offer_with(node_id: &str, price: PriceQuote, seed: u8, expires_unix: u64) -> SignedOffer {
        let key = SigningKey::from_bytes(&[seed; 32]);
        let body = OfferBody {
            schema_ver: OFFER_SCHEMA_VER,
            node_id: node_id.into(),
            endpoint: format!("https://{node_id}.example/vtessera"),
            device: AdvertisedDevice::NvidiaGpu {
                model: "H100-80GB".into(),
                vram_mb: 80 * 1024,
            },
            price,
            issued_unix: NOW - 1000,
            expires_unix,
        };
        sign(body, &key)
    }

    fn offer(node_id: &str, price: PriceQuote, seed: u8) -> SignedOffer {
        offer_with(node_id, price, seed, EXPIRES)
    }

    fn paid() -> PriceQuote {
        PriceQuote::Paid {
            currency: Currency::Eurc,
            per_device_second_micros: 250,
            payout_id: "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM".into(),
        }
    }

    fn signed_node_id(seed: u8) -> String {
        let key = SigningKey::from_bytes(&[seed; 32]);
        derive_node_id(&key.verifying_key().to_bytes())
    }

    #[test]
    fn register_verifies_signature_and_node_id() {
        let node_id = signed_node_id(1);
        let mut state = IndexState::new();
        let stored = state
            .register(offer(&node_id, paid(), 1), "push".into(), NOW)
            .unwrap();
        assert_eq!(stored, node_id);
        assert_eq!(state.count(), 1);
    }

    #[test]
    fn register_rejects_tampered_offer() {
        let node_id = signed_node_id(1);
        let mut offer = offer(&node_id, paid(), 1);
        offer.body.endpoint = "https://imposter.example".into();
        let mut state = IndexState::new();
        assert!(matches!(
            state.register(offer, "push".into(), NOW),
            Err(RegisterError::Verify(VerifyError::SignatureMismatch))
        ));
        assert_eq!(state.count(), 0);
    }

    #[test]
    fn register_json_parses_serialized_offer() {
        let node_id = signed_node_id(1);
        let offer = offer(&node_id, paid(), 1);
        let mut state = IndexState::new();
        let node = state
            .register_json(&serde_json::to_string(&offer).unwrap(), "push".into(), NOW)
            .unwrap();
        assert_eq!(node, node_id);
    }

    #[test]
    fn register_json_rejects_garbage() {
        let mut state = IndexState::new();
        assert!(matches!(
            state.register_json("not json", "push".into(), NOW),
            Err(RegisterError::BadJson(_))
        ));
    }

    #[test]
    fn register_rejects_expired_offer() {
        let node_id = signed_node_id(1);
        let o = offer_with(&node_id, paid(), 1, 1000);
        let mut state = IndexState::new();
        assert!(matches!(
            state.register(o, "push".into(), NOW),
            Err(RegisterError::Verify(VerifyError::Expired))
        ));
    }

    #[test]
    fn list_prunes_expired() {
        let a = signed_node_id(1);
        let b = signed_node_id(2);
        let oa = offer_with(&a, paid(), 1, NOW + 100);
        let ob = offer(&b, paid(), 2);

        let mut state = IndexState::new();
        state.register(oa, "push".into(), NOW).unwrap();
        state.register(ob, "push".into(), NOW).unwrap();
        assert_eq!(state.list(NOW + 200, &OfferFilter::default()).len(), 1);
        assert_eq!(state.count(), 1);
    }

    #[test]
    fn list_filters_mode_and_device() {
        let a = signed_node_id(1);
        let b = signed_node_id(2);
        let oa = offer(&a, PriceQuote::Free, 1);
        let ob = offer(&b, paid(), 2);

        let mut state = IndexState::new();
        state.register(oa, "push".into(), NOW).unwrap();
        state.register(ob, "push".into(), NOW).unwrap();

        let free = OfferFilter {
            mode: Some(FilterMode::Free),
            device: None,
            ..OfferFilter::default()
        };
        assert_eq!(state.list(NOW, &free).len(), 1);
        assert_eq!(state.list(NOW, &free)[0].offer.body.node_id, a);

        let gpu = OfferFilter {
            mode: None,
            device: Some(FilterDevice::NvidiaGpu),
            ..OfferFilter::default()
        };
        assert_eq!(state.list(NOW, &gpu).len(), 2);
    }

    #[test]
    fn dispatch_register_list_get_delete_cycle() {
        let node_id = signed_node_id(1);
        let offer = offer(&node_id, paid(), 1);
        let body = serde_json::to_string(&offer).unwrap();

        let mut state = IndexState::new();

        let r = dispatch(
            &mut state,
            Request {
                method: Method::Post,
                path: "/offers".into(),
                headers: vec![],
                body: body.into_bytes(),
            },
            NOW,
        );
        assert_eq!(r.status, 201);

        let r = dispatch(
            &mut state,
            Request {
                method: Method::Get,
                path: "/offers".into(),
                headers: vec![],
                body: vec![],
            },
            NOW,
        );
        assert_eq!(r.status, 200);
        let text = String::from_utf8(r.body).unwrap();
        let v: Value = serde_json::from_str(&text).unwrap();
        assert_eq!(v["count"], 1);
        assert_eq!(v["offers"][0]["offer"]["body"]["node_id"], node_id);

        let r = dispatch(
            &mut state,
            Request {
                method: Method::Get,
                path: format!("/offers/{node_id}"),
                headers: vec![],
                body: vec![],
            },
            NOW,
        );
        assert_eq!(r.status, 200);

        let r = dispatch(
            &mut state,
            Request {
                method: Method::Delete,
                path: format!("/offers/{node_id}"),
                headers: vec![],
                body: vec![],
            },
            NOW,
        );
        assert_eq!(r.status, 200);
        assert_eq!(state.count(), 0);
    }

    #[test]
    fn dispatch_rejects_unverified_register() {
        let node_id = signed_node_id(1);
        let mut offer = offer(&node_id, paid(), 1);
        offer.body.price = PriceQuote::Free;
        let mut state = IndexState::new();
        let r = dispatch(
            &mut state,
            Request {
                method: Method::Post,
                path: "/offers".into(),
                headers: vec![],
                body: serde_json::to_string(&offer).unwrap().into_bytes(),
            },
            NOW,
        );
        assert_eq!(r.status, 400);
        assert_eq!(state.count(), 0);
    }

    fn register_one(state: &mut IndexState, seed: u8) -> String {
        let node_id = signed_node_id(seed);
        state
            .register(offer(&node_id, paid(), seed), "push".into(), NOW)
            .unwrap();
        node_id
    }

    #[test]
    fn claim_is_first_come_first_served() {
        let mut state = IndexState::new();
        let node = register_one(&mut state, 1);
        let until = state.claim(&node, "agent-a", NOW, 60).unwrap();
        assert_eq!(until, NOW + 60);
        assert!(matches!(
            state.claim(&node, "agent-b", NOW + 5, 60),
            Err(ClaimError::Taken(owner)) if owner == "agent-a"
        ));
    }

    #[test]
    fn same_agent_reclaim_renews() {
        let mut state = IndexState::new();
        let node = register_one(&mut state, 1);
        state.claim(&node, "agent-a", NOW, 60).unwrap();
        let until = state.claim(&node, "agent-a", NOW + 30, 60).unwrap();
        assert_eq!(until, NOW + 90);
    }

    #[test]
    fn expired_claim_is_reclaimable_by_another_agent() {
        let mut state = IndexState::new();
        let node = register_one(&mut state, 1);
        state.claim(&node, "agent-a", NOW, 60).unwrap();
        let until = state.claim(&node, "agent-b", NOW + 61, 60).unwrap();
        assert_eq!(until, NOW + 121);
        let e = state.get(&node).unwrap();
        assert_eq!(e.claimed_by.as_deref(), Some("agent-b"));
    }

    #[test]
    fn release_is_owner_only() {
        let mut state = IndexState::new();
        let node = register_one(&mut state, 1);
        state.claim(&node, "agent-a", NOW, 60).unwrap();
        assert!(matches!(
            state.release(&node, "agent-b", NOW + 5),
            Err(ClaimError::NotOwner)
        ));
        state.release(&node, "agent-a", NOW + 5).unwrap();
        let e = state.get(&node).unwrap();
        assert_eq!(e.claimed_by, None);
        assert_eq!(e.claim_until_unix, 0);
    }

    #[test]
    fn release_on_unclaimed_or_unknown_is_not_found() {
        let mut state = IndexState::new();
        let node = register_one(&mut state, 1);
        assert!(matches!(
            state.release(&node, "agent-a", NOW),
            Err(ClaimError::NotFound)
        ));
        assert!(matches!(
            state.release("nope", "agent-a", NOW),
            Err(ClaimError::NotFound)
        ));
    }

    #[test]
    fn prune_clears_expired_claim_but_keeps_entry() {
        let mut state = IndexState::new();
        let node = register_one(&mut state, 1);
        state.claim(&node, "agent-a", NOW, 60).unwrap();
        state.prune(NOW + 61);
        let e = state.get(&node).unwrap();
        assert_eq!(e.claimed_by, None);
        assert_eq!(e.claim_until_unix, 0);
        assert_eq!(state.count(), 1);
    }

    #[test]
    fn available_filter_hides_claimed_offers() {
        let mut state = IndexState::new();
        let a = register_one(&mut state, 1);
        let b = register_one(&mut state, 2);
        state.claim(&a, "agent-a", NOW, 60).unwrap();

        let available = OfferFilter {
            available: true,
            ..OfferFilter::default()
        };
        let listed: Vec<&str> = state
            .list(NOW + 1, &available)
            .iter()
            .map(|e| e.offer.body.node_id.as_str())
            .collect();
        assert_eq!(listed, vec![b.as_str()]);

        let all = OfferFilter::default();
        assert_eq!(state.list(NOW + 1, &all).len(), 2);
    }

    #[test]
    fn register_preserves_a_live_claim_and_drops_an_expired_one() {
        let mut state = IndexState::new();
        let node = register_one(&mut state, 1);
        state.claim(&node, "agent-a", NOW, 60).unwrap();

        state
            .register(offer(&node, paid(), 1), "push".into(), NOW + 10)
            .unwrap();
        let e = state.get(&node).unwrap();
        assert_eq!(e.claimed_by.as_deref(), Some("agent-a"));
        assert_eq!(e.claim_until_unix, NOW + 60);

        state
            .register(offer(&node, paid(), 1), "push".into(), NOW + 61)
            .unwrap();
        let e = state.get(&node).unwrap();
        assert_eq!(e.claimed_by, None);
        assert_eq!(e.claim_until_unix, 0);
    }

    #[test]
    fn dispatch_claim_release_cycle() {
        let node = signed_node_id(1);
        let mut state = IndexState::new();
        state
            .register(offer(&node, paid(), 1), "push".into(), NOW)
            .unwrap();

        let claim = |state: &mut IndexState, agent: &str| {
            dispatch(
                state,
                Request {
                    method: Method::Post,
                    path: format!("/offers/{node}/claim"),
                    headers: vec![],
                    body: format!(r#"{{"agent_id":"{agent}"}}"#).into_bytes(),
                },
                NOW,
            )
        };

        let r = claim(&mut state, "agent-a");
        assert_eq!(r.status, 201);
        let text = String::from_utf8(r.body).unwrap();
        assert!(text.contains("\"status\":\"claimed\""));

        let r = claim(&mut state, "agent-b");
        assert_eq!(r.status, 409);
        assert!(String::from_utf8(r.body).unwrap().contains("agent-a"));

        let r = dispatch(
            &mut state,
            Request {
                method: Method::Delete,
                path: format!("/offers/{node}/claim"),
                headers: vec![],
                body: br#"{"agent_id":"agent-b"}"#.to_vec(),
            },
            NOW,
        );
        assert_eq!(r.status, 403);

        let r = dispatch(
            &mut state,
            Request {
                method: Method::Delete,
                path: format!("/offers/{node}/claim"),
                headers: vec![],
                body: br#"{"agent_id":"agent-a"}"#.to_vec(),
            },
            NOW,
        );
        assert_eq!(r.status, 200);
        assert!(String::from_utf8(r.body).unwrap().contains("released"));
    }

    #[test]
    fn dispatch_claim_missing_agent_id_is_400() {
        let node = signed_node_id(1);
        let mut state = IndexState::new();
        state
            .register(offer(&node, paid(), 1), "push".into(), NOW)
            .unwrap();
        let r = dispatch(
            &mut state,
            Request {
                method: Method::Post,
                path: format!("/offers/{node}/claim"),
                headers: vec![],
                body: br#"{}"#.to_vec(),
            },
            NOW,
        );
        assert_eq!(r.status, 400);
    }

    #[test]
    fn dispatch_list_includes_claim_state_and_available_filter() {
        let node = signed_node_id(1);
        let mut state = IndexState::new();
        state
            .register(offer(&node, paid(), 1), "push".into(), NOW)
            .unwrap();
        state.claim(&node, "agent-a", NOW, 60).unwrap();

        let r = dispatch(
            &mut state,
            Request {
                method: Method::Get,
                path: "/offers?available=1".into(),
                headers: vec![],
                body: vec![],
            },
            NOW,
        );
        assert_eq!(r.status, 200);
        let v: Value = serde_json::from_str(&String::from_utf8(r.body).unwrap()).unwrap();
        assert_eq!(v["count"], 0);

        let r = dispatch(
            &mut state,
            Request {
                method: Method::Get,
                path: "/offers".into(),
                headers: vec![],
                body: vec![],
            },
            NOW,
        );
        let v: Value = serde_json::from_str(&String::from_utf8(r.body).unwrap()).unwrap();
        assert_eq!(v["count"], 1);
        assert_eq!(v["offers"][0]["claimed_by"], "agent-a");
        assert!(v["offers"][0]["claimed_until_unix"].as_u64().unwrap() > NOW);
    }
}
