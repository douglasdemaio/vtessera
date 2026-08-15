//! Offer-index client for the node binary — Module 2a wiring (ROADMAP §2a).
//!
//! Serve-gated: the default library build stays socket-free, so this module
//! exists only under `feature = "serve"`. The binary implements the
//! [`IndexClient`] trait over HTTP; the library and the MCP server consume
//! the trait, so tests can inject a fake.
//!
//! `admit` is the first-come-first-served gate: it performs one race-safe
//! `POST /offers/{node_id}/claim` round trip against the index, which is the
//! claim authority. A 201 means the node is ours (or our claim was renewed);
//! a 409 means a different agent holds the node; any transport failure fails
//! closed — a node that cannot verify a claim must not admit strangers.

#![forbid(unsafe_code)]

/// Filters for the `discover` query. Strings map onto the index's
/// `GET /offers` query params (`mode=free|paid`, `device=...`,
/// `available=1`); node-api deliberately doesn't depend on
/// `vtessera-offer-index`, so the values stay plain strings here.
#[derive(Debug, Clone, Default)]
pub struct IndexQuery {
    pub mode: Option<String>,
    pub device: Option<String>,
    pub available: bool,
}

/// Why a claim gate refused admission.
#[derive(Debug, Clone)]
pub enum AdmitError {
    /// The index reports a live claim held by a different agent.
    Taken(String),
    /// The index could not be reached, so the claim could not be verified.
    Unreachable(String),
}

/// The outbound client the node uses to talk to its configured offer index.
///
/// The node id and index base URL are captured at construction, so both
/// methods need only the per-call parameters.
pub trait IndexClient: Send + Sync {
    /// Claim the node for `agent_id` (or renew the agent's own claim).
    /// `Ok(())` admits; the error carries the refusal reason.
    fn admit(&self, agent_id: &str) -> Result<(), AdmitError>;
    /// Fetch matching offers from the index; returns the index's JSON body.
    fn discover(&self, query: &IndexQuery) -> Result<String, String>;
}
