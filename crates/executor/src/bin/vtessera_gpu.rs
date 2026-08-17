//! `vtessera-gpu` — helper binary for GPU VFIO binding lifecycle.
//!
//! Manages PCI device binding to vfio-pci and writes a state file with
//! GPU metadata that the executor reads for discovery. Requires root or
//! CAP_SYS_ADMIN for PCI manipulation.
//!
//! Usage:
//!   vtessera-gpu bind --device 0000:01:00.0
//!   vtessera-gpu unbind --device 0000:01:00.0
//!   vtessera-gpu list

use std::collections::HashMap;
use std::env;
use std::fs;
use std::path::PathBuf;
use std::process;
use std::time::{SystemTime, UNIX_EPOCH};

const STATE_DIR: &str = "/var/lib/vtessera";
const STATE_FILE: &str = "/var/lib/vtessera/gpus.json";

#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct GpuDevice {
    pub pci_address: String,
    pub vendor: String,
    pub model: String,
    pub vram_mb: u32,
    pub bound_at: String,
}

fn usage() -> ! {
    eprintln!("Usage: vtessera-gpu <bind|unbind|list> [--device <PCI_ADDRESS>]");
    process::exit(1);
}

/// Validate and normalize a PCI address.
/// Accepts both full (`0000:01:00.0`) and short (`01:00.0`) forms.
/// Returns the full 4-part form with lowercase hex.
fn parse_pci_address(addr: &str) -> Result<String, String> {
    let addr = addr.to_lowercase();
    let parts: Vec<&str> = addr.split(':').collect();

    let (domain, bus, dev_func) = match parts.len() {
        3 => (parts[0], parts[1], parts[2]),
        2 => ("0000", parts[0], parts[1]),
        _ => {
            return Err(format!(
                "invalid PCI address: {addr} (expected DDDD:BB:DD.F or BB:DD.F)"
            ))
        }
    };

    let dev_func_parts: Vec<&str> = dev_func.split('.').collect();
    if dev_func_parts.len() != 2 {
        return Err(format!(
            "invalid PCI address: {addr} (expected DDDD:BB:DD.F)"
        ));
    }
    let dev = dev_func_parts[0];
    let func = dev_func_parts[1];

    // Validate hex digits
    for (label, val, expected_len) in [
        ("domain", domain, 4),
        ("bus", bus, 2),
        ("device", dev, 2),
        ("function", func, 1),
    ] {
        if val.len() != expected_len || !val.chars().all(|c| c.is_ascii_hexdigit()) {
            return Err(format!(
                "invalid PCI {label}: {val} (expected {expected_len} hex digits)"
            ));
        }
    }

    Ok(format!("{domain}:{bus}:{dev}.{func}"))
}

fn sysfs_path(pci_addr: &str, attr: &str) -> PathBuf {
    PathBuf::from(format!("/sys/bus/pci/devices/{pci_addr}/{attr}"))
}

fn read_sysfs_attr(pci_addr: &str, attr: &str) -> Result<String, String> {
    let path = sysfs_path(pci_addr, attr);
    fs::read_to_string(&path)
        .map(|s| s.trim().to_string())
        .map_err(|e| format!("failed to read {}: {e}", path.display()))
}

fn current_driver(pci_addr: &str) -> Result<Option<String>, String> {
    let driver_link = sysfs_path(pci_addr, "driver");
    match fs::read_link(&driver_link) {
        Ok(target) => {
            let name = target
                .file_name()
                .map(|n| n.to_string_lossy().to_string())
                .unwrap_or_default();
            Ok(Some(name))
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(e) => Err(format!("failed to read driver link: {e}")),
    }
}

fn load_state() -> Vec<GpuDevice> {
    fs::read_to_string(STATE_FILE)
        .ok()
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or_default()
}

fn save_state(devices: &[GpuDevice]) -> Result<(), String> {
    fs::create_dir_all(STATE_DIR).map_err(|e| format!("failed to create {STATE_DIR}: {e}"))?;
    let json = serde_json::to_string_pretty(devices)
        .map_err(|e| format!("failed to serialize state: {e}"))?;
    fs::write(STATE_FILE, format!("{json}\n"))
        .map_err(|e| format!("failed to write {STATE_FILE}: {e}"))
}

fn timestamp_now() -> String {
    let secs = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();
    format!("{secs}")
}

fn pci_vendor_name(vendor_id: &str) -> Option<&'static str> {
    match vendor_id {
        "0x10de" => Some("nvidia"),
        "0x1002" => Some("amd"),
        _ => None,
    }
}

/// Static lookup table: PCI device ID → (model, vram_mb).
/// Covers common NVIDIA and AMD datacenter GPUs.
fn gpu_model_lookup(vendor: &str, device_id: &str) -> (&'static str, u32) {
    let table: HashMap<(&str, &str), (&str, u32)> = HashMap::from([
        // NVIDIA datacenter GPUs
        (("nvidia", "0x1b30"), ("H100-80GB", 81920)),
        (("nvidia", "0x20b0"), ("H200-141GB", 146944)),
        (("nvidia", "0x20f1"), ("H100-NVL-94GB", 96256)),
        (("nvidia", "0x1db5"), ("A100-80GB", 81920)),
        (("nvidia", "0x1db6"), ("A100-40GB", 40960)),
        (("nvidia", "0x25b6"), ("A100-SXM4-80GB", 81920)),
        (("nvidia", "0x2782"), ("L40S-48GB", 49152)),
        (("nvidia", "0x2783"), ("L4-24GB", 24576)),
        (("nvidia", "0x20b5"), ("RTX-6000-48GB", 49152)),
        (("nvidia", "0x2230"), ("RTX-5090-32GB", 32768)),
        // AMD datacenter GPUs
        (("amd", "0x740c"), ("MI300X-192GB", 196608)),
        (("amd", "0x7408"), ("MI300A-128GB", 131072)),
        (("amd", "0x738c"), ("MI250X-128GB", 131072)),
        (("amd", "0x738e"), ("MI250-128GB", 131072)),
    ]);

    table
        .get(&(vendor, device_id))
        .copied()
        .unwrap_or(("unknown", 0))
}

fn detect_gpu(pci_addr: &str) -> Result<GpuDevice, String> {
    let vendor_hex = read_sysfs_attr(pci_addr, "vendor")?;
    let vendor_name =
        pci_vendor_name(&vendor_hex).ok_or_else(|| format!("unknown GPU vendor: {vendor_hex}"))?;

    let device_hex = read_sysfs_attr(pci_addr, "device")?;
    let (model, vram_mb) = gpu_model_lookup(vendor_name, &device_hex);

    Ok(GpuDevice {
        pci_address: pci_addr.to_string(),
        vendor: vendor_name.to_string(),
        model: model.to_string(),
        vram_mb,
        bound_at: timestamp_now(),
    })
}

fn cmd_bind(pci_addr: &str) -> Result<(), String> {
    let pci_addr = parse_pci_address(pci_addr)?;

    // Check device exists
    if !sysfs_path(&pci_addr, "vendor").exists() {
        return Err(format!("PCI device not found: {pci_addr}"));
    }

    // Already bound to vfio-pci? No-op.
    if let Some(driver) = current_driver(&pci_addr)? {
        if driver == "vfio-pci" {
            eprintln!("{pci_addr}: already bound to vfio-pci, skipping");
            return Ok(());
        }

        // Unbind from current driver
        let unbind_path = sysfs_path(&pci_addr, "driver/unbind");
        fs::write(&unbind_path, &pci_addr)
            .map_err(|e| format!("failed to unbind from {driver}: {e}"))?;
        eprintln!("{pci_addr}: unbound from {driver}");
    }

    // Load vfio-pci module
    let status = process::Command::new("modprobe")
        .arg("vfio-pci")
        .status()
        .map_err(|e| format!("failed to run modprobe vfio-pci: {e}"))?;
    if !status.success() {
        return Err("modprobe vfio-pci failed".into());
    }

    // Bind to vfio-pci
    let bind_path = PathBuf::from("/sys/bus/pci/drivers/vfio-pci/bind");
    if !bind_path.exists() {
        return Err("vfio-pci driver not available".into());
    }
    fs::write(&bind_path, &pci_addr).map_err(|e| format!("failed to bind to vfio-pci: {e}"))?;
    eprintln!("{pci_addr}: bound to vfio-pci");

    // Detect GPU metadata
    let gpu = detect_gpu(&pci_addr)?;
    eprintln!(
        "{pci_addr}: vendor={} model={} vram={}MB",
        gpu.vendor, gpu.model, gpu.vram_mb
    );

    // Update state file
    let mut devices = load_state();
    devices.retain(|d| d.pci_address != pci_addr);
    devices.push(gpu);
    save_state(&devices)?;

    eprintln!("state saved to {STATE_FILE}");
    Ok(())
}

fn cmd_unbind(pci_addr: &str) -> Result<(), String> {
    let pci_addr = parse_pci_address(pci_addr)?;

    // Check current driver
    let driver = current_driver(&pci_addr)?
        .ok_or_else(|| format!("PCI device not bound to any driver: {pci_addr}"))?;

    if driver != "vfio-pci" {
        eprintln!("{pci_addr}: not bound to vfio-pci (driver={driver}), skipping");
        return Ok(());
    }

    // Unbind from vfio-pci
    let unbind_path = sysfs_path(&pci_addr, "driver/unbind");
    fs::write(&unbind_path, &pci_addr)
        .map_err(|e| format!("failed to unbind from vfio-pci: {e}"))?;
    eprintln!("{pci_addr}: unbound from vfio-pci");

    // Detect vendor to load native driver
    let vendor_hex = read_sysfs_attr(&pci_addr, "vendor")?;
    let native_driver = match vendor_hex.as_str() {
        "0x10de" => "nvidia",
        "0x1002" => "amdgpu",
        _ => return Err(format!("unknown vendor: {vendor_hex}, cannot rebind")),
    };

    // Load native driver
    let status = process::Command::new("modprobe")
        .arg(native_driver)
        .status()
        .map_err(|e| format!("failed to run modprobe {native_driver}: {e}"))?;
    if !status.success() {
        return Err(format!("modprobe {native_driver} failed"));
    }

    // Rebind to native driver
    let bind_path = PathBuf::from(format!("/sys/bus/pci/drivers/{native_driver}/bind"));
    if bind_path.exists() {
        fs::write(&bind_path, &pci_addr)
            .map_err(|e| format!("failed to rebind to {native_driver}: {e}"))?;
        eprintln!("{pci_addr}: rebound to {native_driver}");
    } else {
        eprintln!("{pci_addr}: warning: {native_driver} bind path not found, manual rebinding may be needed");
    }

    // Remove from state file
    let mut devices = load_state();
    devices.retain(|d| d.pci_address != pci_addr);
    save_state(&devices)?;

    eprintln!("state saved to {STATE_FILE}");
    Ok(())
}

fn cmd_list() -> Result<(), String> {
    let devices = load_state();
    let json =
        serde_json::to_string_pretty(&devices).map_err(|e| format!("failed to serialize: {e}"))?;
    println!("{json}");
    Ok(())
}

fn main() {
    let args: Vec<String> = env::args().collect();
    if args.len() < 2 {
        usage();
    }

    let result = match args[1].as_str() {
        "bind" => {
            let device = args
                .windows(2)
                .find(|w| w[0] == "--device")
                .map(|w| w[1].as_str());
            match device {
                Some(addr) => cmd_bind(addr),
                None => {
                    eprintln!("bind requires --device <PCI_ADDRESS>");
                    usage();
                }
            }
        }
        "unbind" => {
            let device = args
                .windows(2)
                .find(|w| w[0] == "--device")
                .map(|w| w[1].as_str());
            match device {
                Some(addr) => cmd_unbind(addr),
                None => {
                    eprintln!("unbind requires --device <PCI_ADDRESS>");
                    usage();
                }
            }
        }
        "list" => cmd_list(),
        other => {
            eprintln!("unknown command: {other}");
            usage();
        }
    };

    if let Err(e) = result {
        eprintln!("error: {e}");
        process::exit(1);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pci_address_valid() {
        assert!(parse_pci_address("0000:01:00.0").is_ok());
        assert!(parse_pci_address("0000:73:00.1").is_ok());
        assert!(parse_pci_address("73:00.0").is_ok());
    }

    #[test]
    fn pci_address_short_normalizes() {
        let addr = parse_pci_address("01:00.0").unwrap();
        assert_eq!(addr, "0000:01:00.0");
    }

    #[test]
    fn pci_address_invalid() {
        assert!(parse_pci_address("invalid").is_err());
        assert!(parse_pci_address("0000:01:00").is_err());
        assert!(parse_pci_address("XXXX:01:00.0").is_err());
    }

    #[test]
    fn pci_address_normalizes_lowercase() {
        let addr = parse_pci_address("0000:01:00.0").unwrap();
        assert_eq!(addr, "0000:01:00.0");
    }

    #[test]
    fn vendor_name_mapping() {
        assert_eq!(pci_vendor_name("0x10de"), Some("nvidia"));
        assert_eq!(pci_vendor_name("0x1002"), Some("amd"));
        assert_eq!(pci_vendor_name("0x8086"), None);
    }

    #[test]
    fn gpu_model_known_nvidia() {
        let (model, vram) = gpu_model_lookup("nvidia", "0x1b30");
        assert_eq!(model, "H100-80GB");
        assert_eq!(vram, 81920);
    }

    #[test]
    fn gpu_model_known_amd() {
        let (model, vram) = gpu_model_lookup("amd", "0x740c");
        assert_eq!(model, "MI300X-192GB");
        assert_eq!(vram, 196608);
    }

    #[test]
    fn gpu_model_unknown() {
        let (model, vram) = gpu_model_lookup("nvidia", "0xffff");
        assert_eq!(model, "unknown");
        assert_eq!(vram, 0);
    }

    #[test]
    fn state_round_trip() {
        let devices = vec![GpuDevice {
            pci_address: "0000:01:00.0".into(),
            vendor: "nvidia".into(),
            model: "H100-80GB".into(),
            vram_mb: 81920,
            bound_at: "1234567890".into(),
        }];
        let json = serde_json::to_string_pretty(&devices).unwrap();
        let parsed: Vec<GpuDevice> = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed.len(), 1);
        assert_eq!(parsed[0].pci_address, "0000:01:00.0");
        assert_eq!(parsed[0].vendor, "nvidia");
        assert_eq!(parsed[0].model, "H100-80GB");
        assert_eq!(parsed[0].vram_mb, 81920);
    }

    #[test]
    fn state_empty_round_trip() {
        let devices: Vec<GpuDevice> = vec![];
        let json = serde_json::to_string_pretty(&devices).unwrap();
        let parsed: Vec<GpuDevice> = serde_json::from_str(&json).unwrap();
        assert!(parsed.is_empty());
    }
}
