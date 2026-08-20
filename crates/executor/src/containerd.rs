use std::path::Path;

use serde::{Deserialize, Serialize};

use crate::ExecutorError;

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

/// Containerd gRPC client for managing pods and containers.
///
/// This is a minimal client that wraps containerd's gRPC API. The actual
/// gRPC implementation uses tonic with containerd's protobuf definitions.
/// For now, the methods are stubs that will be filled in when testing
/// against a real containerd instance.
pub struct ContainerdClient {
    socket: String,
}

impl ContainerdClient {
    /// Create a new containerd client connected to the given socket.
    pub fn new(socket: &Path) -> Self {
        Self {
            socket: socket.to_string_lossy().to_string(),
        }
    }

    /// Pull an OCI image from a registry.
    ///
    /// # Arguments
    /// * `image` - Full image reference (e.g., "docker.io/library/alpine:latest")
    /// * `policy` - Pull policy: "Always", "IfNotPresent", or "Never"
    pub fn pull_image(&self, image: &str, policy: &str) -> Result<(), ExecutorError> {
        eprintln!(
            "containerd: pulling image {} (policy={}) from {}",
            image, policy, self.socket
        );

        // TODO: Implement actual gRPC call to containerd's ImageService.Pull
        // For now, this is a stub that succeeds immediately.

        Ok(())
    }

    /// Create a container within a pod.
    pub fn create_container(&self, config: &ContainerConfig) -> Result<String, ExecutorError> {
        eprintln!(
            "containerd: creating container {} (image={})",
            config.id, config.image
        );

        // TODO: Implement actual gRPC call to containerd's TaskService.Create
        // For now, return the container ID.

        Ok(config.id.clone())
    }

    /// Create a pod with the given configuration and containers.
    pub fn create_pod(&self, config: &PodConfig) -> Result<String, ExecutorError> {
        eprintln!(
            "containerd: creating pod {} (runtime={}, containers={})",
            config.id,
            config.runtime,
            config.containers.len()
        );

        // TODO: Implement actual gRPC calls to create pod sandbox
        // 1. Create pod sandbox with PodSandboxConfig
        // 2. Create each container with ContainerConfig
        // For now, return the pod ID.

        Ok(config.id.clone())
    }

    /// Start a pod and all its containers.
    pub fn run_pod(&self, pod_id: &str) -> Result<(), ExecutorError> {
        eprintln!("containerd: starting pod {pod_id}");

        // TODO: Implement actual gRPC call to containerd's TaskService.Start
        // For now, this is a stub that succeeds immediately.

        Ok(())
    }

    /// Stop a pod and all its containers.
    pub fn stop_pod(&self, pod_id: &str) -> Result<(), ExecutorError> {
        eprintln!("containerd: stopping pod {pod_id}");

        // TODO: Implement actual gRPC call to containerd's TaskService.Kill/Pause
        // For now, this is a stub that succeeds immediately.

        Ok(())
    }

    /// Remove a pod and its containers.
    pub fn remove_pod(&self, pod_id: &str) -> Result<(), ExecutorError> {
        eprintln!("containerd: removing pod {pod_id}");

        // TODO: Implement actual gRPC call to containerd's PodSandboxService.RemovePodSandbox
        // For now, this is a stub that succeeds immediately.

        Ok(())
    }

    /// Wait for a container to exit and return its exit code.
    pub fn wait_container(&self, container_id: &str) -> Result<i32, ExecutorError> {
        eprintln!("containerd: waiting for container {container_id}");

        // TODO: Implement actual gRPC call to containerd's TaskService.Wait
        // For now, return exit code 0.

        Ok(0)
    }
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
        assert_eq!(client.socket, "/run/containerd/containerd.sock");
    }
}
