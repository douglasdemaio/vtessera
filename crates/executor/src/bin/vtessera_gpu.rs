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
    /// MIG profiles this GPU supports (e.g. "1g.10gb", "3g.40gb").
    /// Empty for non-MIG GPUs or GPUs not yet queried.
    #[serde(default)]
    pub mig_profiles: Vec<String>,
    /// Active MIG instances on this GPU.
    #[serde(default)]
    pub mig_instances: Vec<MigInstance>,
}

/// A single MIG instance created on a parent GPU.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize, PartialEq, Eq)]
pub struct MigInstance {
    /// NVIDIA-assigned UUID for this MIG instance.
    pub uuid: String,
    /// MIG profile (e.g. "1g.10gb", "3g.40gb").
    pub profile: String,
    /// VFIO PCI address of the MIG instance (after bind to vfio-pci).
    pub pci_address: String,
    /// VRAM in MB for this MIG slice.
    pub vram_mb: u32,
}

fn usage() -> ! {
    eprintln!("Usage: vtessera-gpu <COMMAND> [OPTIONS]");
    eprintln!();
    eprintln!("Commands:");
    eprintln!("  bind      --device <ADDR>        Bind GPU to vfio-pci");
    eprintln!("  unbind    --device <ADDR>        Unbind GPU from vfio-pci");
    eprintln!("  list                             List VFIO-bound GPUs");
    eprintln!("  mig-list  --device <ADDR>        List MIG profiles and instances");
    eprintln!("  mig-create --device <ADDR> --profile <PROFILE>");
    eprintln!("                                  Create a MIG instance and bind to vfio-pci");
    eprintln!("  mig-destroy --device <ADDR> --uuid <UUID>");
    eprintln!("                                  Destroy a MIG instance");
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
        mig_profiles: Vec::new(),
        mig_instances: Vec::new(),
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

/// Detect available MIG profiles for a GPU by parsing nvidia-smi output.
/// Falls back to sysfs if nvidia-smi is not available.
fn detect_mig_profiles(pci_addr: &str) -> Result<Vec<String>, String> {
    let mut profiles = Vec::new();

    // Try nvidia-smi mig --list first (most reliable)
    let output = process::Command::new("nvidia-smi")
        .args(["mig", "--list"])
        .output();
    if let Ok(o) = output {
        if o.status.success() {
            let stdout = String::from_utf8_lossy(&o.stdout);
            for line in stdout.lines() {
                // nvidia-smi mig --list output has lines like:
                // "   1g.10gb       1        10240 MiB"
                let trimmed = line.trim();
                if trimmed.starts_with("1g.")
                    || trimmed.starts_with("2g.")
                    || trimmed.starts_with("3g.")
                    || trimmed.starts_with("4g.")
                    || trimmed.starts_with("7g.")
                {
                    if let Some(profile) = trimmed.split_whitespace().next() {
                        profiles.push(profile.to_string());
                    }
                }
            }
            if !profiles.is_empty() {
                return Ok(profiles);
            }
        }
    }

    // Fallback: check sysfs for MIG manager
    let mig_dir = sysfs_path(pci_addr, "mig_manager");
    if mig_dir.exists() {
        if let Ok(entries) = fs::read_dir(&mig_dir) {
            for entry in entries.flatten() {
                let name = entry.file_name().to_string_lossy().to_string();
                // sysfs MIG entries are typically numeric instance IDs
                if name.chars().all(|c| c.is_ascii_digit()) {
                    let profile_path = entry.path().join("gpu_instance_profile");
                    if let Ok(profile) = fs::read_to_string(&profile_path) {
                        profiles.push(profile.trim().to_string());
                    }
                }
            }
        }
    }

    Ok(profiles)
}

/// Detect active MIG instances on a GPU.
fn detect_mig_instances(pci_addr: &str) -> Result<Vec<MigInstance>, String> {
    let mut instances = Vec::new();

    // Try nvidia-smi mig --list-devices
    let output = process::Command::new("nvidia-smi")
        .args(["mig", "--list-devices"])
        .output();
    if let Ok(o) = output {
        if o.status.success() {
            let stdout = String::from_utf8_lossy(&o.stdout);
            for line in stdout.lines() {
                let trimmed = line.trim();
                // Output format: "GPU 0: H100-80GB (UUID: GPU-xxxxx)"
                // followed by MIG instances
                if trimmed.contains("MIG") || trimmed.contains("Instance") {
                    // Parse MIG instance lines
                    if let Some(uuid_start) = trimmed.find("UUID:") {
                        let uuid_part = &trimmed[uuid_start + 5..];
                        if let Some(uuid_end) = uuid_part.find(')') {
                            let uuid = uuid_part[..uuid_end].trim().to_string();
                            // Try to determine profile from the line
                            let profile = if trimmed.contains("1g.") {
                                extract_profile(trimmed, "1g.")
                            } else if trimmed.contains("2g.") {
                                extract_profile(trimmed, "2g.")
                            } else if trimmed.contains("3g.") {
                                extract_profile(trimmed, "3g.")
                            } else if trimmed.contains("4g.") {
                                extract_profile(trimmed, "4g.")
                            } else if trimmed.contains("7g.") {
                                extract_profile(trimmed, "7g.")
                            } else {
                                "unknown".to_string()
                            };
                            instances.push(MigInstance {
                                uuid,
                                profile,
                                pci_address: String::new(), // Filled after vfio-bind
                                vram_mb: 0,                 // Filled from profile lookup
                            });
                        }
                    }
                }
            }
        }
    }

    // Fallback: check sysfs
    if instances.is_empty() {
        let mig_dir = sysfs_path(pci_addr, "mig_manager");
        if mig_dir.exists() {
            if let Ok(entries) = fs::read_dir(&mig_dir) {
                for entry in entries.flatten() {
                    let name = entry.file_name().to_string_lossy().to_string();
                    if name.chars().all(|c| c.is_ascii_digit()) {
                        let profile_path = entry.path().join("gpu_instance_profile");
                        let profile = fs::read_to_string(&profile_path)
                            .map(|s| s.trim().to_string())
                            .unwrap_or_else(|_| "unknown".to_string());
                        instances.push(MigInstance {
                            uuid: format!("mig-{pci_addr}-{name}"),
                            profile,
                            pci_address: String::new(),
                            vram_mb: 0,
                        });
                    }
                }
            }
        }
    }

    Ok(instances)
}

fn extract_profile(line: &str, prefix: &str) -> String {
    for word in line.split_whitespace() {
        if word.starts_with(prefix) {
            return word.to_string();
        }
    }
    format!("{prefix}unknown")
}

/// Map a MIG profile to its approximate VRAM in MB.
fn mig_profile_vram_mb(profile: &str) -> u32 {
    if profile.contains("1g.") {
        // 1g profiles: 10GB, 20GB, 40GB
        if profile.contains("40gb") {
            40960
        } else if profile.contains("20gb") {
            20480
        } else {
            10240
        }
    } else if profile.contains("2g.") {
        // 2g profiles: 20GB, 40GB
        if profile.contains("40gb") {
            40960
        } else {
            20480
        }
    } else if profile.contains("3g.") {
        // 3g profiles: 40GB, 80GB
        if profile.contains("80gb") {
            81920
        } else {
            40960
        }
    } else if profile.contains("4g.") {
        // 4g profiles: 40GB, 80GB
        if profile.contains("80gb") {
            81920
        } else {
            40960
        }
    } else if profile.contains("7g.") {
        // 7g is full GPU
        81920
    } else {
        0
    }
}

fn cmd_mig_list(pci_addr: &str) -> Result<(), String> {
    let pci_addr = parse_pci_address(pci_addr)?;

    // Check device exists
    if !sysfs_path(&pci_addr, "vendor").exists() {
        return Err(format!("PCI device not found: {pci_addr}"));
    }

    let profiles = detect_mig_profiles(&pci_addr)?;
    let instances = detect_mig_instances(&pci_addr)?;

    let output = serde_json::json!({
        "pci_address": pci_addr,
        "available_profiles": profiles,
        "active_instances": instances,
    });
    println!(
        "{}",
        serde_json::to_string_pretty(&output).map_err(|e| format!("failed to serialize: {e}"))?
    );
    Ok(())
}

fn cmd_mig_create(pci_addr: &str, profile: &str) -> Result<(), String> {
    let pci_addr = parse_pci_address(pci_addr)?;

    // Check device exists
    if !sysfs_path(&pci_addr, "vendor").exists() {
        return Err(format!("PCI device not found: {pci_addr}"));
    }

    // Validate profile is available
    let available = detect_mig_profiles(&pci_addr)?;
    if !available.contains(&profile.to_string()) {
        return Err(format!(
            "MIG profile {profile} not available on {pci_addr}. Available: {:?}",
            available
        ));
    }

    // Create MIG instance via nvidia-smi
    let output = process::Command::new("nvidia-smi")
        .args(["mig", "--create-gpu-instance", profile, "--gpu", &pci_addr])
        .output()
        .map_err(|e| format!("failed to run nvidia-smi: {e}"))?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        return Err(format!("nvidia-smi mig create failed: {stderr}"));
    }

    // Discover the newly created MIG instance
    let instances = detect_mig_instances(&pci_addr)?;
    let new_instance = instances
        .iter()
        .find(|i| i.profile == profile)
        .ok_or_else(|| "MIG instance created but not found in detection".to_string())?;

    // Find the MIG instance's PCI address from sysfs
    let mig_dir = sysfs_path(&pci_addr, "mig_manager");
    let mut instance_pci = String::new();
    if mig_dir.exists() {
        if let Ok(entries) = fs::read_dir(&mig_dir) {
            for entry in entries.flatten() {
                let name = entry.file_name().to_string_lossy().to_string();
                if name.chars().all(|c| c.is_ascii_digit()) {
                    // Check if this is the newly created instance
                    let uuid_path = entry.path().join("gpu_instance_uuid");
                    if let Ok(uuid) = fs::read_to_string(&uuid_path) {
                        if uuid.trim() == new_instance.uuid {
                            // Look for the PCI device in sysfs
                            let pci_dir = entry.path().join("pci");
                            if pci_dir.exists() {
                                if let Ok(pci_entries) = fs::read_dir(&pci_dir) {
                                    for pci_entry in pci_entries.flatten() {
                                        let pci_name =
                                            pci_entry.file_name().to_string_lossy().to_string();
                                        if parse_pci_address(&pci_name).is_ok() {
                                            instance_pci = pci_name;
                                            break;
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    if instance_pci.is_empty() {
        eprintln!(
            "warning: could not determine MIG instance PCI address; it may need manual vfio-bind"
        );
        instance_pci = format!("mig-{}-{}", pci_addr, new_instance.uuid);
    }

    // Unbind MIG instance from nvidia driver and bind to vfio-pci
    let unbind_path = sysfs_path(&instance_pci, "driver/unbind");
    if unbind_path.exists() {
        fs::write(&unbind_path, &instance_pci)
            .map_err(|e| format!("failed to unbind MIG instance: {e}"))?;
    }

    // Load vfio-pci
    let status = process::Command::new("modprobe")
        .arg("vfio-pci")
        .status()
        .map_err(|e| format!("failed to run modprobe vfio-pci: {e}"))?;
    if !status.success() {
        return Err("modprobe vfio-pci failed".into());
    }

    // Bind to vfio-pci
    let bind_path = PathBuf::from("/sys/bus/pci/drivers/vfio-pci/bind");
    if bind_path.exists() {
        fs::write(&bind_path, &instance_pci)
            .map_err(|e| format!("failed to bind MIG instance to vfio-pci: {e}"))?;
    }

    // Update state file
    let mut devices = load_state();
    if let Some(gpu) = devices.iter_mut().find(|d| d.pci_address == pci_addr) {
        let mig = MigInstance {
            uuid: new_instance.uuid.clone(),
            profile: profile.to_string(),
            pci_address: instance_pci.clone(),
            vram_mb: mig_profile_vram_mb(profile),
        };
        // Don't add duplicate
        if !gpu.mig_instances.iter().any(|i| i.uuid == mig.uuid) {
            gpu.mig_instances.push(mig);
        }
    } else {
        // GPU not in state file yet — detect and add it
        let mut gpu = detect_gpu(&pci_addr)?;
        gpu.mig_profiles = detect_mig_profiles(&pci_addr).unwrap_or_default();
        gpu.mig_instances.push(MigInstance {
            uuid: new_instance.uuid.clone(),
            profile: profile.to_string(),
            pci_address: instance_pci.clone(),
            vram_mb: mig_profile_vram_mb(profile),
        });
        devices.push(gpu);
    }
    save_state(&devices)?;

    eprintln!(
        "MIG instance {} created on {pci_addr} (profile={profile}, pci={instance_pci})",
        new_instance.uuid
    );
    eprintln!("state saved to {STATE_FILE}");
    Ok(())
}

fn cmd_mig_destroy(pci_addr: &str, uuid: &str) -> Result<(), String> {
    let pci_addr = parse_pci_address(pci_addr)?;

    // Find the MIG instance in state
    let mut devices = load_state();
    let gpu = devices
        .iter()
        .find(|d| d.pci_address == pci_addr)
        .ok_or_else(|| format!("GPU {pci_addr} not found in state file"))?;

    let instance = gpu
        .mig_instances
        .iter()
        .find(|i| i.uuid == uuid)
        .ok_or_else(|| format!("MIG instance {uuid} not found on {pci_addr}"))?;

    let instance_pci = instance.pci_address.clone();

    // Unbind from vfio-pci if bound
    if !instance_pci.is_empty() && !instance_pci.starts_with("mig-") {
        let driver = current_driver(&instance_pci)?;
        if driver.as_deref() == Some("vfio-pci") {
            let unbind_path = sysfs_path(&instance_pci, "driver/unbind");
            fs::write(&unbind_path, &instance_pci)
                .map_err(|e| format!("failed to unbind MIG instance from vfio-pci: {e}"))?;
            eprintln!("{instance_pci}: unbound from vfio-pci");
        }
    }

    // Destroy via nvidia-smi
    let output = process::Command::new("nvidia-smi")
        .args([
            "mig",
            "--destroy-gpu-instance",
            "--instance",
            uuid,
            "--gpu",
            &pci_addr,
        ])
        .output()
        .map_err(|e| format!("failed to run nvidia-smi: {e}"))?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        return Err(format!("nvidia-smi mig destroy failed: {stderr}"));
    }

    // Remove from state file
    if let Some(gpu) = devices.iter_mut().find(|d| d.pci_address == pci_addr) {
        gpu.mig_instances.retain(|i| i.uuid != uuid);
    }
    save_state(&devices)?;

    eprintln!("MIG instance {uuid} destroyed on {pci_addr}");
    eprintln!("state saved to {STATE_FILE}");
    Ok(())
}

fn find_arg(args: &[String], flag: &str) -> Option<String> {
    args.windows(2).find(|w| w[0] == flag).map(|w| w[1].clone())
}

fn main() {
    let args: Vec<String> = env::args().collect();
    if args.len() < 2 {
        usage();
    }

    let result = match args[1].as_str() {
        "bind" => {
            let device = find_arg(&args, "--device");
            match device {
                Some(addr) => cmd_bind(&addr),
                None => {
                    eprintln!("bind requires --device <PCI_ADDRESS>");
                    usage();
                }
            }
        }
        "unbind" => {
            let device = find_arg(&args, "--device");
            match device {
                Some(addr) => cmd_unbind(&addr),
                None => {
                    eprintln!("unbind requires --device <PCI_ADDRESS>");
                    usage();
                }
            }
        }
        "list" => cmd_list(),
        "mig-list" => {
            let device = find_arg(&args, "--device");
            match device {
                Some(addr) => cmd_mig_list(&addr),
                None => {
                    eprintln!("mig-list requires --device <PCI_ADDRESS>");
                    usage();
                }
            }
        }
        "mig-create" => {
            let device = find_arg(&args, "--device");
            let profile = find_arg(&args, "--profile");
            match (device, profile) {
                (Some(addr), Some(profile)) => cmd_mig_create(&addr, &profile),
                _ => {
                    eprintln!("mig-create requires --device <PCI_ADDRESS> --profile <PROFILE>");
                    usage();
                }
            }
        }
        "mig-destroy" => {
            let device = find_arg(&args, "--device");
            let uuid = find_arg(&args, "--uuid");
            match (device, uuid) {
                (Some(addr), Some(uuid)) => cmd_mig_destroy(&addr, &uuid),
                _ => {
                    eprintln!("mig-destroy requires --device <PCI_ADDRESS> --uuid <UUID>");
                    usage();
                }
            }
        }
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
            mig_profiles: vec!["1g.10gb".into(), "3g.40gb".into()],
            mig_instances: vec![MigInstance {
                uuid: "GPU-abc-123".into(),
                profile: "1g.10gb".into(),
                pci_address: "0000:01:00.1".into(),
                vram_mb: 10240,
            }],
        }];
        let json = serde_json::to_string_pretty(&devices).unwrap();
        let parsed: Vec<GpuDevice> = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed.len(), 1);
        assert_eq!(parsed[0].pci_address, "0000:01:00.0");
        assert_eq!(parsed[0].vendor, "nvidia");
        assert_eq!(parsed[0].model, "H100-80GB");
        assert_eq!(parsed[0].vram_mb, 81920);
        assert_eq!(parsed[0].mig_profiles, vec!["1g.10gb", "3g.40gb"]);
        assert_eq!(parsed[0].mig_instances.len(), 1);
        assert_eq!(parsed[0].mig_instances[0].uuid, "GPU-abc-123");
        assert_eq!(parsed[0].mig_instances[0].profile, "1g.10gb");
    }

    #[test]
    fn state_empty_round_trip() {
        let devices: Vec<GpuDevice> = vec![];
        let json = serde_json::to_string_pretty(&devices).unwrap();
        let parsed: Vec<GpuDevice> = serde_json::from_str(&json).unwrap();
        assert!(parsed.is_empty());
    }

    #[test]
    fn mig_instance_round_trip() {
        let instance = MigInstance {
            uuid: "GPU-abc-123".into(),
            profile: "1g.10gb".into(),
            pci_address: "0000:01:00.1".into(),
            vram_mb: 10240,
        };
        let json = serde_json::to_string_pretty(&instance).unwrap();
        let parsed: MigInstance = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed.uuid, "GPU-abc-123");
        assert_eq!(parsed.profile, "1g.10gb");
        assert_eq!(parsed.pci_address, "0000:01:00.1");
        assert_eq!(parsed.vram_mb, 10240);
    }

    #[test]
    fn mig_instance_empty_state_compatible() {
        // Old state files without mig_profiles/mig_instances should parse fine
        let json = r#"[
            {
                "pci_address": "0000:01:00.0",
                "vendor": "nvidia",
                "model": "H100-80GB",
                "vram_mb": 81920,
                "bound_at": "1234567890"
            }
        ]"#;
        let parsed: Vec<GpuDevice> = serde_json::from_str(json).unwrap();
        assert_eq!(parsed.len(), 1);
        assert!(parsed[0].mig_profiles.is_empty());
        assert!(parsed[0].mig_instances.is_empty());
    }

    #[test]
    fn mig_profile_vram() {
        assert_eq!(mig_profile_vram_mb("1g.10gb"), 10240);
        assert_eq!(mig_profile_vram_mb("1g.20gb"), 20480);
        assert_eq!(mig_profile_vram_mb("3g.40gb"), 40960);
        assert_eq!(mig_profile_vram_mb("7g.80gb"), 81920);
        assert_eq!(mig_profile_vram_mb("unknown"), 0);
    }
}
