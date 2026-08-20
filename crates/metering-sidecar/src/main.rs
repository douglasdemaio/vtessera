use std::fs;
use std::path::Path;
use std::process::Command;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
#[allow(dead_code)]
struct JobManifest {
    job_id: String,
    command: Vec<String>,
    env: Vec<(String, String)>,
    vcpus: u32,
    mem_kb: u64,
    max_duration_secs: u64,
}

#[derive(Debug, Clone, Serialize)]
struct GuestMetering {
    cpu_seconds: f64,
    peak_mem_kb: u64,
    elapsed_secs: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    gpu: Option<GuestGpuSelfReport>,
}

#[derive(Debug, Clone, Serialize)]
struct GuestGpuSelfReport {
    gpu_seconds: f64,
    vram_mb_peak: u32,
    vram_mb_avg: f32,
    gpu_util_avg_pct: f32,
    driver_version: String,
}

#[derive(Debug, Clone, Serialize)]
struct GuestResult {
    exit_code: i32,
}

fn read_manifest(path: &Path) -> Result<JobManifest, String> {
    let data = fs::read_to_string(path)
        .map_err(|e| format!("failed to read manifest {}: {e}", path.display()))?;
    serde_json::from_str(&data)
        .map_err(|e| format!("failed to parse manifest: {e}"))
}

fn get_cpu_usage(pid: u32) -> Result<(f64, u64), String> {
    let stat_path = format!("/proc/{pid}/stat");
    let status_path = format!("/proc/{pid}/status");

    let stat = fs::read_to_string(&stat_path)
        .map_err(|e| format!("failed to read {stat_path}: {e}"))?;

    let fields: Vec<&str> = stat.split_whitespace().collect();
    if fields.len() < 22 {
        return Err("insufficient fields in /proc/pid/stat".into());
    }

    let utime: u64 = fields[13]
        .parse()
        .map_err(|e| format!("failed to parse utime: {e}"))?;
    let stime: u64 = fields[14]
        .parse()
        .map_err(|e| format!("failed to parse stime: {e}"))?;

    let ticks_per_sec = 100.0;
    let cpu_seconds = (utime + stime) as f64 / ticks_per_sec;

    let status = fs::read_to_string(&status_path)
        .map_err(|e| format!("failed to read {status_path}: {e}"))?;

    let peak_mem_kb = status
        .lines()
        .find(|l| l.starts_with("VmPeak:"))
        .and_then(|l| l.split_whitespace().nth(1))
        .and_then(|s| s.parse::<u64>().ok())
        .unwrap_or(0);

    Ok((cpu_seconds, peak_mem_kb))
}

fn get_gpu_metrics() -> Option<GuestGpuSelfReport> {
    let output = Command::new("nvidia-smi")
        .args([
            "--query-gpu=utilization.gpu,memory.used,memory.total,driver_version",
            "--format=csv,noheader,nounits",
        ])
        .output()
        .ok()?;

    if !output.status.success() {
        return None;
    }

    let stdout = String::from_utf8_lossy(&output.stdout);
    let line = stdout.lines().next()?;
    let fields: Vec<&str> = line.split(", ").collect();
    if fields.len() < 4 {
        return None;
    }

    let gpu_util: f32 = fields[0].parse().ok()?;
    let vram_used_mb: u32 = fields[1].parse().ok()?;
    let _vram_total_mb: u32 = fields[2].parse().ok()?;
    let driver_version = fields[3].to_string();

    Some(GuestGpuSelfReport {
        gpu_seconds: 0.0,
        vram_mb_peak: vram_used_mb,
        vram_mb_avg: vram_used_mb as f32,
        gpu_util_avg_pct: gpu_util,
        driver_version,
    })
}

fn wait_for_process(pid: u32, max_duration_secs: u64) -> (i32, Instant) {
    let start = Instant::now();
    let max = Duration::from_secs(max_duration_secs);

    loop {
        let status_path = format!("/proc/{pid}");
        if !Path::new(&status_path).exists() {
            return (0, start);
        }

        if start.elapsed() >= max {
            let _ = Command::new("kill").arg("-9").arg(pid.to_string()).output();
            return (124, start);
        }

        std::thread::sleep(Duration::from_millis(100));
    }
}

fn write_metering(path: &Path, metering: &GuestMetering) -> Result<(), String> {
    let json = serde_json::to_string_pretty(metering)
        .map_err(|e| format!("failed to serialize metering: {e}"))?;
    fs::write(path, json)
        .map_err(|e| format!("failed to write {}: {e}", path.display()))
}

fn write_result(path: &Path, result: &GuestResult) -> Result<(), String> {
    let json = serde_json::to_string_pretty(result)
        .map_err(|e| format!("failed to serialize result: {e}"))?;
    fs::write(path, json)
        .map_err(|e| format!("failed to write {}: {e}", path.display()))
}

fn main() {
    let manifest_path = std::env::var("MANIFEST_PATH")
        .unwrap_or_else(|_| "/mnt/vtessera/manifest.json".into());
    let output_dir = std::env::var("OUTPUT_DIR")
        .unwrap_or_else(|_| "/mnt/vtessera/out".into());
    let job_id = std::env::var("JOB_ID").unwrap_or_else(|_| "unknown".into());

    eprintln!("metering-sidecar: starting for job {job_id}");
    eprintln!("metering-sidecar: manifest={manifest_path}, output={output_dir}");

    let manifest = match read_manifest(Path::new(&manifest_path)) {
        Ok(m) => m,
        Err(e) => {
            eprintln!("metering-sidecar: fatal: {e}");
            std::process::exit(1);
        }
    };

    let pid = std::process::id();

    let gpu_enabled = std::env::var("GPU_ENABLED").ok().as_deref() == Some("1");
    let mut gpu_metrics = if gpu_enabled { get_gpu_metrics() } else { None };

    let (exit_code, start) = wait_for_process(pid + 1, manifest.max_duration_secs);

    let elapsed = start.elapsed();
    let elapsed_secs = elapsed.as_secs().max(1);

    let (cpu_seconds, peak_mem_kb) = get_cpu_usage(pid + 1).unwrap_or((0.0, 0));

    if gpu_enabled {
        gpu_metrics = get_gpu_metrics();
        if let Some(ref mut g) = gpu_metrics {
            g.gpu_seconds = elapsed_secs as f64;
        }
    }

    let metering = GuestMetering {
        cpu_seconds,
        peak_mem_kb,
        elapsed_secs,
        gpu: gpu_metrics,
    };

    let output_path = Path::new(&output_dir);
    if let Err(e) = fs::create_dir_all(output_path) {
        eprintln!("metering-sidecar: failed to create output dir: {e}");
        std::process::exit(1);
    }

    if let Err(e) = write_metering(&output_path.join("metering.json"), &metering) {
        eprintln!("metering-sidecar: failed to write metering: {e}");
        std::process::exit(1);
    }

    if let Err(e) = write_result(
        &output_path.join("result.json"),
        &GuestResult { exit_code },
    ) {
        eprintln!("metering-sidecar: failed to write result: {e}");
        std::process::exit(1);
    }

    eprintln!("metering-sidecar: done, exit_code={exit_code}, cpu={cpu_seconds:.1}s, elapsed={elapsed_secs}s");
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn manifest_roundtrip() {
        let manifest = JobManifest {
            job_id: "test-job".into(),
            command: vec!["echo".into(), "hello".into()],
            env: vec![("FOO".into(), "bar".into())],
            vcpus: 4,
            mem_kb: 8192,
            max_duration_secs: 3600,
        };
        let json = serde_json::to_string(&manifest).unwrap();
        let parsed: JobManifest = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed.job_id, "test-job");
        assert_eq!(parsed.vcpus, 4);
    }

    #[test]
    fn metering_serialization() {
        let metering = GuestMetering {
            cpu_seconds: 123.45,
            peak_mem_kb: 1024,
            elapsed_secs: 60,
            gpu: None,
        };
        let json = serde_json::to_string(&metering).unwrap();
        assert!(json.contains("cpu_seconds"));
        assert!(json.contains("123.45"));
    }

    #[test]
    fn result_serialization() {
        let result = GuestResult { exit_code: 0 };
        let json = serde_json::to_string(&result).unwrap();
        assert!(json.contains("exit_code"));
        assert!(json.contains("0"));
    }
}
