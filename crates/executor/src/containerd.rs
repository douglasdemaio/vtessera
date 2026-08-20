use std::path::Path;

use serde::{Deserialize, Serialize};

use crate::ExecutorError;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContainerConfig {
    pub id: String,
    pub image: String,
    pub command: Vec<String>,
    pub env: Vec<(String, String)>,
    pub volumes: Vec<VolumeMount>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct VolumeMount {
    pub host_path: String,
    pub container_path: String,
    pub readonly: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PodConfig {
    pub id: String,
    pub runtime: String,
    pub containers: Vec<ContainerConfig>,
}

#[allow(dead_code)]
pub struct ContainerdClient {
    socket: String,
}

impl ContainerdClient {
    pub fn new(socket: &Path) -> Self {
        Self {
            socket: socket.to_string_lossy().to_string(),
        }
    }

    pub fn pull_image(&self, _image: &str, _policy: &str) -> Result<(), ExecutorError> {
        Ok(())
    }

    pub fn create_container(&self, _config: &ContainerConfig) -> Result<String, ExecutorError> {
        Ok(_config.id.clone())
    }

    pub fn create_pod(&self, _config: &PodConfig) -> Result<String, ExecutorError> {
        Ok(_config.id.clone())
    }

    pub fn run_pod(&self, _pod_id: &str) -> Result<(), ExecutorError> {
        Ok(())
    }

    pub fn stop_pod(&self, _pod_id: &str) -> Result<(), ExecutorError> {
        Ok(())
    }

    pub fn remove_pod(&self, _pod_id: &str) -> Result<(), ExecutorError> {
        Ok(())
    }

    pub fn wait_container(&self, _container_id: &str) -> Result<i32, ExecutorError> {
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
