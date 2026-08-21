use serde::Deserialize;
use std::fs;
use std::io;

/// Server configuration for the reference marketplace server.
#[derive(Debug, Deserialize)]
pub struct ServerConfig {
    /// Address to listen on (e.g. "0.0.0.0:8443").
    pub listen_addr: String,
    /// Path to the key registry TOML file.
    pub key_registry_path: String,
    /// Path to the JSON lines storage file.
    pub storage_path: String,
}

impl ServerConfig {
    /// Load config from a TOML file.
    pub fn load(path: &str) -> Result<Self, io::Error> {
        let contents = fs::read_to_string(path)?;
        let config: ServerConfig =
            toml::from_str(&contents).map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
        Ok(config)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use tempfile::NamedTempFile;

    #[test]
    fn test_load_valid_config() {
        let mut f = NamedTempFile::new().unwrap();
        writeln!(
            f,
            r#"
listen_addr = "0.0.0.0:8443"
key_registry_path = "/etc/vtessera/keys.toml"
storage_path = "/var/lib/vtessera/receipts.jsonl"
"#
        )
        .unwrap();

        let config = ServerConfig::load(f.path().to_str().unwrap()).unwrap();
        assert_eq!(config.listen_addr, "0.0.0.0:8443");
        assert_eq!(config.key_registry_path, "/etc/vtessera/keys.toml");
        assert_eq!(
            config.storage_path,
            "/var/lib/vtessera/receipts.jsonl"
        );
    }

    #[test]
    fn test_load_missing_file() {
        let err = ServerConfig::load("/nonexistent/path/config.toml");
        assert!(err.is_err());
    }

    #[test]
    fn test_load_invalid_toml() {
        let mut f = NamedTempFile::new().unwrap();
        writeln!(f, "not valid toml {{{{").unwrap();

        let err = ServerConfig::load(f.path().to_str().unwrap());
        assert!(err.is_err());
    }
}
