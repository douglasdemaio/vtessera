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
/// CPU percentage is reported as 0.0 in v0 since a single sample cannot
/// compute a delta. Disk free is reported as a placeholder value since
/// obtaining it requires `statvfs(2)` (unsafe) and is not critical in v0.
pub fn sample(_state_dir: &str) -> io::Result<ResourceSample> {
    let ts_unix = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();

    let cpu_pct = 0.0;
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
    fn test_sample() {
        let result = sample("/");
        assert!(result.is_ok(), "sample failed: {:?}", result.err());
        let s = result.unwrap();
        assert!(s.ts_unix > 0, "timestamp should be > 0");
        assert!(s.mem_used_kb > 0, "mem_used_kb should be > 0");
    }
}
