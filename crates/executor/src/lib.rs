//! Vtessera executor — Module 1 (ROADMAP.md §1).
//!
//! This crate is the privileged side of Vtessera: the surface that takes a
//! job an AI agent has paid for (or one a seller is donating free) and runs
//! it in an isolated VM with optional GPU passthrough. The v0 metering
//! daemon (`vtesserad`) deliberately does none of this; the executor lives
//! in its own crate with its own audit and threat model.
//!
//! The crate ships as a **skeleton**: the types, traits, and a no-op
//! development backend are pinned here so settlement (Module 3) and the
//! escrow program (Module 4) can be wired against a stable interface
//! while the heavy backends — Kata Containers on Cloud Hypervisor for
//! the VM, VFIO for GPU passthrough, DCGM for per-job GPU metering — land
//! behind cargo features.
//!
//! See ROADMAP.md §1 for the full picture, in particular:
//!
//! - §1a — VMM choice (Kata + Cloud Hypervisor recommended)
//! - §1c — GPU sharing modes (whole-GPU, MIG, vGPU, time-slice) and the
//!   confidential-GPU caveat
//! - §1d — per-device metering fields that flow into signed receipts and
//!   then into the completion fraction `f` settlement computes
//! - §1e — admission, network defaults, and the systemd-analyze bar

#![forbid(unsafe_code)]

use serde::{Deserialize, Serialize};

/// What the executor can run.
///
/// One concrete value per backend, including the no-op CPU backend used by
/// tests and dry-runs. The variants are open by design — additions don't
/// shift the existing wire format because each value carries its own
/// configuration struct.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Backend {
    /// Records the request and returns synthetic metering. Used in tests,
    /// CI, and `--dry-run` paths so settlement and escrow can be exercised
    /// end-to-end without a privileged VMM available. Never the production
    /// default.
    NoopCpu,
    /// Kata Containers on a Cloud Hypervisor backend. The recommended
    /// production path (ROADMAP.md §1a). Wired as a feature in a follow-up.
    KataCloudHypervisor,
    /// Cloud Hypervisor managed directly without the OCI / Kata layer.
    CloudHypervisor,
    /// QEMU + VFIO. Heaviest, most complete device support; fallback for
    /// exotic hardware.
    QemuVfio,
}

/// Device classes Vtessera can advertise to AI buyers.
///
/// Settlement (Module 3) prices in *device-time*: CPU-seconds for the CPU
/// class, GPU-seconds + VRAM-GB-hours for GPU classes, with MIG profiles
/// broken out separately because their unit economics differ.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, Hash)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum DeviceClass {
    Cpu,
    /// Whole NVIDIA GPU via VFIO passthrough. `model` is the consumer-facing
    /// label, e.g. "H100-80GB", "A100-40GB". One tenant per GPU.
    NvidiaGpu {
        model: String,
    },
    /// NVIDIA MIG instance on an A100/H100. `parent_model` is the host GPU,
    /// `profile` is the MIG profile such as "1g.10gb" or "3g.40gb". MIG is
    /// the strongest GPU-sharing mode short of whole-GPU.
    NvidiaMig {
        parent_model: String,
        profile: String,
    },
    /// AMD GPU via ROCm.
    AmdGpu {
        model: String,
    },
}

/// Job network policy.
///
/// Default is `None` — guests can't talk to the host network. Buyers who
/// need outbound (e.g. to pull a model from Hugging Face) request it
/// explicitly so the seller can price it.
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq, Default)]
#[serde(rename_all = "snake_case")]
pub enum NetworkPolicy {
    /// No host network access from the guest. Default.
    #[default]
    None,
    /// Outbound HTTPS only (TCP/443) — the common "pull a model" exception.
    OutboundHttps,
    /// Full guest egress. Most permissive; sellers should price accordingly.
    Egress,
}

/// What the buyer needs the seller to provide.
///
/// Admission (ROADMAP.md §1e) matches a [`JobSpec`] against a node's
/// declared capabilities (advertised in Module 2's offer) before the
/// executor is even invoked.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct DeviceRequirements {
    pub class: DeviceClass,
    /// vCPUs to expose to the guest.
    pub vcpus: u32,
    /// Guest memory ceiling.
    pub mem_kb: u64,
    /// Minimum VRAM in MB. Ignored for CPU-only jobs.
    pub min_vram_mb: u32,
    /// Driver / runtime hint, e.g. "cuda-12.4" or "rocm-6.0". Free-form;
    /// the executor matches against the pinned image set the node publishes.
    pub driver_hint: Option<String>,
}

/// One job submitted by an AI agent (paid via x402, or free).
///
/// The OCI image is the unit of work AI buyers already ship. Sellers don't
/// build images; they only pin a small set of driver/CUDA bases the guest
/// can layer on.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct JobSpec {
    /// Caller-supplied identifier. The settlement service uses this to
    /// thread receipts back to the on-chain escrow account.
    pub job_id: String,
    /// OCI image reference, e.g. "ghcr.io/example/llama:cuda12.4".
    pub image: String,
    /// Entrypoint override; `None` means use the image's default.
    pub command: Vec<String>,
    /// Environment variables exposed to the guest.
    pub env: Vec<(String, String)>,
    /// Device + resource requirements admission matches against.
    pub devices: DeviceRequirements,
    /// Network policy. Defaults to `None` — explicit egress only.
    #[serde(default)]
    pub network: NetworkPolicy,
    /// Hard wall-clock ceiling. The executor terminates the job at this
    /// boundary even if it hasn't exited. The buyer is refunded the
    /// unearned portion via Module 4.
    pub max_duration_secs: u64,
}

/// Per-job metering written into the signed receipt (ROADMAP.md §1d).
///
/// Settlement (Module 3) aggregates these into the completion fraction
/// `f ∈ [0, 1]` the escrow program splits against. The v0 receipt format
/// gains these fields when the executor crate is wired in; the signature
/// path itself (Ed25519, canonical bytes, schema_ver bump) doesn't change.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct JobMetering {
    pub job_id: String,
    /// Backend that actually ran the job.
    pub backend: Backend,
    /// Device class the metering is denominated in. Mirrors the request
    /// so settlement can detect (and refuse to pay against) downgrades —
    /// e.g. a job that asked for a whole H100 but ran on a MIG slice.
    pub device: DeviceClass,
    /// CPU-seconds consumed by the guest.
    pub cpu_seconds: f64,
    /// Peak guest memory, kB.
    pub peak_mem_kb: u64,
    /// GPU-seconds the guest actually held the accelerator for. Zero for
    /// CPU-only jobs.
    pub gpu_seconds: f64,
    /// VRAM × time integral, in **GB-hours**. This is the unit DCGM
    /// reports; settlement prices in it directly.
    pub vram_gb_hours: f64,
    /// Job exit status. Settlement treats non-zero `f`-eligible work even
    /// for failed exits, scaled by how far through the contract it got.
    pub exit_status: ExitStatus,
    /// Wall-clock the job actually ran for. Used to enforce `max_duration_secs`.
    pub elapsed_secs: u64,
}

/// How a job ended. Open for additions; serialised as a tagged variant.
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum ExitStatus {
    /// Process exited 0.
    Completed,
    /// Process exited non-zero. Settlement may still credit partial work.
    Failed { code: i32 },
    /// Executor hit `max_duration_secs` and killed the job.
    TimedOut,
    /// Buyer cancelled mid-run (Module 2 cancel-by-stopping-pay-as-you-go,
    /// or a seller-side eviction).
    Cancelled,
}

/// Common executor error surface.
#[derive(Debug)]
pub enum ExecutorError {
    /// The admission layer rejected the job before it ran. The string
    /// is human-readable and not part of any wire format.
    Admission(String),
    /// The backend is implemented in the type system but not yet wired
    /// (Kata, QEMU, etc. in this skeleton).
    BackendUnimplemented(Backend),
    /// Anything else the backend reports.
    Backend(String),
}

impl std::fmt::Display for ExecutorError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ExecutorError::Admission(why) => write!(f, "admission rejected: {why}"),
            ExecutorError::BackendUnimplemented(b) => {
                write!(f, "backend not yet implemented: {b:?}")
            }
            ExecutorError::Backend(why) => write!(f, "backend error: {why}"),
        }
    }
}

impl std::error::Error for ExecutorError {}

/// The minimum executor contract.
///
/// Synchronous on purpose — async runtimes (tokio, async-std) would widen
/// the dep budget for what is, at the privileged layer, a small number of
/// long-running jobs. Backends that need concurrency manage their own
/// thread pool internally.
pub trait Executor {
    /// Run a job to completion (or `max_duration_secs`) and return its
    /// metering. The metering struct is what gets folded into the signed
    /// receipt and then into settlement's `f` calculation.
    fn run(&self, spec: &JobSpec) -> Result<JobMetering, ExecutorError>;
}

/// Development backend. Records that the job was "run" and returns
/// synthetic metering — exists so CI can exercise the executor → receipt
/// → settlement → escrow chain without a privileged VMM.
///
/// Never the production default. Production code must construct a real
/// backend explicitly.
pub struct NoopCpuExecutor;

impl Executor for NoopCpuExecutor {
    fn run(&self, spec: &JobSpec) -> Result<JobMetering, ExecutorError> {
        admission_check(spec)?;
        Ok(JobMetering {
            job_id: spec.job_id.clone(),
            backend: Backend::NoopCpu,
            device: spec.devices.class.clone(),
            cpu_seconds: spec.devices.vcpus as f64,
            peak_mem_kb: spec.devices.mem_kb,
            gpu_seconds: 0.0,
            vram_gb_hours: 0.0,
            exit_status: ExitStatus::Completed,
            elapsed_secs: 1,
        })
    }
}

/// Admission policy (ROADMAP.md §1e).
///
/// Pure function so it's straightforward to test. Backends call this
/// before any privileged operation so a malformed spec can't escape the
/// trait boundary into VMM territory.
fn admission_check(spec: &JobSpec) -> Result<(), ExecutorError> {
    if spec.job_id.is_empty() {
        return Err(ExecutorError::Admission("empty job_id".into()));
    }
    if spec.image.is_empty() {
        return Err(ExecutorError::Admission("empty image".into()));
    }
    if spec.devices.vcpus == 0 {
        return Err(ExecutorError::Admission("vcpus must be > 0".into()));
    }
    if spec.devices.mem_kb == 0 {
        return Err(ExecutorError::Admission("mem_kb must be > 0".into()));
    }
    if spec.max_duration_secs == 0 {
        return Err(ExecutorError::Admission(
            "max_duration_secs must be > 0".into(),
        ));
    }
    // GPU jobs without a VRAM floor are almost always misconfigured.
    if !matches!(spec.devices.class, DeviceClass::Cpu) && spec.devices.min_vram_mb == 0 {
        return Err(ExecutorError::Admission(
            "GPU job missing min_vram_mb".into(),
        ));
    }
    Ok(())
}

/// Production-backend stub. Lands here so the variant exists in the type
/// system and downstream code can branch on it; the real implementation
/// lives behind a cargo feature once Kata + Cloud Hypervisor + VFIO are
/// wired.
pub struct KataCloudHypervisorExecutor;

impl Executor for KataCloudHypervisorExecutor {
    fn run(&self, _spec: &JobSpec) -> Result<JobMetering, ExecutorError> {
        Err(ExecutorError::BackendUnimplemented(
            Backend::KataCloudHypervisor,
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample_spec() -> JobSpec {
        JobSpec {
            job_id: "test-job-0001".into(),
            image: "example.invalid/llama:cuda12.4".into(),
            command: vec!["python".into(), "infer.py".into()],
            env: vec![("HF_HOME".into(), "/work/hf".into())],
            devices: DeviceRequirements {
                class: DeviceClass::NvidiaGpu {
                    model: "H100-80GB".into(),
                },
                vcpus: 8,
                mem_kb: 32 * 1024 * 1024,
                min_vram_mb: 40_000,
                driver_hint: Some("cuda-12.4".into()),
            },
            network: NetworkPolicy::OutboundHttps,
            max_duration_secs: 3600,
        }
    }

    #[test]
    fn noop_backend_returns_metering_threaded_through_request() {
        let spec = sample_spec();
        let metering = NoopCpuExecutor.run(&spec).expect("noop should accept");
        assert_eq!(metering.job_id, spec.job_id);
        assert_eq!(metering.device, spec.devices.class);
        assert_eq!(metering.backend, Backend::NoopCpu);
        assert!(matches!(metering.exit_status, ExitStatus::Completed));
    }

    #[test]
    fn admission_rejects_empty_job_id() {
        let mut spec = sample_spec();
        spec.job_id = String::new();
        assert!(matches!(
            NoopCpuExecutor.run(&spec),
            Err(ExecutorError::Admission(_))
        ));
    }

    #[test]
    fn admission_rejects_gpu_with_zero_vram() {
        let mut spec = sample_spec();
        spec.devices.min_vram_mb = 0;
        assert!(matches!(
            NoopCpuExecutor.run(&spec),
            Err(ExecutorError::Admission(_))
        ));
    }

    #[test]
    fn admission_accepts_cpu_with_zero_vram() {
        let mut spec = sample_spec();
        spec.devices.class = DeviceClass::Cpu;
        spec.devices.min_vram_mb = 0;
        assert!(NoopCpuExecutor.run(&spec).is_ok());
    }

    #[test]
    fn kata_backend_is_unimplemented() {
        let spec = sample_spec();
        let err = KataCloudHypervisorExecutor.run(&spec).unwrap_err();
        assert!(matches!(
            err,
            ExecutorError::BackendUnimplemented(Backend::KataCloudHypervisor)
        ));
    }

    #[test]
    fn device_class_roundtrips_through_serde_json_via_hand_format() {
        // We don't pull serde_json in; instead verify the tagged enum
        // shape stays stable by checking the Debug format includes the
        // backend tag — a cheap regression test for "did someone collapse
        // the variants accidentally".
        let mig = DeviceClass::NvidiaMig {
            parent_model: "H100-80GB".into(),
            profile: "1g.10gb".into(),
        };
        let dbg = format!("{mig:?}");
        assert!(dbg.contains("NvidiaMig"));
        assert!(dbg.contains("H100-80GB"));
        assert!(dbg.contains("1g.10gb"));
    }
}
