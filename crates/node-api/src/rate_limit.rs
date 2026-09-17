//! Request rate limiting (ROADMAP.md §5 — abuse handling).
//!
//! A node-local token bucket keyed by request identity. The node-api crate
//! is pure dispatch and framework-agnostic, so the limiter lives here where
//! every transport funnels through [`crate::dispatch`]: the node's TCP HTTP
//! server, the iroh QUIC router, and the outbound coordinator pull loop all
//! share the same choke point. A rate limiter wired into the node binary
//! refuses abusive agents before they can busy the executor or fill the
//! on-disk job queue.
//!
//! Keying: one bucket per `X-Agent-Id` header, plus a single shared `*`
//! bucket for anonymous traffic. That isolates well-behaved agents from a
//! rogue peer while still bounding the aggregate anonymous rate. Keys are
//! capped to [`MAX_KEY_LEN`] bytes, and the distinct-key table is bounded by
//! [`MAX_DISTINCT_KEYS`] and evicted wholesale when full — deterministic
//! and O(1) worst case, which is all a node-local table needs.
//!
//! `None` in [`crate::NodeState::rate_limit`] (the default) means no
//! limiting at all — today's behavior, byte for byte.
//!
//! The bucket math is a standard continuous token bucket: each key holds up
//! to `burst` tokens, refilled at `refill_per_sec` tokens/second and capped
//! at `burst`. A request consumes one token; when none remain it is rejected
//! with the seconds until a token is available (for the 429 `Retry-After`).

use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, Instant};

/// Max bytes of an `X-Agent-Id` used as a bucket key (a hostile header must
/// not grow the key table unboundedly).
pub const MAX_KEY_LEN: usize = 64;

/// Cap on distinct in-memory buckets before the table resets.
pub const MAX_DISTINCT_KEYS: usize = 1024;

/// One key's token bucket. `last` is the last time the bucket was touched.
struct Bucket {
    tokens: f64,
    last: Instant,
}

/// A bounded, per-key token-bucket rate limiter.
///
/// Cheap: a single `Mutex<HashMap>` and per-key header-derived keys; no `now`
/// provider is injected because every caller is on the same thread/tick as
/// `Instant::now` — the module's unit tests drive a private `check_at` with
/// fabricated clocks instead.
pub struct RateLimiter {
    burst: f64,
    refill_per_sec: f64,
    inner: Mutex<HashMap<String, Bucket>>,
}

impl RateLimiter {
    /// A limiter whose buckets hold `burst` tokens and refill at
    /// `refill_per_sec` tokens/second. `burst` is clamped to ≥ 1 and
    /// `refill_per_sec` to > 0 so the math never divides by zero.
    pub fn new(burst: u32, refill_per_sec: f64) -> Self {
        RateLimiter {
            burst: burst.max(1) as f64,
            refill_per_sec: refill_per_sec.max(f64::EPSILON),
            inner: Mutex::new(HashMap::new()),
        }
    }

    /// Try to consume one request's token for `key`.
    ///
    /// `Ok(())` admits the request. `Err(retry_after)` rejects it; the
    /// integer seconds until a token is available is what a 429 response
    /// should advertise in `Retry-After`.
    pub fn check(&self, key: &str) -> Result<(), Duration> {
        self.check_at(key, Instant::now())
    }

    fn check_at(&self, key: &str, now: Instant) -> Result<(), Duration> {
        let mut inner = self.inner.lock().expect("rate-limit mutex poisoned");
        // Bounded memory: when the table is full of distinct keys, reset it
        // deterministically. A stale allowed token for a known agent is a
        // one-burst relaxation, never unbounded growth.
        if !inner.contains_key(key) && inner.len() >= MAX_DISTINCT_KEYS {
            inner.clear();
        }
        let bucket = inner.entry(key.to_string()).or_insert_with(|| Bucket {
            tokens: self.burst,
            last: now,
        });
        bucket.tokens = (bucket.tokens
            + now.duration_since(bucket.last).as_secs_f64() * self.refill_per_sec)
            .min(self.burst);
        bucket.last = now;
        if bucket.tokens >= 1.0 {
            bucket.tokens -= 1.0;
            Ok(())
        } else {
            let wait = (1.0 - bucket.tokens) / self.refill_per_sec;
            Err(Duration::from_secs(wait.max(1.0).ceil() as u64))
        }
    }
}

/// The bucket key for a request: its `x-agent-id` header when present
/// (capped, non-empty), else the `*` shared bucket for anonymous traffic.
pub fn request_key(headers: &[(String, String)]) -> String {
    headers
        .iter()
        .find(|(k, _)| k == "x-agent-id")
        .map(|(_, v)| v.chars().take(MAX_KEY_LEN).collect())
        .filter(|k: &String| !k.is_empty())
        .unwrap_or_else(|| "*".to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn headers(agent: Option<&str>) -> Vec<(String, String)> {
        agent
            .map(|a| vec![("x-agent-id".into(), a.into())])
            .unwrap_or_default()
    }

    #[test]
    fn burst_is_consumed_then_rejected() {
        let rl = RateLimiter::new(2, 0.5);
        let t0 = Instant::now();
        assert!(rl.check_at("a", t0).is_ok());
        assert!(rl.check_at("a", t0).is_ok());
        // Third request at the same instant has no tokens.
        assert!(rl.check_at("a", t0).is_err());
    }

    #[test]
    fn refill_admits_after_bucket_drains() {
        let rl = RateLimiter::new(1, 1.0);
        let t0 = Instant::now();
        assert!(rl.check_at("a", t0).is_ok());
        assert!(rl.check_at("a", t0).is_err());
        // One second later a token has refilled.
        assert!(rl.check_at("a", t0 + Duration::from_secs(1)).is_ok());
        // And is gone again immediately.
        assert!(rl.check_at("a", t0 + Duration::from_secs(1)).is_err());
    }

    #[test]
    fn retry_after_reports_seconds_to_waits() {
        let rl = RateLimiter::new(1, 0.5);
        let t0 = Instant::now();
        assert!(rl.check_at("a", t0).is_ok());
        let err = rl.check_at("a", t0).unwrap_err();
        // Refill 0.5/s needs 2s for a full token; guards force ≥ 1.
        assert!(err.as_secs() >= 1);
    }

    #[test]
    fn keys_are_independent() {
        let rl = RateLimiter::new(1, 0.5);
        let t0 = Instant::now();
        assert!(rl.check_at("a", t0).is_ok());
        assert!(rl.check_at("a", t0).is_err());
        // A different key still has its own fresh bucket.
        assert!(rl.check_at("b", t0).is_ok());
    }

    #[test]
    fn table_resets_when_silently_full() {
        let rl = RateLimiter::new(1, 0.5);
        let t0 = Instant::now();
        // Fill the table past MAX_DISTINCT_KEYS distinct keys.
        for i in 0..=MAX_DISTINCT_KEYS {
            assert!(rl.check_at(&format!("k{i}"), t0).is_ok());
        }
        // The table was reset, so a previously-seen key gets a fresh bucket.
        assert!(rl.check_at("k0", t0).is_ok());
    }

    #[test]
    fn request_key_prefers_agent_id_and_caps_length() {
        assert_eq!(request_key(&headers(Some("agent-1"))), "agent-1");
        assert_eq!(request_key(&headers(None)), "*");
        assert_eq!(request_key(&headers(Some(""))), "*");
        let long = "x".repeat(MAX_KEY_LEN + 50);
        assert_eq!(request_key(&headers(Some(&long))).len(), MAX_KEY_LEN);
    }
}
