//! GPU metering types and host-side nvidia-smi polling.
//!
//! [`GpuSample`] and [`GuestGpuMetering`] are always compiled (data-only).
//! The nvidia-smi polling thread ([`GpuMeter`]) is feature-gated on `gpu`.

// --- Always-compiled data types ---

/// Accumulated GPU metrics from host-side polling.
#[derive(Debug, Clone, Default, serde::Serialize, serde::Deserialize, PartialEq)]
pub struct GpuSample {
    /// Wall-clock seconds the GPU was accessible to this job.
    pub gpu_seconds: f64,
    /// Time-weighted VRAM integral in GB-hours.
    pub vram_gb_hours: f64,
    /// Average GPU utilization percentage (0–100).
    pub avg_gpu_util_pct: f32,
    /// Average power draw in watts.
    pub avg_power_watts: f32,
    /// Peak VRAM usage in MB.
    pub peak_vram_mb: u32,
    /// Number of polling samples taken.
    pub samples: u32,
}

/// Guest-side GPU metrics (written by the guest runner inside the VM).
#[derive(Debug, Clone, Default, serde::Serialize, serde::Deserialize, PartialEq)]
pub struct GuestGpuMetering {
    /// Wall-clock GPU time from the guest's perspective.
    pub gpu_seconds: f64,
    /// Peak VRAM used inside the guest (MB).
    pub vram_mb_peak: u32,
    /// Average VRAM used inside the guest (MB).
    pub vram_mb_avg: f32,
    /// Average GPU utilization percentage (0–100).
    pub gpu_util_avg_pct: f32,
    /// Guest NVIDIA driver version string.
    pub driver_version: String,
}

/// Parse a nvidia-smi CSV line into `(vram_mb, util_pct, power_watts)`.
pub fn parse_nvidia_smi_csv(line: &str) -> Option<(u32, f32, f32)> {
    let parts: Vec<&str> = line.split(',').map(|s| s.trim()).collect();
    if parts.len() < 3 {
        return None;
    }
    let vram_mb = parts[0].parse::<u32>().ok()?;
    let util_pct = parts[1].parse::<f32>().ok()?;
    let power_w = parts[2].parse::<f32>().ok()?;
    Some((vram_mb, util_pct, power_w))
}

// --- Feature-gated: nvidia-smi polling ---

#[cfg(feature = "gpu")]
mod polling {
    use std::process::Command;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::Arc;
    use std::thread::{self, JoinHandle};
    use std::time::{Duration, Instant};

    use super::GpuSample;

    /// Background GPU meter spawned alongside a Cloud Hypervisor job.
    pub struct GpuMeter {
        #[allow(dead_code)]
        pci_address: String,
        stop: Arc<AtomicBool>,
        handle: Option<JoinHandle<GpuSample>>,
    }

    impl GpuMeter {
        /// Start polling GPU metrics for the given PCI address.
        pub fn start(pci_address: &str, poll_interval: Duration) -> Self {
            let stop = Arc::new(AtomicBool::new(false));
            let stop_clone = stop.clone();
            let pci = pci_address.to_string();

            let handle = thread::spawn(move || poll_gpu(&pci, poll_interval, stop_clone));

            Self {
                pci_address: pci_address.to_string(),
                stop,
                handle: Some(handle),
            }
        }

        /// Stop polling and return the accumulated sample.
        pub fn stop(&mut self) -> Option<GpuSample> {
            self.stop.store(true, Ordering::Relaxed);
            self.handle.take().and_then(|h| h.join().ok())
        }
    }

    /// Poll nvidia-smi and accumulate GPU metrics until `stop` is set.
    fn poll_gpu(pci: &str, interval: Duration, stop: Arc<AtomicBool>) -> GpuSample {
        let mut sample = GpuSample::default();
        let mut last_time = Instant::now();
        let mut warned_no_nvidia_smi = false;

        while !stop.load(Ordering::Relaxed) {
            match query_nvidia_smi(pci) {
                Some((vram_mb, util_pct, power_w)) => {
                    let now = Instant::now();
                    let dt = last_time.elapsed().as_secs_f64();
                    last_time = now;

                    if sample.samples > 0 {
                        sample.vram_gb_hours += (vram_mb as f64 / 1024.0) * dt / 3600.0;
                        sample.gpu_seconds += dt;
                    }

                    let total_time = sample.gpu_seconds;
                    if total_time > 0.0 {
                        let w = (dt / total_time) as f32;
                        sample.avg_gpu_util_pct =
                            sample.avg_gpu_util_pct * (1.0 - w) + util_pct * w;
                        sample.avg_power_watts = sample.avg_power_watts * (1.0 - w) + power_w * w;
                    } else {
                        sample.avg_gpu_util_pct = util_pct;
                        sample.avg_power_watts = power_w;
                    }

                    if vram_mb > sample.peak_vram_mb {
                        sample.peak_vram_mb = vram_mb;
                    }

                    sample.samples += 1;
                }
                None => {
                    if !warned_no_nvidia_smi {
                        eprintln!("gpu_meter: nvidia-smi unavailable for {pci}, metering disabled");
                        warned_no_nvidia_smi = true;
                    }
                }
            }

            if stop.load(Ordering::Relaxed) {
                break;
            }
            thread::sleep(interval);
        }

        sample
    }

    /// Query nvidia-smi for a single GPU.
    fn query_nvidia_smi(pci: &str) -> Option<(u32, f32, f32)> {
        let output = Command::new("nvidia-smi")
            .args([
                "--query-gpu=memory.used,utilization.gpu,power.draw",
                "--format=csv,noheader,nounits",
                "--id",
                pci,
            ])
            .output()
            .ok()?;

        if !output.status.success() {
            return None;
        }

        let stdout = String::from_utf8_lossy(&output.stdout);
        let line = stdout.trim();

        let parts: Vec<&str> = line.split(',').map(|s| s.trim()).collect();
        if parts.len() < 3 {
            return None;
        }

        let vram_mb = parts[0].parse::<u32>().ok()?;
        let util_pct = parts[1].parse::<f32>().ok()?;
        let power_w = parts[2].parse::<f32>().ok()?;

        Some((vram_mb, util_pct, power_w))
    }

    /// Detect if a GPU is available inside the guest via nvidia-smi.
    pub fn detect_guest_gpu() -> bool {
        Command::new("nvidia-smi")
            .args(["--query-gpu=driver_version", "--format=csv,noheader"])
            .output()
            .map(|o| o.status.success())
            .unwrap_or(false)
    }

    /// Query guest-side GPU metrics. Returns `None` if nvidia-smi is unavailable.
    pub fn query_guest_gpu() -> Option<super::GuestGpuMetering> {
        let output = Command::new("nvidia-smi")
            .args([
                "--query-gpu=driver_version,memory.used,memory.total,utilization.gpu",
                "--format=csv,noheader,nounits",
            ])
            .output()
            .ok()?;

        if !output.status.success() {
            return None;
        }

        let stdout = String::from_utf8_lossy(&output.stdout);
        let line = stdout.trim();

        let parts: Vec<&str> = line.split(',').map(|s| s.trim()).collect();
        if parts.len() < 4 {
            return None;
        }

        let driver_version = parts[0].to_string();
        let vram_used_mb = parts[1].parse::<u32>().ok()?;
        let _vram_total_mb = parts[2].parse::<u32>().ok()?;
        let util_pct = parts[3].parse::<f32>().ok()?;

        Some(super::GuestGpuMetering {
            gpu_seconds: 0.0,
            vram_mb_peak: vram_used_mb,
            vram_mb_avg: vram_used_mb as f32,
            gpu_util_avg_pct: util_pct,
            driver_version,
        })
    }
}

#[cfg(feature = "gpu")]
pub use polling::*;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn gpu_sample_default_is_zero() {
        let s = GpuSample::default();
        assert_eq!(s.gpu_seconds, 0.0);
        assert_eq!(s.vram_gb_hours, 0.0);
        assert_eq!(s.avg_gpu_util_pct, 0.0);
        assert_eq!(s.avg_power_watts, 0.0);
        assert_eq!(s.peak_vram_mb, 0);
        assert_eq!(s.samples, 0);
    }

    #[cfg(feature = "cloud-hypervisor")]
    #[test]
    fn gpu_sample_roundtrip_json() {
        let sample = GpuSample {
            gpu_seconds: 3600.0,
            vram_gb_hours: 0.42,
            avg_gpu_util_pct: 85.3,
            avg_power_watts: 280.5,
            peak_vram_mb: 16384,
            samples: 3600,
        };
        let json = serde_json::to_string(&sample).unwrap();
        let parsed: GpuSample = serde_json::from_str(&json).unwrap();
        assert_eq!(sample, parsed);
    }

    #[test]
    fn parse_nvidia_smi_csv_line() {
        let line = "16384, 85, 280.5";
        let (vram, util, power) = parse_nvidia_smi_csv(line).unwrap();
        assert_eq!(vram, 16384);
        assert_eq!(util, 85.0);
        assert_eq!(power, 280.5);
    }

    #[test]
    fn parse_nvidia_smi_csv_with_spaces() {
        let line = "  8192 , 42 , 150.0 ";
        let (vram, util, power) = parse_nvidia_smi_csv(line).unwrap();
        assert_eq!(vram, 8192);
        assert_eq!(util, 42.0);
        assert_eq!(power, 150.0);
    }

    #[test]
    fn parse_nvidia_smi_csv_too_few_fields() {
        assert!(parse_nvidia_smi_csv("16384, 85").is_none());
    }

    #[test]
    fn parse_nvidia_smi_csv_non_numeric() {
        assert!(parse_nvidia_smi_csv("abc, def, ghi").is_none());
    }

    #[cfg(feature = "cloud-hypervisor")]
    #[test]
    fn guest_gpu_metering_roundtrip() {
        let g = GuestGpuMetering {
            gpu_seconds: 120.0,
            vram_mb_peak: 8192,
            vram_mb_avg: 4096.0,
            gpu_util_avg_pct: 65.5,
            driver_version: "535.129.03".into(),
        };
        let json = serde_json::to_string(&g).unwrap();
        let parsed: GuestGpuMetering = serde_json::from_str(&json).unwrap();
        assert_eq!(g, parsed);
    }
}
