use std::fs;
use std::path::PathBuf;
use std::time::{Duration, Instant};

use crate::cloud_hypervisor::{
    apply_host_net_policy, parse_metering, remove_host_net_policy, select_gpu, write_manifest,
    CloudHypervisorConfig,
};
use crate::containerd::{ContainerConfig, ContainerdClient, PodConfig, VolumeMount};
use crate::{
    Backend, DeviceClass, Executor, ExecutorError, ExitStatus, JobMetering, JobSpec, NetworkPolicy,
};

#[derive(Debug, Clone)]
pub struct KataConfig {
    pub containerd_socket: PathBuf,
    pub kata_runtime: String,
    pub workdir: PathBuf,
    pub sidecar_image: String,
    pub keep_jobs: bool,
    pub vfio_devices: Vec<String>,
    pub gpu_helper: PathBuf,
    pub gpu_time_slice: bool,
    pub gpu_meter_poll_interval_secs: u64,
    pub net_enforcement: String,
    pub image_pull_policy: String,
}

impl Default for KataConfig {
    fn default() -> Self {
        Self {
            containerd_socket: PathBuf::from("/run/containerd/containerd.sock"),
            kata_runtime: "kata".into(),
            workdir: PathBuf::from("/var/lib/vtessera/jobs"),
            sidecar_image: "ghcr.io/vtessera/metering-sidecar:latest".into(),
            keep_jobs: false,
            vfio_devices: Vec::new(),
            gpu_helper: PathBuf::from("/usr/bin/vtessera-gpu"),
            gpu_time_slice: false,
            gpu_meter_poll_interval_secs: 5,
            net_enforcement: "host".into(),
            image_pull_policy: "IfNotPresent".into(),
        }
    }
}

#[derive(Default)]
pub struct KataExecutor {
    pub config: KataConfig,
}

fn kata_admission(spec: &JobSpec, config: &KataConfig) -> Result<(), ExecutorError> {
    crate::admission_check(spec)?;

    if !config.containerd_socket.exists() {
        return Err(ExecutorError::Backend(format!(
            "containerd socket not found: {}",
            config.containerd_socket.display()
        )));
    }

    match &spec.devices.class {
        DeviceClass::Cpu => {}
        DeviceClass::NvidiaGpu { .. }
        | DeviceClass::AmdGpu { .. }
        | DeviceClass::NvidiaMig { .. }
        | DeviceClass::NvidiaVgpu { .. } => {
            if config.vfio_devices.is_empty() {
                return Err(ExecutorError::Admission(
                    "GPU job requires vfio_devices in config".into(),
                ));
            }
        }
    }

    Ok(())
}

fn kata_network_interface(_job_id: &str) -> String {
    "kata_nic0".into()
}

impl Executor for KataExecutor {
    fn run(&self, spec: &JobSpec) -> Result<JobMetering, ExecutorError> {
        kata_admission(spec, &self.config)?;

        const MIN_MEM_KB: u64 = 128 * 1024;
        if spec.devices.mem_kb < MIN_MEM_KB {
            return Err(ExecutorError::Admission(format!(
                "mem_kb must be >= {} (128 MiB) for virtiofs shared memory; got {}",
                MIN_MEM_KB, spec.devices.mem_kb,
            )));
        }

        let is_gpu = matches!(
            spec.devices.class,
            DeviceClass::NvidiaGpu { .. }
                | DeviceClass::AmdGpu { .. }
                | DeviceClass::NvidiaMig { .. }
                | DeviceClass::NvidiaVgpu { .. }
        );

        let vfio_addrs = if is_gpu {
            select_gpu(
                spec,
                &CloudHypervisorConfig {
                    vfio_devices: self.config.vfio_devices.clone(),
                    gpu_helper: self.config.gpu_helper.clone(),
                    gpu_time_slice: self.config.gpu_time_slice,
                    ..Default::default()
                },
            )?
        } else {
            Vec::new()
        };

        let job_dir = self.config.workdir.join(&spec.job_id);
        if job_dir.exists() {
            return Err(ExecutorError::Backend(format!(
                "workdir already exists (duplicate job_id?): {}",
                job_dir.display()
            )));
        }
        fs::create_dir_all(job_dir.join("out")).map_err(|e| {
            ExecutorError::Backend(format!("create workdir {}: {e}", job_dir.display()))
        })?;

        write_manifest(
            &job_dir.join("manifest.json"),
            spec,
            &CloudHypervisorConfig::default(),
        )?;

        let client = ContainerdClient::new(&self.config.containerd_socket);
        client.pull_image(&spec.image, &self.config.image_pull_policy)?;

        let workload_id = format!("{}-workload", spec.job_id);
        let sidecar_id = format!("{}-sidecar", spec.job_id);
        let pod_id = spec.job_id.clone();

        let mut env_vars: Vec<(String, String)> = spec.env.clone();
        env_vars.push(("JOB_ID".into(), spec.job_id.clone()));
        env_vars.push(("VCPU_COUNT".into(), spec.devices.vcpus.to_string()));
        env_vars.push(("MEM_KB".into(), spec.devices.mem_kb.to_string()));
        if is_gpu {
            env_vars.push(("GPU_ENABLED".into(), "1".into()));
            env_vars.push(("VFIO_DEVICES".into(), vfio_addrs.join(",")));
        }

        let workload_config = ContainerConfig {
            id: workload_id.clone(),
            image: spec.image.clone(),
            command: spec.command.clone(),
            env: env_vars,
            volumes: vec![VolumeMount {
                host_path: job_dir.to_string_lossy().to_string(),
                container_path: "/mnt/vtessera".into(),
                readonly: false,
            }],
        };

        let sidecar_config = ContainerConfig {
            id: sidecar_id.clone(),
            image: self.config.sidecar_image.clone(),
            command: vec!["/usr/local/bin/metering-sidecar".into()],
            env: vec![
                ("JOB_ID".into(), spec.job_id.clone()),
                ("MANIFEST_PATH".into(), "/mnt/vtessera/manifest.json".into()),
                ("OUTPUT_DIR".into(), "/mnt/vtessera/out".into()),
            ],
            volumes: vec![VolumeMount {
                host_path: job_dir.to_string_lossy().to_string(),
                container_path: "/mnt/vtessera".into(),
                readonly: false,
            }],
        };

        let pod_config = PodConfig {
            id: pod_id.clone(),
            runtime: self.config.kata_runtime.clone(),
            containers: vec![workload_config, sidecar_config],
        };

        client.create_pod(&pod_config)?;
        client.run_pod(&pod_id)?;

        let iface = kata_network_interface(&spec.job_id);
        if spec.network != NetworkPolicy::None {
            apply_host_net_policy(&spec.job_id, &iface, &spec.network, &[])?;
        }

        let started = Instant::now();
        let max = Duration::from_secs(spec.max_duration_secs);

        let exit_code = loop {
            match client.wait_container(&workload_id) {
                Ok(code) => break code,
                Err(_) => {
                    if started.elapsed() >= max {
                        let _ = client.stop_pod(&pod_id);
                        let _ = client.remove_pod(&pod_id);
                        remove_host_net_policy(&spec.job_id);
                        if !self.config.keep_jobs {
                            let _ = fs::remove_dir_all(&job_dir);
                        }
                        return Ok(JobMetering {
                            job_id: spec.job_id.clone(),
                            backend: Backend::KataCloudHypervisor,
                            device: spec.devices.class.clone(),
                            cpu_seconds: max.as_secs_f64() * spec.devices.vcpus as f64,
                            peak_mem_kb: spec.devices.mem_kb,
                            gpu_seconds: if is_gpu { max.as_secs_f64() } else { 0.0 },
                            vram_gb_hours: 0.0,
                            exit_status: ExitStatus::TimedOut,
                            elapsed_secs: max.as_secs(),
                            gpu_sample: None,
                        });
                    }
                    std::thread::sleep(Duration::from_millis(50));
                }
            }
        };

        remove_host_net_policy(&spec.job_id);

        let _ = client.stop_pod(&pod_id);
        let _ = client.remove_pod(&pod_id);

        let exit_status = if exit_code == 0 {
            ExitStatus::Completed
        } else {
            ExitStatus::Failed { code: exit_code }
        };

        let elapsed_secs = started.elapsed().as_secs().max(1);

        let gpu_sample = None;

        let metering = if job_dir.join("out").join("metering.json").exists() {
            parse_metering(&job_dir, spec, Backend::KataCloudHypervisor, gpu_sample)?
        } else {
            JobMetering {
                job_id: spec.job_id.clone(),
                backend: Backend::KataCloudHypervisor,
                device: spec.devices.class.clone(),
                cpu_seconds: elapsed_secs as f64 * spec.devices.vcpus as f64,
                peak_mem_kb: spec.devices.mem_kb,
                gpu_seconds: if is_gpu { elapsed_secs as f64 } else { 0.0 },
                vram_gb_hours: 0.0,
                exit_status,
                elapsed_secs,
                gpu_sample,
            }
        };

        if !self.config.keep_jobs {
            let _ = fs::remove_dir_all(&job_dir);
        }

        Ok(metering)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn kata_config_defaults() {
        let config = KataConfig::default();
        assert_eq!(
            config.containerd_socket,
            PathBuf::from("/run/containerd/containerd.sock")
        );
        assert_eq!(config.kata_runtime, "kata");
        assert_eq!(config.workdir, PathBuf::from("/var/lib/vtessera/jobs"));
        assert_eq!(
            config.sidecar_image,
            "ghcr.io/vtessera/metering-sidecar:latest"
        );
        assert!(!config.keep_jobs);
        assert!(config.vfio_devices.is_empty());
    }

    #[test]
    fn kata_executor_default() {
        let executor = KataExecutor::default();
        assert_eq!(
            executor.config.containerd_socket,
            PathBuf::from("/run/containerd/containerd.sock")
        );
    }

    #[test]
    fn kata_network_interface_name() {
        let iface = kata_network_interface("test-job-0001");
        assert_eq!(iface, "kata_nic0");
    }
}
