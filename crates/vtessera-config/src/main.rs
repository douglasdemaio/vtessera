use dialoguer::Input;
use std::fs;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let args: Vec<String> = std::env::args().skip(1).collect();

    // Non-interactive mode: if all flags are provided, skip prompts.
    let mode = get_arg(&args, "--mode").unwrap_or_else(|| prompt("Mode (public/private)", "public"));
    let cidrs = get_arg(&args, "--cidrs").unwrap_or_else(|| {
        prompt(
            "Allowed CIDRs (comma-separated, e.g. 10.0.0.0/8)",
            "",
        )
    });
    let require_ca = get_arg(&args, "--require-ca").is_some()
        || prompt_yes_no("Require internal CA key registry?", false);
    let marketplace_target = get_arg(&args, "--marketplace-target")
        .unwrap_or_else(|| prompt("Marketplace target (public/internal/none)", "public"));
    let marketplace_endpoint = if marketplace_target == "none" {
        None
    } else {
        Some(
            get_arg(&args, "--marketplace-endpoint")
                .unwrap_or_else(|| prompt("Internal marketplace endpoint", "")),
        )
    };
    let key_registry = if require_ca {
        Some(
            get_arg(&args, "--key-registry")
                .unwrap_or_else(|| prompt("Key registry path", "/etc/vtessera/keys.toml")),
        )
    } else {
        None
    };
    let output = get_arg(&args, "--output")
        .unwrap_or_else(|| prompt("Output config path", "/etc/vtessera/vtesserad.toml"));

    // Build TOML config.
    let mut toml = String::new();
    toml.push_str(&format!("mode = {mode:?}\n\n"));

    // Network section.
    toml.push_str("[network]\n");
    toml.push_str(&format!("mode = {mode:?}\n"));
    if !cidrs.is_empty() {
        let cidr_list: Vec<String> = cidrs
            .split(',')
            .map(|s| format!("{:?}", s.trim()))
            .collect();
        toml.push_str(&format!("allowed_cidrs = [{}]\n", cidr_list.join(", ")));
    }
    if require_ca {
        toml.push_str("require_internal_ca = true\n");
        if let Some(ref kr) = key_registry {
            toml.push_str(&format!("key_registry_path = {kr:?}\n"));
        }
    }
    toml.push('\n');

    // Marketplace section.
    toml.push_str("[marketplace]\n");
    toml.push_str(&format!("target = {marketplace_target:?}\n"));
    if let Some(ref ep) = marketplace_endpoint {
        toml.push_str(&format!("endpoint = {ep:?}\n"));
    }

    // Validate before writing.
    validate_config(&toml)?;

    // Write config.
    if let Some(parent) = std::path::Path::new(&output).parent() {
        fs::create_dir_all(parent)?;
    }
    fs::write(&output, &toml)?;
    println!("Config written to {output}");

    // Write empty key registry if needed.
    if let Some(ref kr) = key_registry {
        if !std::path::Path::new(kr).exists() {
            if let Some(parent) = std::path::Path::new(kr).parent() {
                fs::create_dir_all(parent)?;
            }
            fs::write(kr, "# Key registry — add [[keys]] entries manually\n\n")?;
            println!("Key registry written to {kr} (empty, add keys manually)");
        }
    }

    Ok(())
}

fn get_arg(args: &[String], flag: &str) -> Option<String> {
    args.iter()
        .position(|a| a == flag)
        .and_then(|i| args.get(i + 1))
        .cloned()
}

fn prompt(message: &str, default: &str) -> String {
    Input::new()
        .with_prompt(message)
        .default(default.to_string())
        .allow_empty(default.is_empty())
        .interact_text()
        .unwrap_or_default()
}

fn prompt_yes_no(message: &str, default: bool) -> bool {
    let default_str = if default { "y" } else { "n" };
    let input: String = Input::new()
        .with_prompt(message)
        .default(default_str.to_string())
        .interact_text()
        .unwrap_or(default_str.to_string());
    input.to_lowercase().starts_with('y')
}

fn validate_config(toml_str: &str) -> Result<(), Box<dyn std::error::Error>> {
    let parsed: toml::Value = toml::from_str(toml_str)?;

    // Validate mode.
    if let Some(mode) = parsed.get("network").and_then(|n| n.get("mode")) {
        let mode_str = mode.as_str().unwrap_or("");
        if mode_str != "public" && mode_str != "private" {
            return Err(format!("invalid mode: {mode_str}").into());
        }
    }

    // Validate marketplace target.
    if let Some(target) = parsed
        .get("marketplace")
        .and_then(|m| m.get("target"))
    {
        let target_str = target.as_str().unwrap_or("");
        if target_str != "public" && target_str != "internal" && target_str != "none" {
            return Err(format!("invalid marketplace target: {target_str}").into());
        }
    }

    // Validate CIDRs if present.
    if let Some(cidrs) = parsed
        .get("network")
        .and_then(|n| n.get("allowed_cidrs"))
        .and_then(|c| c.as_array())
    {
        for cidr in cidrs {
            let cidr_str = cidr.as_str().unwrap_or("");
            if !cidr_str.is_empty() {
                validate_cidr(cidr_str)?;
            }
        }
    }

    Ok(())
}

fn validate_cidr(s: &str) -> Result<(), Box<dyn std::error::Error>> {
    let parts: Vec<&str> = s.split('/').collect();
    if parts.len() != 2 {
        return Err(format!("invalid CIDR: {s}").into());
    }

    let prefix: u8 = parts[1]
        .parse()
        .map_err(|_| format!("invalid prefix length: {}", parts[1]))?;
    if prefix > 32 {
        return Err(format!("prefix length must be 0-32, got {prefix}").into());
    }

    let octets: Vec<u8> = parts[0]
        .split('.')
        .map(|o| o.parse::<u8>())
        .collect::<Result<_, _>>()
        .map_err(|_| format!("invalid octet in CIDR: {s}"))?;
    if octets.len() != 4 {
        return Err(format!("CIDR must have 4 octets: {s}").into());
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_validate_cidr_valid() {
        assert!(validate_cidr("10.0.0.0/8").is_ok());
        assert!(validate_cidr("192.168.1.0/24").is_ok());
        assert!(validate_cidr("0.0.0.0/0").is_ok());
        assert!(validate_cidr("255.255.255.255/32").is_ok());
    }

    #[test]
    fn test_validate_cidr_invalid() {
        assert!(validate_cidr("10.0.0.0").is_err());
        assert!(validate_cidr("10.0.0.0/33").is_err());
        assert!(validate_cidr("256.0.0.0/8").is_err());
        assert!(validate_cidr("abc.def.ghi.jkl/8").is_err());
    }

    #[test]
    fn test_validate_config_valid() {
        let toml = r#"
mode = "private"

[network]
mode = "private"
allowed_cidrs = ["10.0.0.0/8"]

[marketplace]
target = "internal"
endpoint = "https://compute.internal.corp/api/v1/receipts"
"#;
        assert!(validate_config(toml).is_ok());
    }

    #[test]
    fn test_validate_config_invalid_mode() {
        let toml = r#"
mode = "invalid"

[network]
mode = "invalid"

[marketplace]
target = "public"
"#;
        assert!(validate_config(toml).is_err());
    }

    #[test]
    fn test_get_arg() {
        let args = vec![
            "--mode".into(),
            "private".into(),
            "--cidrs".into(),
            "10.0.0.0/8".into(),
        ];
        assert_eq!(get_arg(&args, "--mode"), Some("private".into()));
        assert_eq!(get_arg(&args, "--cidrs"), Some("10.0.0.0/8".into()));
        assert_eq!(get_arg(&args, "--missing"), None);
    }
}
