//! Usage-based autoscale engine (issue #109 v2).
//!
//! The third controller input of the capacity-automation design: poll a
//! node's `/metrics` demand gauges, and when sustained demand saturates the
//! currently advertised capacity, re-write the node's watched `capacity.toml`
//! (the same file-drop path the node already applies live) to scale
//! `advertised_vcpus` / `max_concurrent_jobs` up; when idle for long enough,
//! scale back down. Hysteresis (separate up/down basis-point thresholds) and
//! cooldowns prevent flapping.
//!
//! Everything here is pure logic except [`fetch_metrics`]: the decision
//! ([`LoopState::decide_at`]) and the target computation
//! ([`scale_target`]) are deterministic and unit-tested without sockets.
//! The engine writes through the same atomic commit + events log the
//! file-drop controller uses, so an audit trail covers every transition
//! regardless of which input caused it.

use std::fs;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use serde::Deserialize;

use crate::{append_event, diff_config, write_atomically, CapacityConfig, CapacityError};

/// Operator-facing autoscale configuration. Unknown keys are rejected so a
/// typo'ed threshold fails loudly. All floors/ceilings are optional with
/// conservative defaults; [`AutoscaleConfig::validate`] enforces the
/// invariants (floor <= ceiling, up window above down window, sane bounds).
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AutoscaleConfig {
    /// Node base URL whose `/metrics` is polled, e.g. `"http://127.0.0.1:8402"`.
    pub url: String,
    /// The node's watched capacity file this engine writes (the `--out`
    /// path of the file-drop mode).
    pub out: PathBuf,
    /// Optional JSON-lines audit log (same format as `--events` in the
    /// file-drop mode).
    #[serde(default)]
    pub events: Option<PathBuf>,
    /// Poll interval (seconds).
    #[serde(default = "default_poll_secs")]
    pub poll_secs: u64,
    /// Advertised vCPUs floor — scale-down never goes below this.
    #[serde(default = "default_min_vcpus")]
    pub min_vcpus: u32,
    /// Advertised vCPUs ceiling — scale-up never goes above this (the
    /// operator is responsible for keeping it at or below host reality).
    #[serde(default = "default_max_vcpus")]
    pub max_vcpus: u32,
    /// Concurrency floor.
    #[serde(default = "default_min_concurrent")]
    pub min_concurrent_jobs: u32,
    /// Concurrency ceiling.
    #[serde(default = "default_max_concurrent")]
    pub max_concurrent_jobs: u32,
    /// vCPU step each scale decision moves by.
    #[serde(default = "default_step_vcpus")]
    pub step_vcpus: u32,
    /// Scale up when `vtessera_demand_pct` is at/above this (basis points).
    #[serde(default = "default_up_pct")]
    pub up_pct: u64,
    /// ...for at least this long (seconds) before acting.
    #[serde(default = "default_up_window_secs")]
    pub up_window_secs: u64,
    /// Scale down when `vtessera_demand_running_pct` is at/below this.
    #[serde(default = "default_down_pct")]
    pub down_pct: u64,
    /// ...for at least this long (seconds) before acting.
    #[serde(default = "default_down_window_secs")]
    pub down_window_secs: u64,
    /// Minimum time (seconds) between two scale actions.
    #[serde(default = "default_cooldown_secs")]
    pub cooldown_secs: u64,
}

fn default_poll_secs() -> u64 {
    30
}
fn default_min_vcpus() -> u32 {
    2
}
fn default_max_vcpus() -> u32 {
    16
}
fn default_min_concurrent() -> u32 {
    1
}
fn default_max_concurrent() -> u32 {
    8
}
fn default_step_vcpus() -> u32 {
    2
}
fn default_up_pct() -> u64 {
    8000
}
fn default_up_window_secs() -> u64 {
    300
}
fn default_down_pct() -> u64 {
    3000
}
fn default_down_window_secs() -> u64 {
    900
}
fn default_cooldown_secs() -> u64 {
    600
}

impl AutoscaleConfig {
    /// Reject configurations that would misbehave: broken windows, reversed
    /// floors/ceilings, or an up threshold at or below the down threshold
    /// (up must be materially above the level that turns growth off).
    pub fn validate(&self) -> Result<(), CapacityError> {
        if self.poll_secs == 0 {
            return Err(CapacityError("poll_secs must be >= 1".into()));
        }
        if self.step_vcpus == 0 {
            return Err(CapacityError("step_vcpus must be >= 1".into()));
        }
        if self.min_vcpus == 0 {
            return Err(CapacityError("min_vcpus must be >= 1".into()));
        }
        if self.max_vcpus < self.min_vcpus {
            return Err(CapacityError("max_vcpus must be >= min_vcpus".into()));
        }
        if self.max_concurrent_jobs < self.min_concurrent_jobs {
            return Err(CapacityError(
                "max_concurrent_jobs must be >= min_concurrent_jobs".into(),
            ));
        }
        if self.max_concurrent_jobs == 0 {
            return Err(CapacityError("max_concurrent_jobs must be >= 1".into()));
        }
        if self.up_pct == 0 || self.up_pct > 10000 {
            return Err(CapacityError("up_pct must be in 1..=10000".into()));
        }
        if self.down_pct > 10000 {
            return Err(CapacityError("down_pct must be <= 10000".into()));
        }
        if self.up_pct <= self.down_pct {
            return Err(CapacityError(
                "up_pct must be above down_pct (hysteresis)".into(),
            ));
        }
        if self.up_window_secs == 0 {
            return Err(CapacityError("up_window_secs must be >= 1".into()));
        }
        if self.down_window_secs == 0 {
            return Err(CapacityError("down_window_secs must be >= 1".into()));
        }
        if self.cooldown_secs == 0 {
            return Err(CapacityError("cooldown_secs must be >= 1".into()));
        }
        Ok(())
    }
}

/// Read and validate an autoscale config file.
pub fn load_config(path: &Path) -> Result<AutoscaleConfig, CapacityError> {
    let raw = fs::read_to_string(path)
        .map_err(|e| CapacityError(format!("read {}: {e}", path.display())))?;
    let cfg: AutoscaleConfig =
        toml::from_str(&raw).map_err(|e| CapacityError(format!("{}: {e}", path.display())))?;
    cfg.validate()?;
    Ok(cfg)
}

/// One `/metrics` sample the engine keys its decisions off.
#[derive(Debug, Clone, Copy)]
pub struct Sample {
    /// `vtessera_demand_pct` — basis points of (running+queued)/max_concurrent.
    pub demand_pct: u64,
    /// `vtessera_demand_running_pct` — basis points of running/max_concurrent.
    pub running_pct: u64,
}

/// Which direction the engine wants to move.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Scale {
    /// Demand saturated the current cap: grow `advertised_vcpus` by one
    /// [`AutoscaleConfig::step_vcpus`].
    Up,
    /// Idle long enough: shrink by one step, never below the floor.
    Down,
}

/// Running window/cool-down bookkeeping for the decision loop. Counters
/// reset whenever the input stops agreeing, so a transient spike never
/// trips a scale-up on its own.
#[derive(Debug, Default)]
pub struct LoopState {
    up_hits: u64,
    down_hits: u64,
    last_action_at: Option<Instant>,
}

impl LoopState {
    /// Feed one metric sample and return the resulting action, if any.
    ///
    /// `now` is passed in so tests can drive a fabricated timeline.
    pub fn decide_at(
        &mut self,
        now: Instant,
        sample: Sample,
        cfg: &AutoscaleConfig,
    ) -> Option<Scale> {
        // Cooling down: hold still regardless of the sample.
        if let Some(t) = self.last_action_at {
            if now.duration_since(t) < Duration::from_secs(cfg.cooldown_secs) {
                return None;
            }
        }
        if sample.demand_pct >= cfg.up_pct {
            self.up_hits += 1;
            self.down_hits = 0;
        } else if sample.running_pct <= cfg.down_pct {
            self.down_hits += 1;
            self.up_hits = 0;
        } else {
            // Between the thresholds — a stable-but-engaged node does nothing.
            self.up_hits = 0;
            self.down_hits = 0;
        }

        let up_needed = hits_needed(cfg.poll_secs, cfg.up_window_secs);
        let down_needed = hits_needed(cfg.poll_secs, cfg.down_window_secs);
        if self.up_hits >= up_needed {
            self.up_hits = 0;
            self.last_action_at = Some(now);
            Some(Scale::Up)
        } else if self.down_hits >= down_needed {
            self.down_hits = 0;
            self.last_action_at = Some(now);
            Some(Scale::Down)
        } else {
            None
        }
    }
}

/// Polls needed to cover `window_secs` at `poll_secs`, never below 1.
fn hits_needed(poll_secs: u64, window_secs: u64) -> u64 {
    window_secs.div_ceil(poll_secs.max(1)).max(1)
}

/// Extract a Prometheus gauge value for `name` from a `/metrics` body.
/// Matches a `name <value>` line; comments and other series are ignored.
pub fn parse_gauge(body: &str, name: &str) -> Option<u64> {
    let prefix = format!("{name} ");
    body.lines().find_map(|line| {
        let rest = line.trim().strip_prefix(&prefix)?;
        rest.parse::<u64>().ok()
    })
}

/// GET `{url}/metrics` and return the raw Prometheus text body.
pub fn fetch_metrics(url: &str) -> Result<String, CapacityError> {
    let url = format!("{}/metrics", url.trim_end_matches('/'));
    let response = ureq::get(&url)
        .call()
        .map_err(|e| CapacityError(format!("GET {url}: {e}")))?;
    response
        .into_body()
        .read_to_string()
        .map_err(|e| CapacityError(format!("read {url}: {e}")))
}

/// What a scale decision targets, given the node's currently committed
/// capacity. Returns `None` when the floor/ceiling already holds — the
/// caller leaves the file untouched and logs a no-op.
#[derive(Debug, Clone, PartialEq)]
pub struct Target {
    /// New `advertised_vcpus`.
    pub vcpus: u32,
    /// New `max_concurrent_jobs`, kept in the same ratio to vCPUs as the
    /// baseline so admitted capacity tracks advertised capacity.
    pub concurrent_jobs: u32,
    /// New `advertised_mem_mb`, scaled by the same ratio, when the baseline
    /// commits a memory figure (so the advertised shape stays honest).
    pub mem_mb: Option<u32>,
    /// The `capacity.toml` body to commit (only the knobs this engine owns).
    pub body: String,
}

/// Compute the `capacity.toml` body a scale decision should commit, or
/// `None` when the requested direction is already at its floor/ceiling.
pub fn scale_target(
    current: Option<&CapacityConfig>,
    scale: Scale,
    cfg: &AutoscaleConfig,
) -> Option<Target> {
    let base_vcpus = current
        .and_then(|c| c.advertised_vcpus)
        .unwrap_or(cfg.min_vcpus);
    let base_concurrent = current
        .and_then(|c| c.max_concurrent_jobs)
        .unwrap_or(cfg.min_concurrent_jobs);
    let base_mem = current.and_then(|c| c.advertised_mem_mb).unwrap_or(0);

    let next_vcpus = match scale {
        Scale::Up => base_vcpus.saturating_add(cfg.step_vcpus).min(cfg.max_vcpus),
        Scale::Down => base_vcpus.saturating_sub(cfg.step_vcpus).max(cfg.min_vcpus),
    };
    if next_vcpus == base_vcpus {
        return None;
    }

    let ratio = base_concurrent as f64 / base_vcpus.max(1) as f64;
    let concurrent_jobs = (ratio * next_vcpus as f64).round().clamp(
        cfg.min_concurrent_jobs as f64,
        cfg.max_concurrent_jobs as f64,
    ) as u32;
    let mem_mb = (base_mem > 0).then(|| {
        ((base_mem as f64 * next_vcpus as f64 / base_vcpus.max(1) as f64).round() as u32).max(1)
    });

    let mut body =
        format!("advertised_vcpus = {next_vcpus}\nmax_concurrent_jobs = {concurrent_jobs}\n");
    if let Some(m) = mem_mb {
        body.push_str(&format!("advertised_mem_mb = {m}\n"));
    }
    Some(Target {
        vcpus: next_vcpus,
        concurrent_jobs,
        mem_mb,
        body,
    })
}

/// Commit a controller-generated `body` over `out` atomically, skip-when-
/// unchanged, and append an audit event with `source` as the origin.
fn commit_generated(
    out: &Path,
    events: Option<&Path>,
    body: &str,
    source: &Path,
) -> Result<(), CapacityError> {
    let next: CapacityConfig =
        toml::from_str(body).map_err(|e| CapacityError(format!("generated capacity: {e}")))?;
    let prev = fs::read_to_string(out)
        .ok()
        .and_then(|raw| toml::from_str::<CapacityConfig>(&raw).ok());
    let changed = diff_config(prev.as_ref(), &next);
    if changed.is_empty() {
        return Ok(());
    }
    write_atomically(out, body)?;
    if let Some(ev) = events {
        append_event(ev, source, &changed)?;
    }
    Ok(())
}

/// Drive the autoscale loop forever: poll the node, decide, apply.
///
/// Fetch errors reset the hysteresis counters (never act on stale data) and
/// are logged; the loop keeps sampling.
pub fn run(cfg: AutoscaleConfig) {
    println!(
        "autoscale: polling {} for vtessera_demand_pct/running_pct every {}s; scaling {} within {}..={} vcpus",
        cfg.url, cfg.poll_secs, cfg.out.display(), cfg.min_vcpus, cfg.max_vcpus
    );
    let mut state = LoopState::default();
    loop {
        let sample = match fetch_metrics(&cfg.url) {
            Ok(body) => Sample {
                demand_pct: parse_gauge(&body, "vtessera_demand_pct").unwrap_or(0),
                running_pct: parse_gauge(&body, "vtessera_demand_running_pct").unwrap_or(0),
            },
            Err(e) => {
                eprintln!("vtessera-capacity autoscale: {e}");
                state = LoopState::default();
                std::thread::sleep(Duration::from_secs(cfg.poll_secs));
                continue;
            }
        };

        let now = Instant::now();
        if let Some(scale) = state.decide_at(now, sample, &cfg) {
            let current = fs::read_to_string(&cfg.out)
                .ok()
                .and_then(|raw| toml::from_str::<CapacityConfig>(&raw).ok());
            match scale_target(current.as_ref(), scale, &cfg) {
                Some(target) => {
                    let source = Path::new("autoscale");
                    match commit_generated(&cfg.out, cfg.events.as_deref(), &target.body, source) {
                        Ok(()) => println!(
                            "autoscale: {scale:?} -> vcpus={} concurrent={} mem={:?} (demand {}bps, running {}bps)",
                            target.vcpus,
                            target.concurrent_jobs,
                            target.mem_mb,
                            sample.demand_pct,
                            sample.running_pct
                        ),
                        Err(e) => eprintln!(
                            "autoscale: commit to {} failed: {e}",
                            cfg.out.display()
                        ),
                    }
                }
                None => println!("autoscale: {scale:?} requested at an existing bound"),
            }
        }
        std::thread::sleep(Duration::from_secs(cfg.poll_secs));
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::load;
    use std::fs;
    use std::path::PathBuf;

    fn temp_dir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("vta-{tag}"));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).expect("create temp dir");
        dir
    }

    fn cfg(f: impl FnOnce(&mut AutoscaleConfig)) -> AutoscaleConfig {
        let mut c = AutoscaleConfig {
            url: "http://127.0.0.1:9".into(),
            out: PathBuf::from("capacity.toml"),
            events: None,
            poll_secs: 30,
            min_vcpus: 2,
            max_vcpus: 16,
            min_concurrent_jobs: 1,
            max_concurrent_jobs: 8,
            step_vcpus: 2,
            up_pct: 8000,
            up_window_secs: 300,
            down_pct: 3000,
            down_window_secs: 900,
            cooldown_secs: 600,
        };
        f(&mut c);
        c
    }

    fn baseline() -> CapacityConfig {
        CapacityConfig {
            max_concurrent_jobs: Some(4),
            max_queue_len: None,
            advertised_vcpus: Some(8),
            advertised_mem_mb: Some(16384),
            price_multiplier: None,
            accept_workloads: None,
        }
    }

    #[test]
    fn validate_rejects_reversed_bounds() {
        let err = cfg(|c| c.max_vcpus = 1).validate().unwrap_err();
        assert!(err.to_string().contains("max_vcpus"), "err: {err}");
        let err = cfg(|c| c.up_pct = 3000).validate().unwrap_err();
        assert!(err.to_string().contains("hysteresis"), "err: {err}");
        assert!(cfg(|_| {}).validate().is_ok());
    }

    #[test]
    fn load_config_parses_and_validates() {
        let dir = temp_dir("load");
        let p = dir.join("autoscale.toml");
        fs::write(
            &p,
            "url = \"http://127.0.0.1:8402\"\nout = \"/tmp/opencode/capacity.toml\"\n\
             min_vcpus = 4\nmax_vcpus = 32\n",
        )
        .unwrap();
        let c = load_config(&p).unwrap();
        assert_eq!(c.min_vcpus, 4);
        assert_eq!(c.max_vcpus, 32);
        assert_eq!(c.poll_secs, 30); // default applied
        fs::write(&p, "url = \"x\"\nout = \"y\"\nbogus = 1\n").unwrap();
        let err = load_config(&p).unwrap_err();
        assert!(err.to_string().contains("unknown field"), "err: {err}");
    }

    #[test]
    fn parse_gauge_finds_number_among_other_lines() {
        let body =
            "# HELP vtessera_demand_pct ...\nvtessera_demand_pct 5000\nvtessera_queue_running 1\n";
        assert_eq!(parse_gauge(body, "vtessera_demand_pct"), Some(5000));
        assert_eq!(parse_gauge(body, "vtessera_demand_running_pct"), None);
        assert_eq!(parse_gauge("", "vtessera_demand_pct"), None);
        // Prefix collision: a longer gauge name is not a match.
        assert_eq!(
            parse_gauge("vtessera_demand_pct_extra 5\n", "vtessera_demand_pct"),
            None
        );
    }

    #[test]
    fn sustained_high_demand_scales_up() {
        let mut ls = LoopState::default();
        let c = cfg(|_| {});
        let t0 = Instant::now();
        let s = Sample {
            demand_pct: 9000,
            running_pct: 9000,
        };
        let up_needed = hits_needed(30, 300); // 10 samples
        for i in 0..up_needed as u128 {
            let at = t0 + Duration::from_secs(30 * i as u64);
            let got = ls.decide_at(at, s, &c);
            if i < up_needed as u128 - 1 {
                assert_eq!(got, None, "must wait for the window at sample {i}");
            } else {
                assert_eq!(got, Some(Scale::Up));
            }
        }
    }

    #[test]
    fn brief_spike_does_not_act() {
        let mut ls = LoopState::default();
        let c = cfg(|_| {});
        let t0 = Instant::now();
        let s = Sample {
            demand_pct: 9000,
            running_pct: 9000,
        };
        for i in 0..hits_needed(30, 300) - 2 {
            assert_eq!(ls.decide_at(t0 + Duration::from_secs(30 * i), s, &c), None);
        }
        // Demand evaporates before the window elapses — counters reset.
        let s = Sample {
            demand_pct: 5000,
            running_pct: 5000,
        };
        assert_eq!(ls.decide_at(t0 + Duration::from_secs(1000), s, &c), None);
    }

    #[test]
    fn sustained_idle_scales_down() {
        let mut ls = LoopState::default();
        let c = cfg(|_| {});
        let t0 = Instant::now();
        let idle = Sample {
            demand_pct: 500,
            running_pct: 500,
        };
        let down_needed = hits_needed(30, 900);
        for i in 0..down_needed as u128 {
            let got = ls.decide_at(t0 + Duration::from_secs(30 * i as u64), idle, &c);
            if i < down_needed as u128 - 1 {
                assert_eq!(got, None, "sample {i}");
            } else {
                assert_eq!(got, Some(Scale::Down));
            }
        }
    }

    #[test]
    fn cooldown_gates_nearby_second_action() {
        let mut ls = LoopState::default();
        let c = cfg(|x| {
            x.cooldown_secs = 100;
        });
        let t0 = Instant::now();
        let busy = Sample {
            demand_pct: 9000,
            running_pct: 9000,
        };
        for i in 0..hits_needed(30, 300) as u128 {
            ls.decide_at(t0 + Duration::from_secs(30 * i as u64), busy, &c);
        }
        // Still busy after the cooldown almost elapsed: blocked by cooldown.
        assert_eq!(ls.decide_at(t0 + Duration::from_secs(99), busy, &c), None);
        // After the cooldown passes, a fresh window re-arms an action.
        for i in 0..hits_needed(30, 300) as u128 {
            if let Some(scale) =
                ls.decide_at(t0 + Duration::from_secs(100 + 30 * i as u64), busy, &c)
            {
                assert_eq!(scale, Scale::Up);
                break;
            }
        }
    }

    #[test]
    fn scale_target_moves_vcpus_and_scales_ratio() {
        let c = cfg(|_| {});
        let up = scale_target(Some(&baseline()), Scale::Up, &c).unwrap();
        assert_eq!(up.vcpus, 10);
        assert_eq!(up.concurrent_jobs, 5); // 4/8 vcpus ratio kept
        assert_eq!(up.mem_mb, Some(20480)); // 16384*10/8
    }

    #[test]
    fn scale_target_respects_floors_and_ceilings() {
        let c = cfg(|x| {
            x.min_vcpus = 4;
            x.max_vcpus = 8;
        });
        // At the ceiling already: Up is a no-op (None).
        assert_eq!(
            scale_target(
                Some(&CapacityConfig {
                    advertised_vcpus: Some(8),
                    ..baseline()
                }),
                Scale::Up,
                &c
            ),
            None
        );
        // Down from min_vcpus clamps at the floor: already there, so the
        // requested direction is a no-op (None, not an infinite rewrite).
        assert_eq!(
            scale_target(
                Some(&CapacityConfig {
                    advertised_vcpus: Some(4),
                    ..baseline()
                }),
                Scale::Down,
                &c
            ),
            None
        );
    }

    #[test]
    fn scale_target_mem_proportionality_preserves_honest_shape() {
        let c = cfg(|_| {});
        let down = scale_target(Some(&baseline()), Scale::Down, &c).unwrap();
        assert_eq!(down.vcpus, 6);
        assert_eq!(down.mem_mb, Some(12288)); // 16384*6/8
                                              // Baseline with no mem knobs: mem not touched by the engine.
        let no_mem = CapacityConfig {
            advertised_vcpus: Some(8),
            max_concurrent_jobs: Some(4),
            advertised_mem_mb: None,
            ..baseline()
        };
        let t = scale_target(Some(&no_mem), Scale::Down, &c).unwrap();
        assert_eq!(t.mem_mb, None);
        assert!(!t.body.contains("advertised_mem_mb"));
    }

    #[test]
    fn commit_generated_writes_and_dedupes() {
        let dir = temp_dir("commit");
        let out = dir.join("capacity.toml");
        let events = dir.join("events.jsonl");
        let c = cfg(|_| {});
        let t = scale_target(None, Scale::Up, &c).unwrap(); // 2->4 vcpus
        commit_generated(&out, Some(&events), &t.body, Path::new("autoscale")).unwrap();
        let parsed = load(&out).unwrap();
        assert_eq!(parsed.advertised_vcpus, Some(4));
        assert_eq!(parsed.max_concurrent_jobs, Some(2)); // 1/2 floor ratio

        // Identical target: unchanged, no second event.
        commit_generated(&out, Some(&events), &t.body, Path::new("autoscale")).unwrap();
        let lines: Vec<_> = fs::read_to_string(&events)
            .unwrap()
            .lines()
            .map(|l| serde_json::from_str::<serde_json::Value>(l).unwrap())
            .collect();
        assert_eq!(lines.len(), 1);
        assert_eq!(lines[0]["source"], "autoscale");
    }
}
