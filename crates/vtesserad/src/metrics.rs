use std::fs;
use std::io;
use std::time::{SystemTime, UNIX_EPOCH};

/// A single point-in-time sample of machine resource usage.
#[derive(Debug, Clone, Copy)]
pub struct ResourceSample {
    pub ts_unix: u64,
    pub cpu_pct: f64,
    pub mem_used_kb: u64,
    pub disk_free_kb: u64,
}

/// Persistent state for computing CPU percentage across samples.
/// Holds the previous `/proc/stat` jiffies and wall-clock timestamp.
#[derive(Debug, Clone, Default)]
pub struct CpuState {
    prev_idle: u64,
    prev_total: u64,
    prev_ts: u64,
}

/// Parse the first "cpu" line of `/proc/stat` → (idle_jiffies, total_jiffies).
fn read_proc_stat() -> io::Result<(u64, u64)> {
    let content = fs::read_to_string("/proc/stat")?;
    let line = content
        .lines()
        .next()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "empty /proc/stat"))?;
    // cpu  user nice system idle iowait irq softirq steal guest guest_nice
    let parts: Vec<u64> = line
        .split_whitespace()
        .skip(1) // skip "cpu"
        .filter_map(|s| s.parse().ok())
        .collect();
    if parts.len() < 4 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "unexpected /proc/stat format",
        ));
    }
    let idle = parts[3];
    let total: u64 = parts.iter().sum();
    Ok((idle, total))
}

/// Read `/proc/meminfo` and return used memory in kB (total - available).
fn read_mem_used_kb() -> io::Result<u64> {
    let meminfo = fs::read_to_string("/proc/meminfo")?;
    let mut total_kb = 0u64;
    let mut avail_kb = 0u64;
    for line in meminfo.lines() {
        if let Some(rest) = line.strip_prefix("MemTotal:") {
            total_kb = parse_kb_value(rest)?;
        } else if let Some(rest) = line.strip_prefix("MemAvailable:") {
            avail_kb = parse_kb_value(rest)?;
        }
    }
    if total_kb == 0 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "MemTotal not found in /proc/meminfo",
        ));
    }
    Ok(total_kb.saturating_sub(avail_kb))
}

fn parse_kb_value(s: &str) -> io::Result<u64> {
    let trimmed = s.trim();
    let num_str = trimmed
        .split_whitespace()
        .next()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "empty value in meminfo"))?;
    num_str
        .parse::<u64>()
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "failed to parse meminfo value"))
}

/// Collect a single resource sample.
///
/// CPU percentage is computed from the delta between this sample and the
/// previous one using `/proc/stat`. On the first call (no previous state)
/// CPU is reported as 0.0 since a delta requires two points.
pub fn sample(_state_dir: &str, cpu_state: &mut CpuState) -> io::Result<ResourceSample> {
    let ts_unix = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();

    let (idle, total) = read_proc_stat()?;

    let cpu_pct = if cpu_state.prev_ts > 0 {
        let delta_total = total.saturating_sub(cpu_state.prev_total);
        let delta_idle = idle.saturating_sub(cpu_state.prev_idle);
        if delta_total > 0 {
            ((delta_total - delta_idle) as f64 / delta_total as f64) * 100.0
        } else {
            0.0
        }
    } else {
        0.0
    };

    // Update state for next sample.
    cpu_state.prev_idle = idle;
    cpu_state.prev_total = total;
    cpu_state.prev_ts = ts_unix;

    let mem_used_kb = read_mem_used_kb()?;
    let disk_free_kb = 0;

    Ok(ResourceSample {
        ts_unix,
        cpu_pct,
        mem_used_kb,
        disk_free_kb,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_read_mem_used_kb() {
        let result = read_mem_used_kb();
        assert!(
            result.is_ok(),
            "read_mem_used_kb failed: {:?}",
            result.err()
        );
        let val = result.unwrap();
        assert!(val > 0, "mem_used_kb should be > 0, got {val}");
    }

    #[test]
    fn test_sample_first_call_zero_cpu() {
        let mut state = CpuState::default();
        let result = sample("/", &mut state);
        assert!(result.is_ok(), "sample failed: {:?}", result.err());
        let s = result.unwrap();
        assert!(s.ts_unix > 0, "timestamp should be > 0");
        assert!(s.mem_used_kb > 0, "mem_used_kb should be > 0");
        // First call always returns 0.0 for CPU (no delta).
        assert_eq!(s.cpu_pct, 0.0);
    }

    #[test]
    fn test_sample_second_call_has_cpu() {
        let mut state = CpuState::default();
        // First sample — primes the state.
        let _ = sample("/", &mut state).unwrap();
        // Small sleep so jiffies advance.
        std::thread::sleep(std::time::Duration::from_millis(50));
        // Second sample — should compute a real CPU percentage.
        let s = sample("/", &mut state).unwrap();
        assert!(
            s.cpu_pct >= 0.0 && s.cpu_pct <= 100.0,
            "cpu_pct out of range: {}",
            s.cpu_pct
        );
    }

    #[test]
    fn test_read_proc_stat() {
        let result = read_proc_stat();
        assert!(result.is_ok(), "read_proc_stat failed: {:?}", result.err());
        let (idle, total) = result.unwrap();
        assert!(total > 0, "total jiffies should be > 0");
        assert!(idle > 0, "idle jiffies should be > 0");
        assert!(idle <= total, "idle should be <= total");
    }
}
