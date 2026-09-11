use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::time::Duration;

use serde::{Deserialize, Serialize};

use crate::ExecutorError;

// Generated from the pinned CRI runtime/v1 proto (crates/executor/proto/
// cri_runtime_v1.proto). Keep the module rustfmt-skip + allow scoped so the
// committed, generated file is never reformatted or linted by hand-edited
// code conventions. Regenerate with scripts/regenerate-cri-api.sh.
#[rustfmt::skip]
#[allow(clippy::all, dead_code)]
#[path = "gen/cri_runtime_v1.rs"]
mod cri;

/// Container configuration for creating containers in a pod.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContainerConfig {
    pub id: String,
    pub image: String,
    pub command: Vec<String>,
    pub env: Vec<(String, String)>,
    pub volumes: Vec<VolumeMount>,
}

/// Volume mount configuration.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct VolumeMount {
    pub host_path: String,
    pub container_path: String,
    pub readonly: bool,
}

/// Pod configuration containing multiple containers.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PodConfig {
    pub id: String,
    pub runtime: String,
    pub containers: Vec<ContainerConfig>,
}

/// Per-pod client-side state for one CRI pod sandbox: the opaque sandbox id
/// containerd returned (which is NOT the caller's pod id) plus the container
/// ids we created inside it, in creation order.
#[derive(Debug, Clone)]
struct PodState {
    sandbox_id: String,
    containers: Vec<String>,
}

/// Containerd gRPC client for managing pods and containers.
///
/// Talks to containerd's CRI `runtime.v1` service (the same surface `crictl`
/// uses) over the containerd socket with tonic. Each pod maps to one CRI pod
/// sandbox (`RunPodSandbox`, runtime handler = `PodConfig.runtime`, e.g.
/// "kata"); each pod container maps to a CRI container created in that sandbox.
/// The executor's `Executor` trait is synchronous, so the async tonic client
/// is driven through a single-threaded tokio runtime owned by this struct.
pub struct ContainerdClient {
    socket: PathBuf,
    rt: Option<tokio::runtime::Runtime>,
    channel: Option<tonic::transport::Channel>,
    /// `PodConfig.id` → sandbox state. Populated by [`Self::create_pod`].
    pods: HashMap<String, PodState>,
}

impl ContainerdClient {
    /// Create a new containerd client connected to the given socket.
    ///
    /// The actual socket connection is established lazily on the first RPC so
    /// construction is infallible and works before containerd exists.
    pub fn new(socket: &Path) -> Self {
        Self {
            socket: socket.to_path_buf(),
            rt: None,
            channel: None,
            pods: HashMap::new(),
        }
    }

    /// Lazily build the single-threaded tokio runtime used to drive tonic.
    fn runtime(&mut self) -> Result<&tokio::runtime::Runtime, ExecutorError> {
        if self.rt.is_none() {
            let rt = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .map_err(|e| ExecutorError::Backend(format!("build tokio runtime: {e}")))?;
            self.rt = Some(rt);
        }
        Ok(self.rt.as_ref().expect("runtime set above"))
    }

    /// Get (or lazily establish) the tonic channel to the containerd socket.
    fn channel(&mut self) -> Result<tonic::transport::Channel, ExecutorError> {
        if let Some(channel) = &self.channel {
            return Ok(channel.clone());
        }

        let endpoint = tonic::transport::Endpoint::from_shared("http://[::]:50051")
            .map_err(|e| ExecutorError::Backend(format!("invalid containerd endpoint: {e}")))?
            .connect_timeout(Duration::from_secs(15));

        // The endpoint URI above is decorative — the connector below always
        // opens the Unix socket. `service_fn` infers `http::Uri` from the
        // `connect_with_connector` bound; no direct http dependency needed.
        let socket = self.socket.clone();
        let connector = tower::service_fn(move |_| {
            let socket = socket.clone();
            async move {
                let stream = tokio::net::UnixStream::connect(&socket)
                    .await
                    .map_err(|e| {
                        std::io::Error::new(
                            e.kind(),
                            format!("connect containerd socket {}: {e}", socket.display()),
                        )
                    })?;
                Ok::<_, std::io::Error>(hyper_util::rt::TokioIo::new(stream))
            }
        });

        let channel = self
            .runtime()?
            .block_on(endpoint.connect_with_connector(connector))
            .map_err(|e| {
                ExecutorError::Backend(format!(
                    "connect to containerd via {}: {e}",
                    self.socket.display()
                ))
            })?;
        self.channel = Some(channel.clone());
        Ok(channel)
    }

    /// A ready `runtime.v1.RuntimeService` client (client-only, no server).
    fn runtime_client(
        &mut self,
    ) -> Result<
        cri::runtime_service_client::RuntimeServiceClient<tonic::transport::Channel>,
        ExecutorError,
    > {
        let channel = self.channel()?;
        Ok(cri::runtime_service_client::RuntimeServiceClient::new(
            channel,
        ))
    }

    /// A ready `runtime.v1.ImageService` client for image operations.
    fn image_client(
        &mut self,
    ) -> Result<
        cri::image_service_client::ImageServiceClient<tonic::transport::Channel>,
        ExecutorError,
    > {
        let channel = self.channel()?;
        Ok(cri::image_service_client::ImageServiceClient::new(channel))
    }

    /// Pull an OCI image from a registry according to the pull policy.
    ///
    /// `policy` is one of "Always", "IfNotPresent", or "Never" (the standard
    /// Kubernetes image pull policies that `crikctl` and containerd honor).
    ///
    /// # Arguments
    /// * `image` - Full image reference (e.g., "docker.io/library/alpine:latest")
    /// * `policy` - Pull policy: "Always", "IfNotPresent", or "Never"
    pub fn pull_image(&mut self, image: &str, policy: &str) -> Result<(), ExecutorError> {
        if image.is_empty() {
            return Err(ExecutorError::Admission("empty image reference".into()));
        }
        match decide_pull(policy, self.image_present(image)?)? {
            PullDecision::Skip => return Ok(()),
            PullDecision::Pull => {}
        }

        let request = cri::PullImageRequest {
            image: Some(cri::ImageSpec {
                image: image.to_string(),
                annotations: HashMap::new(),
                ..Default::default()
            }),
            auth: None,
            sandbox_config: None,
        };
        let mut client = self.image_client()?;
        let rt = self.runtime()?;
        let response = rt
            .block_on(client.pull_image(request))
            .map_err(|s| rpc_error("PullImage", &s))?;
        response.into_inner();
        Ok(())
    }

    /// Create a container within a pod (requires that pod to exist first).
    ///
    /// CRI containers always live in a sandbox, so this needs the `pod_id`
    /// previously returned by [`Self::create_pod`]. Returns the containerd
    /// container id.
    pub fn create_container(
        &mut self,
        pod_id: &str,
        config: &ContainerConfig,
    ) -> Result<String, ExecutorError> {
        let state = self.pods.get(pod_id).cloned().ok_or_else(|| {
            ExecutorError::Backend(format!("unknown pod {pod_id}: create_pod first"))
        })?;
        let container_id = self.create_in_sandbox(&state.sandbox_id, config)?;
        if let Some(state) = self.pods.get_mut(pod_id) {
            state.containers.push(container_id.clone());
        }
        Ok(container_id)
    }

    /// Create a pod: one CRI sandbox (runtime handler = `PodConfig.runtime`,
    /// e.g. "kata") plus every configured container in it.
    ///
    /// Returns the caller's pod id; the opaque containerd sandbox id is kept
    /// internally and used by the later lifecycle calls.
    pub fn create_pod(&mut self, config: &PodConfig) -> Result<String, ExecutorError> {
        if self.pods.contains_key(&config.id) {
            return Err(ExecutorError::Admission(format!(
                "pod {} already exists",
                config.id
            )));
        }
        if config.id.is_empty() {
            return Err(ExecutorError::Admission("empty pod id".into()));
        }

        let sandbox_config = pod_sandbox_config(config);
        let request = cri::RunPodSandboxRequest {
            config: Some(sandbox_config.clone()),
            runtime_handler: config.runtime.clone(),
        };
        let mut client = self.runtime_client()?;
        let rt = self.runtime()?;
        let response = rt
            .block_on(client.run_pod_sandbox(request))
            .map_err(|s| rpc_error("RunPodSandbox", &s))?;
        let sandbox_id = response.into_inner().pod_sandbox_id;
        if sandbox_id.is_empty() {
            return Err(ExecutorError::Backend(
                "RunPodSandbox returned an empty sandbox id".into(),
            ));
        }

        let mut state = PodState {
            sandbox_id,
            containers: Vec::with_capacity(config.containers.len()),
        };
        for container in &config.containers {
            match self.create_in_sandbox(&state.sandbox_id, container) {
                Ok(id) => state.containers.push(id),
                Err(e) => {
                    // Don't leak a half-created sandbox on the second
                    // container failing — best-effort tear-down.
                    let _ = self.cleanup_sandbox(&state.sandbox_id);
                    return Err(e);
                }
            }
        }
        self.pods.insert(config.id.clone(), state);
        Ok(config.id.clone())
    }

    /// Create one CRI container inside an existing sandbox and return its id.
    fn create_in_sandbox(
        &mut self,
        sandbox_id: &str,
        config: &ContainerConfig,
    ) -> Result<String, ExecutorError> {
        let cri_config = container_config(config, sandbox_id);
        let request = cri::CreateContainerRequest {
            pod_sandbox_id: sandbox_id.to_string(),
            config: Some(cri_config),
            sandbox_config: None,
        };
        let mut client = self.runtime_client()?;
        let rt = self.runtime()?;
        let response = rt
            .block_on(client.create_container(request))
            .map_err(|s| rpc_error("CreateContainer", &s))?;
        let container_id = response.into_inner().container_id;
        if container_id.is_empty() {
            return Err(ExecutorError::Backend(
                "CreateContainer returned an empty container id".into(),
            ));
        }
        Ok(container_id)
    }

    /// Best-effort stop + remove of a sandbox that never made it into `pods`
    /// (used to clean up after a partially-created pod).
    fn cleanup_sandbox(&mut self, sandbox_id: &str) -> Result<(), ExecutorError> {
        let mut client = self.runtime_client()?;
        let rt = self.runtime()?;
        let stop = cri::StopPodSandboxRequest {
            pod_sandbox_id: sandbox_id.to_string(),
        };
        let _ = rt.block_on(client.stop_pod_sandbox(stop));
        let remove = cri::RemovePodSandboxRequest {
            pod_sandbox_id: sandbox_id.to_string(),
        };
        rt.block_on(client.remove_pod_sandbox(remove))
            .map_err(|s| rpc_error("RemovePodSandbox", &s))?;
        Ok(())
    }

    /// Start a pod: starts every container created by [`Self::create_pod`].
    pub fn run_pod(&mut self, pod_id: &str) -> Result<(), ExecutorError> {
        let state = self.pods.get(pod_id).cloned().ok_or_else(|| {
            ExecutorError::Backend(format!("unknown pod {pod_id}: create_pod first"))
        })?;
        if state.containers.is_empty() {
            return Err(ExecutorError::Backend(format!(
                "pod {pod_id} has no containers to start"
            )));
        }

        let mut client = self.runtime_client()?;
        let rt = self.runtime()?;
        for container_id in &state.containers {
            let request = cri::StartContainerRequest {
                container_id: container_id.clone(),
            };
            rt.block_on(client.start_container(request))
                .map_err(|s| rpc_error("StartContainer", &s))?;
        }
        Ok(())
    }

    /// Stop a pod sandbox and all of its containers.
    pub fn stop_pod(&mut self, pod_id: &str) -> Result<(), ExecutorError> {
        let state = self.pods.get(pod_id).cloned().ok_or_else(|| {
            ExecutorError::Backend(format!("unknown pod {pod_id}: create_pod first"))
        })?;
        let request = cri::StopPodSandboxRequest {
            pod_sandbox_id: state.sandbox_id,
        };
        let mut client = self.runtime_client()?;
        let rt = self.runtime()?;
        rt.block_on(client.stop_pod_sandbox(request))
            .map_err(|s| rpc_error("StopPodSandbox", &s))?;
        Ok(())
    }

    /// Remove a pod sandbox and its containers, and forget its client state.
    pub fn remove_pod(&mut self, pod_id: &str) -> Result<(), ExecutorError> {
        let state = self.pods.get(pod_id).cloned().ok_or_else(|| {
            ExecutorError::Backend(format!("unknown pod {pod_id}: create_pod first"))
        })?;
        let request = cri::RemovePodSandboxRequest {
            pod_sandbox_id: state.sandbox_id,
        };
        let mut client = self.runtime_client()?;
        let rt = self.runtime()?;
        rt.block_on(client.remove_pod_sandbox(request))
            .map_err(|s| rpc_error("RemovePodSandbox", &s))?;
        self.pods.remove(pod_id);
        Ok(())
    }

    /// Probe the exit status of a container in the pod sandbox.
    ///
    /// Returns `Ok(exit_code)` once the container is in the CRI `EXITED`
    /// state, or `Err(..)` while it is still created/running (or when
    /// containerd reports a failure). The caller polls at its own cadence —
    /// the Kata executor loops here and enforces `max_duration_secs`.
    pub fn wait_container(&mut self, container_id: &str) -> Result<i32, ExecutorError> {
        let request = cri::ContainerStatusRequest {
            container_id: container_id.to_string(),
            verbose: false,
        };
        let mut client = self.runtime_client()?;
        let rt = self.runtime()?;
        let response = rt
            .block_on(client.container_status(request))
            .map_err(|s| rpc_error("ContainerStatus", &s))?;
        let status = response
            .into_inner()
            .status
            .ok_or_else(|| ExecutorError::Backend("ContainerStatus returned no status".into()))?;

        exited_exit_code(&status).ok_or_else(|| {
            ExecutorError::Backend(format!(
                "workload container {container_id} has not exited (state={})",
                state_name(status.state)
            ))
        })
    }

    /// Is the image reference already present in containerd's image store?
    fn image_present(&mut self, image: &str) -> Result<bool, ExecutorError> {
        let request = cri::ListImagesRequest { filter: None };
        let mut client = self.image_client()?;
        let rt = self.runtime()?;
        let response = rt
            .block_on(client.list_images(request))
            .map_err(|s| rpc_error("ListImages", &s))?;
        let images = response.into_inner().images;
        Ok(images.iter().any(|img| {
            img.repo_tags.iter().any(|t| t == image) || img.repo_digests.iter().any(|d| d == image)
        }))
    }
}

/// Whether the image pull policy requires a pull for the current image state.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum PullDecision {
    Pull,
    Skip,
}

/// Map an image pull policy against whether the image is already present.
/// Pure so the policy table is unit-testable without a containerd.
fn decide_pull(policy: &str, present: bool) -> Result<PullDecision, ExecutorError> {
    match policy {
        "Always" => Ok(PullDecision::Pull),
        "IfNotPresent" => Ok(if present {
            PullDecision::Skip
        } else {
            PullDecision::Pull
        }),
        "Never" => {
            if present {
                Ok(PullDecision::Skip)
            } else {
                Err(ExecutorError::Backend(
                    "image not present and image_pull_policy is Never".into(),
                ))
            }
        }
        other => Err(ExecutorError::Backend(format!(
            "unknown image_pull_policy: {other}"
        ))),
    }
}

/// Map a job pod spec to a CRI `PodSandboxConfig`.
fn pod_sandbox_config(config: &PodConfig) -> cri::PodSandboxConfig {
    let mut labels = HashMap::new();
    labels.insert("vtessera.job".to_string(), config.id.clone());
    cri::PodSandboxConfig {
        metadata: Some(cri::PodSandboxMetadata {
            name: config.id.clone(),
            uid: String::new(),
            namespace: "vtessera".into(),
            attempt: 0,
        }),
        hostname: config.id.clone(),
        labels,
        linux: Some(cri::LinuxPodSandboxConfig::default()),
        ..Default::default()
    }
}

/// Map a job container spec to a CRI `ContainerConfig`.
fn container_config(config: &ContainerConfig, sandbox_id: &str) -> cri::ContainerConfig {
    let mut labels = HashMap::new();
    labels.insert("vtessera.pod".to_string(), sandbox_id.to_string());
    labels.insert("vtessera.container".to_string(), config.id.clone());
    cri::ContainerConfig {
        metadata: Some(cri::ContainerMetadata {
            name: config.id.clone(),
            attempt: 0,
        }),
        image: Some(cri::ImageSpec {
            image: config.image.clone(),
            annotations: HashMap::new(),
            ..Default::default()
        }),
        command: config.command.clone(),
        envs: config
            .env
            .iter()
            .map(|(key, value)| cri::KeyValue {
                key: key.clone(),
                value: value.clone().into_bytes(),
            })
            .collect(),
        mounts: config.volumes.iter().map(volume_mount).collect(),
        labels,
        ..Default::default()
    }
}

/// Map a single volume mount to the CRI `Mount` message.
fn volume_mount(mount: &VolumeMount) -> cri::Mount {
    cri::Mount {
        container_path: mount.container_path.clone(),
        host_path: mount.host_path.clone(),
        readonly: mount.readonly,
        // Host → guest propagation so the job_dir shared volume is visible
        // inside the Kata guest (needed for manifest + metering exchange).
        propagation: cri::MountPropagation::PropagationHostToContainer as i32,
        ..Default::default()
    }
}

/// Exit code from a CRI container status, or `None` while the container has
/// not yet exited (created/running/unknown).
fn exited_exit_code(status: &cri::ContainerStatus) -> Option<i32> {
    match cri::ContainerState::try_from(status.state) {
        Ok(cri::ContainerState::ContainerExited) => Some(status.exit_code),
        _ => None,
    }
}

/// Human-readable CRI container state for error messages.
fn state_name(state: i32) -> &'static str {
    match cri::ContainerState::try_from(state) {
        Ok(cri::ContainerState::ContainerCreated) => "created",
        Ok(cri::ContainerState::ContainerRunning) => "running",
        Ok(cri::ContainerState::ContainerExited) => "exited",
        _ => "unknown",
    }
}

/// Convert a tonic gRPC status into an [`ExecutorError::Backend`].
fn rpc_error(op: &str, status: &tonic::Status) -> ExecutorError {
    ExecutorError::Backend(format!("containerd {op} failed: {}", status.message()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn container_config_roundtrip() {
        let config = ContainerConfig {
            id: "test-container".into(),
            image: "docker.io/library/alpine:latest".into(),
            command: vec!["echo".into(), "hello".into()],
            env: vec![("FOO".into(), "bar".into())],
            volumes: vec![VolumeMount {
                host_path: "/tmp/test".into(),
                container_path: "/mnt/test".into(),
                readonly: false,
            }],
        };
        let json = serde_json::to_string(&config).unwrap();
        let parsed: ContainerConfig = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed.id, config.id);
        assert_eq!(parsed.image, config.image);
        assert_eq!(parsed.command, config.command);
        assert_eq!(parsed.env, config.env);
        assert_eq!(parsed.volumes.len(), 1);
    }

    #[test]
    fn pod_config_roundtrip() {
        let config = PodConfig {
            id: "test-pod".into(),
            runtime: "kata".into(),
            containers: vec![ContainerConfig {
                id: "workload".into(),
                image: "alpine:latest".into(),
                command: vec!["true".into()],
                env: vec![],
                volumes: vec![],
            }],
        };
        let json = serde_json::to_string(&config).unwrap();
        let parsed: PodConfig = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed.id, config.id);
        assert_eq!(parsed.runtime, config.runtime);
        assert_eq!(parsed.containers.len(), 1);
    }

    #[test]
    fn containerd_client_creation() {
        let client = ContainerdClient::new(Path::new("/run/containerd/containerd.sock"));
        assert_eq!(
            client.socket,
            PathBuf::from("/run/containerd/containerd.sock")
        );
        assert!(client.rt.is_none(), "runtime must be lazily built");
        assert!(client.pods.is_empty());
    }

    // --- Pull policy table ---

    #[test]
    fn pull_policy_always_pulls_even_if_present() {
        assert_eq!(decide_pull("Always", true).unwrap(), PullDecision::Pull);
        assert_eq!(decide_pull("Always", false).unwrap(), PullDecision::Pull);
    }

    #[test]
    fn pull_policy_if_not_present_skips_when_present() {
        assert_eq!(
            decide_pull("IfNotPresent", true).unwrap(),
            PullDecision::Skip
        );
        assert_eq!(
            decide_pull("IfNotPresent", false).unwrap(),
            PullDecision::Pull
        );
    }

    #[test]
    fn pull_policy_never_rejects_missing_image() {
        assert_eq!(decide_pull("Never", true).unwrap(), PullDecision::Skip);
        assert!(matches!(
            decide_pull("Never", false),
            Err(ExecutorError::Backend(_))
        ));
    }

    #[test]
    fn pull_policy_unknown_is_an_error() {
        assert!(matches!(
            decide_pull("Sometimes", true),
            Err(ExecutorError::Backend(_))
        ));
    }

    #[test]
    fn pull_image_rejects_empty_reference() {
        let mut client = ContainerdClient::new(Path::new("/does/not/exist.sock"));
        assert!(matches!(
            client.pull_image("", "IfNotPresent"),
            Err(ExecutorError::Admission(_))
        ));
    }

    // --- High-level → CRI mapping (pure functions, no containerd needed) ---

    #[test]
    fn pod_sandbox_mapping_carries_id_and_runtime_labels() {
        let config = PodConfig {
            id: "job-0001".into(),
            runtime: "kata".into(),
            containers: vec![],
        };
        let cri_config = pod_sandbox_config(&config);
        let metadata = cri_config.metadata.expect("metadata set");
        assert_eq!(metadata.name, "job-0001");
        assert_eq!(cri_config.hostname, "job-0001");
        assert_eq!(cri_config.labels.get("vtessera.job").unwrap(), "job-0001");
        assert!(cri_config.linux.is_some(), "linux sandbox config present");
    }

    #[test]
    fn container_mapping_carries_image_command_env_and_mounts() {
        let config = ContainerConfig {
            id: "workload".into(),
            image: "alpine:latest".into(),
            command: vec!["echo".into(), "hi".into()],
            env: vec![("FOO".into(), "bar".into())],
            volumes: vec![VolumeMount {
                host_path: "/var/lib/vtessera/jobs/job-1".into(),
                container_path: "/mnt/vtessera".into(),
                readonly: false,
            }],
        };
        let cri_config = container_config(&config, "sbx-1");
        let metadata = cri_config.metadata.expect("metadata set");
        assert_eq!(metadata.name, "workload");
        assert_eq!(cri_config.image.expect("image set").image, "alpine:latest");
        assert_eq!(cri_config.command, vec!["echo", "hi"]);
        assert_eq!(cri_config.args.len(), 0);
        assert_eq!(cri_config.envs.len(), 1);
        assert_eq!(cri_config.envs[0].key, "FOO");
        assert_eq!(cri_config.envs[0].value, b"bar");
        assert_eq!(cri_config.labels.get("vtessera.pod").unwrap(), "sbx-1");
        assert_eq!(cri_config.mounts.len(), 1);
        assert_eq!(
            cri_config.mounts[0].host_path,
            "/var/lib/vtessera/jobs/job-1"
        );
        assert_eq!(cri_config.mounts[0].container_path, "/mnt/vtessera");
    }

    #[test]
    fn volume_mount_maps_to_host_to_container_propagation() {
        let mount = volume_mount(&VolumeMount {
            host_path: "/host".into(),
            container_path: "/guest".into(),
            readonly: true,
        });
        assert_eq!(mount.host_path, "/host");
        assert_eq!(mount.container_path, "/guest");
        assert!(mount.readonly);
        assert_eq!(
            mount.propagation,
            cri::MountPropagation::PropagationHostToContainer as i32
        );
    }

    #[test]
    fn exited_exit_code_extracts_on_exited() {
        let status = cri::ContainerStatus {
            state: cri::ContainerState::ContainerExited as i32,
            exit_code: 7,
            ..Default::default()
        };
        assert_eq!(exited_exit_code(&status), Some(7));
    }

    #[test]
    fn exited_exit_code_is_none_while_running() {
        for (state, expected) in [
            (cri::ContainerState::ContainerCreated, None),
            (cri::ContainerState::ContainerRunning, None),
            (cri::ContainerState::ContainerUnknown, None),
        ] {
            let status = cri::ContainerStatus {
                state: state as i32,
                exit_code: 0,
                ..Default::default()
            };
            assert_eq!(exited_exit_code(&status), expected);
        }
    }

    #[test]
    fn state_name_maps_all_states() {
        assert_eq!(
            state_name(cri::ContainerState::ContainerCreated as i32),
            "created"
        );
        assert_eq!(
            state_name(cri::ContainerState::ContainerRunning as i32),
            "running"
        );
        assert_eq!(
            state_name(cri::ContainerState::ContainerExited as i32),
            "exited"
        );
        assert_eq!(state_name(99), "unknown");
    }

    #[test]
    fn rpc_error_maps_to_backend_with_message() {
        let status = tonic::Status::unavailable("socket gone");
        let err = rpc_error("PullImage", &status);
        match err {
            ExecutorError::Backend(msg) => {
                assert!(msg.contains("PullImage"), "op recorded: {msg}");
                assert!(msg.contains("socket gone"), "rpc message recorded: {msg}");
            }
            other => panic!("expected Backend, got {other:?}"),
        }
    }

    #[test]
    fn lifecycle_misses_an_unknown_pod() {
        let mut client = ContainerdClient::new(Path::new("/does/not/exist.sock"));
        for f in [
            |c: &mut ContainerdClient| c.run_pod("nope"),
            |c: &mut ContainerdClient| c.stop_pod("nope"),
            |c: &mut ContainerdClient| c.remove_pod("nope"),
        ] {
            assert!(matches!(f(&mut client), Err(ExecutorError::Backend(_))));
        }
        let config = ContainerConfig {
            id: "c".into(),
            image: "i".into(),
            command: vec![],
            env: vec![],
            volumes: vec![],
        };
        assert!(matches!(
            client.create_container("nope", &config),
            Err(ExecutorError::Backend(_))
        ));
    }
}
