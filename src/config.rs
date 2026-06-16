use serde::Deserialize;
use std::fs;
use std::path::{Path, PathBuf};

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(dead_code)]
pub struct Caps {
    pub max_cpus: Option<u64>,
    pub max_mem_kb: Option<u64>,
    pub max_disk_kb: Option<u64>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(dead_code)]
pub struct Config {
    pub sample_interval_secs: u64,
    pub state_dir: PathBuf,
    pub key_path: PathBuf,
    pub resource_caps: Caps,
    pub payout_id: String,
    #[serde(default)]
    pub submit_endpoint: Option<String>,
    #[serde(default)]
    pub window_size: Option<u64>,
}

impl Config {
    pub fn load(path: impl AsRef<Path>) -> Result<Self, ConfigError> {
        let raw = fs::read_to_string(path.as_ref()).map_err(ConfigError::Io)?;
        toml::from_str(&raw).map_err(ConfigError::Parse)
    }

    pub fn validate(&self) -> Result<(), ConfigError> {
        if self.sample_interval_secs == 0 || self.sample_interval_secs > 3600 {
            return Err(ConfigError::Validation(
                "sample_interval_secs must be between 1 and 3600".into(),
            ));
        }
        if self.payout_id.is_empty() {
            return Err(ConfigError::Validation(
                "payout_id must not be empty".into(),
            ));
        }
        if self.state_dir.as_os_str().is_empty() {
            return Err(ConfigError::Validation(
                "state_dir must not be empty".into(),
            ));
        }
        if self.key_path.as_os_str().is_empty() {
            return Err(ConfigError::Validation("key_path must not be empty".into()));
        }
        Ok(())
    }
}

#[derive(Debug)]
pub enum ConfigError {
    Io(std::io::Error),
    Parse(toml::de::Error),
    Validation(String),
}

impl std::fmt::Display for ConfigError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ConfigError::Io(e) => write!(f, "IO error reading config: {e}"),
            ConfigError::Parse(e) => write!(f, "TOML parse error: {e}"),
            ConfigError::Validation(e) => write!(f, "Config validation error: {e}"),
        }
    }
}

impl std::error::Error for ConfigError {}
