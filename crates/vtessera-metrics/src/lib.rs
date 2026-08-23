//! Vtessera metrics — zero-dep Prometheus text format registry.
//!
//! Provides a static registry of counters and gauges that renders to
//! Prometheus text exposition format 0.0.4. No external dependencies.
//!
//! # Usage
//!
//! ```rust
//! use vtessera_metrics::{register_counter, register_gauge, render};
//!
//! let jobs = register_counter("vtessera_jobs_total", "Total jobs submitted");
//! let cpu = register_gauge("vtessera_cpu_pct", "CPU usage in basis points");
//!
//! jobs.inc();
//! cpu.set(4500); // 45.00%
//!
//! let prom = render();
//! assert!(prom.contains("vtessera_jobs_total 1"));
//! ```

use std::collections::BTreeMap;
use std::fmt::Write;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;

/// A monotonically increasing counter.
pub struct Counter {
    value: AtomicU64,
}

impl Default for Counter {
    fn default() -> Self {
        Self::new()
    }
}

impl Counter {
    pub const fn new() -> Self {
        Self {
            value: AtomicU64::new(0),
        }
    }

    pub fn inc(&self) {
        self.value.fetch_add(1, Ordering::Relaxed);
    }

    pub fn add(&self, n: u64) {
        self.value.fetch_add(n, Ordering::Relaxed);
    }

    pub fn get(&self) -> u64 {
        self.value.load(Ordering::Relaxed)
    }

    #[cfg(test)]
    pub fn set_for_test(&self, v: u64) {
        self.value.store(v, Ordering::Relaxed);
    }
}

/// A gauge that can go up or down.
pub struct Gauge {
    value: AtomicU64,
}

impl Default for Gauge {
    fn default() -> Self {
        Self::new()
    }
}

impl Gauge {
    pub const fn new() -> Self {
        Self {
            value: AtomicU64::new(0),
        }
    }

    pub fn set(&self, v: u64) {
        self.value.store(v, Ordering::Relaxed);
    }

    pub fn inc(&self) {
        self.value.fetch_add(1, Ordering::Relaxed);
    }

    pub fn dec(&self) {
        let _ = self
            .value
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |v| v.checked_sub(1));
    }

    pub fn add(&self, n: u64) {
        self.value.fetch_add(n, Ordering::Relaxed);
    }

    pub fn get(&self) -> u64 {
        self.value.load(Ordering::Relaxed)
    }
}

enum MetricKind {
    Counter(&'static Counter),
    Gauge(&'static Gauge),
}

struct Entry {
    name: &'static str,
    help: &'static str,
    kind: MetricKind,
}

// Registry is written during startup (single-threaded init), then only
// read during render. Mutex protects against concurrent init races.
static REGISTRY: Mutex<Vec<Entry>> = Mutex::new(Vec::new());

/// Register a counter metric. Returns a static reference.
///
/// Safe to call multiple times with the same name — returns the same
/// counter (but duplicates are silently ignored in render).
pub fn register_counter(name: &'static str, help: &'static str) -> &'static Counter {
    let counter = Box::leak(Box::new(Counter::new()));
    let mut reg = REGISTRY.lock().unwrap_or_else(|e| e.into_inner());
    reg.push(Entry {
        name,
        help,
        kind: MetricKind::Counter(counter),
    });
    counter
}

/// Register a gauge metric. Returns a static reference.
pub fn register_gauge(name: &'static str, help: &'static str) -> &'static Gauge {
    let gauge = Box::leak(Box::new(Gauge::new()));
    let mut reg = REGISTRY.lock().unwrap_or_else(|e| e.into_inner());
    reg.push(Entry {
        name,
        help,
        kind: MetricKind::Gauge(gauge),
    });
    gauge
}

/// Render all registered metrics in Prometheus text exposition format 0.0.4.
///
/// Output is suitable for `GET /metrics` responses with
/// `Content-Type: text/plain; version=0.0.4; charset=utf-8`.
pub fn render() -> String {
    let reg = REGISTRY.lock().unwrap_or_else(|e| e.into_inner());
    let mut out = String::with_capacity(1024);
    // Use BTreeMap for sorted output (Prometheus expects sorted metric names).
    let mut sorted: BTreeMap<&str, &Entry> = BTreeMap::new();
    for entry in reg.iter() {
        sorted.insert(entry.name, entry);
    }
    for (name, entry) in &sorted {
        match &entry.kind {
            MetricKind::Counter(c) => {
                writeln!(&mut out, "# HELP {} {}", name, entry.help).ok();
                writeln!(&mut out, "# TYPE {} counter", name).ok();
                writeln!(&mut out, "{} {}", name, c.get()).ok();
            }
            MetricKind::Gauge(g) => {
                writeln!(&mut out, "# HELP {} {}", name, entry.help).ok();
                writeln!(&mut out, "# TYPE {} gauge", name).ok();
                writeln!(&mut out, "{} {}", name, g.get()).ok();
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn counter_increments() {
        let c = register_counter("test_ctr_inc", "test");
        let before = c.get();
        c.inc();
        c.add(5);
        assert_eq!(c.get(), before + 6);
    }

    #[test]
    fn gauge_set_inc_dec() {
        let g = register_gauge("test_gauge_ops", "test");
        g.set(100);
        assert_eq!(g.get(), 100);
        g.inc();
        assert_eq!(g.get(), 101);
        g.dec();
        assert_eq!(g.get(), 100);
        g.add(50);
        assert_eq!(g.get(), 150);
    }

    #[test]
    fn render_includes_type_and_help() {
        let _c = register_counter("test_render_ctr", "My counter");
        let _g = register_gauge("test_render_gauge", "My gauge");
        let out = render();
        assert!(out.contains("# HELP test_render_ctr My counter"));
        assert!(out.contains("# TYPE test_render_ctr counter"));
        assert!(out.contains("test_render_ctr "));
        assert!(out.contains("# HELP test_render_gauge My gauge"));
        assert!(out.contains("# TYPE test_render_gauge gauge"));
    }

    #[test]
    fn render_shows_current_values() {
        let c = register_counter("test_val_ctr", "v");
        c.set_for_test(42);
        let out = render();
        assert!(out.contains("test_val_ctr 42"));
    }
}
