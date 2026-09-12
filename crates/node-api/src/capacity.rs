//! Live capacity reconfiguration for a node (issue #109).
//!
//! The `capacity.toml` schema itself lives in the shared controller crate
//! ([`vtessera_capacity::CapacityConfig`]) so the node and the controller can
//! never drift — a file the controller commits is byte-for-byte what this
//! module validates. This module is the node side of that contract: the
//! queue-gate apply, the offer re-signing, and the change watcher.
//!
//! ```toml
//! max_concurrent_jobs = 4    # open execution slots
//! max_queue_len = 32         # jobs allowed to wait in the on-disk queue
//! advertised_vcpus = 8       # offer device (applied by offer re-signing)
//! advertised_mem_mb = 16384  # offer device (applied by offer re-signing)
//! price_multiplier = 1.5     # paid-offer price scale (offer re-signing)
//! accept_workloads = true    # GUI consent-toggle mirror (admission policy)
//! ```
//!
//! Every field is optional — a file only overrides the knobs it mentions
//! and leaves the rest at their current value. Unknown keys are rejected at
//! load so a typo'ed control fails loudly instead of silently no-op'ing.
//!
//! Two apply layers land on a successful load:
//!
//! - the queue gate ([`JobQueue::reconfigure`]) is applied by this module;
//! - the offer-side knobs (`advertised_*`, `price_multiplier`,
//!   `accept_workloads`) are re-signed into the published offer by the
//!   binary via [`re_sign_offer`], which runs on every apply through the
//!   watcher's `on_apply` callback.
//!
//! Wiring in the binary (`vtessera_node.rs`) picks which parts of the offer
//! change; this module is the queue apply + re-signer + watcher.
//!
//! Watching is a cheap mtime+size poll (no inotify dependency): the node's
//! process stays self-contained and the poll interval is seconds-scale
//! anyway.

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::thread;
use std::time::Duration;

use crate::queue::JobQueue;
use vtessera_capacity::{file_stamp, load};
pub use vtessera_capacity::{CapacityConfig, CapacityError};
use vtessera_offer::{AdvertisedDevice, PriceQuote, SignedOffer};
use vtessera_settlement::SigningKey;

/// Default poll interval for the capacity watch loop.
pub const DEFAULT_CAPACITY_POLL_SECS: u64 = 2;

/// Apply the capacity file to a queue once: reconfigure the admission gate
/// from the file's non-`None` queue knobs, leaving everything else at its
/// current value. The parsed config is returned so callers can run the
/// offer-side apply (see [`re_sign_offer`]).
pub fn apply_config(path: &Path, queue: &JobQueue) -> Result<CapacityConfig, CapacityError> {
    let cfg = load(path)?;
    queue.reconfigure(
        cfg.max_concurrent_jobs
            .unwrap_or_else(|| queue.max_concurrent()),
        cfg.max_queue_len.unwrap_or_else(|| queue.max_queue_len()),
    );
    Ok(cfg)
}

/// One apply pass with operator-facing logging. Shared by the startup
/// one-shot and the watch loop so every reload reports the same way. On a
/// successful apply the `on_apply` callback fires with the parsed config —
/// the binary uses it to re-sign the live offer (issue #109).
pub fn apply_and_fire(
    path: &Path,
    queue: &Arc<JobQueue>,
    on_apply: &(dyn Fn(&CapacityConfig) + Send + Sync),
) {
    match apply_config(path, queue) {
        Ok(cfg) => {
            eprintln!("vtessera-node: applied capacity file {}", path.display());
            on_apply(&cfg);
        }
        Err(e) => eprintln!("vtessera-node: capacity apply failed: {e}"),
    }
}

/// Re-sign the published offer from the capacity file's offer-side knobs.
///
/// `advertised_vcpus` / `advertised_mem_mb` are only applicable to a
/// CPU-class device; `price_multiplier` only to a paid price. A knob that
/// does not apply to this offer (e.g. `advertised_vcpus` on a GPU-class
/// device, or a multiplier on a free offer) is skipped — re-signing it in
/// would be a contract violation. Returns `Ok(Some(signed))` when at least
/// one knob applied and the offer was re-signed, `Ok(None)` when no offer
/// knob was present or none applied, and `Err` for an invalid knob (a
/// negative or non-finite `price_multiplier`).
pub fn re_sign_offer(
    cfg: &CapacityConfig,
    current: &SignedOffer,
    signing_key: &SigningKey,
) -> Result<Option<SignedOffer>, CapacityError> {
    let mut body = current.body.clone();
    let mut changed = false;

    if let Some(vcpus) = cfg.advertised_vcpus {
        if let AdvertisedDevice::Cpu { vcpus: v, .. } = &mut body.device {
            *v = vcpus;
            changed = true;
        }
    }
    if let Some(mem_mb) = cfg.advertised_mem_mb {
        if let AdvertisedDevice::Cpu { mem_mb: m, .. } = &mut body.device {
            *m = mem_mb;
            changed = true;
        }
    }
    if let Some(mult) = cfg.price_multiplier {
        if !mult.is_finite() || mult < 0.0 {
            return Err(CapacityError(format!(
                "price_multiplier {mult} must be a finite value >= 0"
            )));
        }
        if let PriceQuote::Paid {
            per_device_second_micros,
            ..
        } = &mut body.price
        {
            *per_device_second_micros =
                (((*per_device_second_micros as f64) * mult).round()).min(u64::MAX as f64) as u64;
            changed = true;
        }
    }

    if !changed {
        return Ok(None);
    }
    Ok(Some(vtessera_offer::sign(body, signing_key)))
}

/// Poll `path` every `poll` and re-apply it whenever it changes (mtime or
/// size). `on_apply` fires after each successful apply. Runs until the
/// process exits. A broken file is reported and retried on the next change
/// — the node keeps its last good values.
pub fn spawn_watcher(
    path: PathBuf,
    queue: Arc<JobQueue>,
    poll: Duration,
    on_apply: Arc<dyn Fn(&CapacityConfig) + Send + Sync>,
) {
    thread::spawn(move || {
        // Don't re-apply the state the binary already applied at startup;
        // only actual changes (or a file appearing later) trigger a pass.
        let mut last = file_stamp(&path);
        loop {
            thread::sleep(poll);
            let stamp = file_stamp(&path);
            if stamp != last {
                apply_and_fire(&path, &queue, on_apply.as_ref());
                last = stamp;
            }
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::queue::{JobQueue, QueuedJob};
    use crate::{JobRunError, JobRunner};
    use std::fs;
    use vtessera_offer::{
        derive_node_id, sign, AdvertisedDevice, Currency, OfferBody, PriceQuote, OFFER_SCHEMA_VER,
    };

    fn temp_dir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("vtcap-{tag}"));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).expect("create temp dir");
        dir
    }

    fn write(dir: &Path, name: &str, body: &str) -> PathBuf {
        let p = dir.join(name);
        fs::write(&p, body).expect("write capacity file");
        p
    }

    struct Echo;
    impl JobRunner for Echo {
        fn run(&self, body: &[u8]) -> Result<String, JobRunError> {
            let _ = QueuedJob {
                seq: 0,
                job_id: "x".into(),
                priority: 0,
                body: body.to_vec(),
                created_unix: 0,
            };
            Ok(r#"{"status":"accepted","job_id":"x"}"#.into())
        }
    }

    #[test]
    fn apply_config_reconfigures_the_queue() {
        let p = write(
            &temp_dir("apply"),
            "capacity.toml",
            "max_concurrent_jobs = 3\nmax_queue_len = 8\n",
        );
        let q = JobQueue::new(temp_dir("apply-q"), Arc::new(Echo), 1, 16);
        apply_config(&p, &q).unwrap();
        assert_eq!(q.max_concurrent(), 3);
        assert_eq!(q.max_queue_len(), 8);
    }

    #[test]
    fn apply_config_keeps_unmentioned_knobs() {
        let p = write(
            &temp_dir("partial"),
            "capacity.toml",
            "max_concurrent_jobs = 2\n",
        );
        let q = JobQueue::new(temp_dir("partial-q"), Arc::new(Echo), 1, 16);
        apply_config(&p, &q).unwrap();
        assert_eq!(q.max_concurrent(), 2);
        assert_eq!(q.max_queue_len(), 16, "unmentioned knob is untouched");
    }

    #[test]
    fn file_stamp_tracks_rewrites() {
        let dir = temp_dir("stamp");
        let p = write(&dir, "capacity.toml", "max_concurrent_jobs = 1\n");
        let before = file_stamp(&p).expect("stamp");
        // Same-second rewrite with different content: the size must flip the stamp.
        fs::write(&p, "max_concurrent_jobs = 77\nmax_queue_len = 3\n").unwrap();
        let after = file_stamp(&p).expect("stamp");
        assert_ne!(before, after);
        assert_eq!(file_stamp(&dir.join("absent.toml")), None);
    }

    fn signed_cpu_paid() -> SignedOffer {
        let key = ed25519_dalek::SigningKey::from_bytes(&[9u8; 32]);
        let body = OfferBody {
            schema_ver: OFFER_SCHEMA_VER,
            node_id: derive_node_id(&key.verifying_key().to_bytes()),
            endpoint_id: hex::encode(key.verifying_key().to_bytes()),
            endpoint: vec!["https://node.example/v1".into()],
            device: AdvertisedDevice::Cpu {
                vcpus: 4,
                mem_mb: 8192,
            },
            price: PriceQuote::Paid {
                currency: Currency::Eurc,
                per_device_second_micros: 2792,
                payout_id: "5fMLGtXrcTXyxXt7RGz7qLgnbxH2nnvkTcXmBRxAARfs".into(),
            },
            issued_unix: 1_700_000_000,
            expires_unix: 1_700_010_000,
        };
        sign(body, &key)
    }

    #[test]
    fn re_sign_offer_applies_cpu_and_paid_knobs() {
        let cfg = CapacityConfig {
            max_concurrent_jobs: None,
            max_queue_len: None,
            advertised_vcpus: Some(12),
            advertised_mem_mb: Some(24576),
            price_multiplier: Some(0.5),
            accept_workloads: None,
        };
        let key = ed25519_dalek::SigningKey::from_bytes(&[9u8; 32]);
        let new = re_sign_offer(&cfg, &signed_cpu_paid(), &key)
            .unwrap()
            .expect("re-signed");
        assert_eq!(
            new.body.device,
            AdvertisedDevice::Cpu {
                vcpus: 12,
                mem_mb: 24576
            }
        );
        match &new.body.price {
            PriceQuote::Paid {
                per_device_second_micros,
                ..
            } => {
                assert_eq!(*per_device_second_micros, 1396, "2792 * 0.5 rounds to 1396");
            }
            other => panic!("expected paid price, got {other:?}"),
        }
    }

    #[test]
    fn re_sign_offer_skips_non_apply_knobs() {
        // `advertised_vcpus` on a GPU-class device is a contract violation → skip.
        let key = ed25519_dalek::SigningKey::from_bytes(&[10u8; 32]);
        let body = OfferBody {
            device: AdvertisedDevice::NvidiaGpu {
                model: "A100".into(),
                vram_mb: 40960,
            },
            ..signed_cpu_paid().body.clone()
        };
        let signed = sign(body, &key);
        let cfg = CapacityConfig {
            max_concurrent_jobs: None,
            max_queue_len: None,
            advertised_vcpus: Some(8),
            advertised_mem_mb: None,
            price_multiplier: None,
            accept_workloads: None,
        };
        assert_eq!(re_sign_offer(&cfg, &signed, &key).unwrap(), None);
    }

    #[test]
    fn re_sign_offer_rejects_negative_multiplier() {
        let cfg = CapacityConfig {
            max_concurrent_jobs: None,
            max_queue_len: None,
            advertised_vcpus: None,
            advertised_mem_mb: None,
            price_multiplier: Some(-1.0),
            accept_workloads: None,
        };
        let key = ed25519_dalek::SigningKey::from_bytes(&[11u8; 32]);
        let err = re_sign_offer(&cfg, &signed_cpu_paid(), &key).unwrap_err();
        assert!(err.to_string().contains("price_multiplier"), "err: {err}");
    }
}
