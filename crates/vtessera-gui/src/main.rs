//! Vtessera GUI — GTK4 front-end.
//!
//! Screen: a two-tab notebook. **Settings** edits the seller profile (free /
//! paid mode, payout address, price per CPU-hour, endpoint, escrow) and the
//! consent switches; **Status** shows the live state of the spawned
//! `vtesserad` + `vtessera-node` children, the settlement authority, a recent
//! job list, plus a rolling log and receipt count.
//!
//! Consent model (docs/CONSENT.md §2): on first run — or after a copy bump —
//! a gate window asks for explicit metering consent before anything is shown
//! or started. "Accept workloads from others" is a second, persisted switch
//! that is off by default; with it off, Start runs only `vtesserad` (metering
//! only) and no offer is written and no node spawned.
//!
//! Data flow (all under Flatpak-writable dirs, see [`crate::settings`]):
//!
//! 1. save `settings.toml` (GUI-owned fields incl. consent)
//! 2. derive + write `vtessera.toml` (daemon config)
//! 3. load/generate the Ed25519 identity key, build the signed offer
//!    (only when accepting workloads)
//! 4. spawn the two binaries with piped logs piped into the log tab
//!    (the node only when accepting workloads)

mod config;
mod daemon;
mod offer;
mod settings;

use std::cell::RefCell;
use std::rc::Rc;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use gtk4::glib;
use gtk4::prelude::*;

use daemon::Daemons;
use settings::Settings;

const APP_ID: &str = "io.github.douglasdemaio.Vtessera";

/// Widgets the callbacks need to reach.
struct Ui {
    settings: Rc<RefCell<Settings>>,
    free_btn: gtk4::ToggleButton,
    paid_btn: gtk4::ToggleButton,
    payout_entry: gtk4::Entry,
    price_entry: gtk4::Entry,
    currency_dd: gtk4::DropDown,
    port_spin: gtk4::SpinButton,
    endpoint_entry: gtk4::Entry,
    escrow_entry: gtk4::Entry,
    network_dd: gtk4::DropDown,
    network_custom_entry: gtk4::Entry,
    local_network_switch: gtk4::Switch,
    marketplace_url_entry: gtk4::Entry,
    cidr_entry: gtk4::Entry,
    interval_spin: gtk4::SpinButton,
    backend_dd: gtk4::DropDown,
    /// "Accept workloads from others" — the second consent gate (§2.2 of
    /// `docs/CONSENT.md`). OFF by default; off until explicitly enabled.
    accept_switch: gtk4::Switch,
    error_label: gtk4::Label,
    settlement_label: gtk4::Label,
    log_view: gtk4::TextView,
    start_btn: gtk4::Button,
    stop_btn: gtk4::Button,
    // Dashboard cards
    status_value: gtk4::Label,
    nodeid_value: gtk4::Label,
    cpu_value: gtk4::Label,
    cpu_fill: gtk4::Box,
    mem_value: gtk4::Label,
    mem_fill: gtk4::Box,
    // Jobs page
    jobs_list: gtk4::Box,
    total_val: gtk4::Label,
    earnings_val: gtk4::Label,
    avgcpu_val: gtk4::Label,
    // Dashboard: last job indicator
    last_job_label: gtk4::Label,
}

/// Mutable node runtime state.
struct NodeState {
    daemons: Rc<RefCell<Option<Daemons>>>,
    /// Worker threads push log lines here; a timeout drains it on the main
    /// thread (widgets may only be touched on the main thread).
    log_pending: Arc<Mutex<Vec<String>>>,
}

impl Ui {
    fn is_free(&self) -> bool {
        self.free_btn.is_active()
    }

    fn sync_mode_sensitivity(&self) {
        let paid = !self.is_free();
        self.payout_entry.set_sensitive(paid);
        self.price_entry.set_sensitive(paid);
        self.currency_dd.set_sensitive(paid);
    }

    /// Show/hide fields based on network dropdown and local-network toggle.
    fn sync_network_sensitivity(&self) {
        let custom = self.network_dd.selected() == 2;
        self.network_custom_entry.set_visible(custom);
        let local = self.local_network_switch.is_active();
        self.marketplace_url_entry.set_visible(!local);
        self.cidr_entry.set_visible(local);
    }

    fn set_error(&self, msg: &str) {
        self.error_label.set_text(msg);
        self.error_label.set_visible(!msg.is_empty());
    }

    fn clear_error(&self) {
        self.error_label.set_text("");
        self.error_label.set_visible(false);
    }

    fn log_line(&self, line: &str) {
        let buffer = self.log_view.buffer();
        let mut end = buffer.end_iter();
        buffer.insert(&mut end, &format!("{line}\n"));
        let mark = buffer.create_mark(None, &end, false);
        self.log_view.scroll_to_mark(&mark, 0.0, false, 0.0, 0.0);
    }

    fn read_settings(&self) -> Result<Settings, String> {
        let mode = if self.is_free() { "free" } else { "paid" };
        let currency = if self.currency_dd.selected() == 1 {
            "usdc"
        } else {
            "eurc"
        };
        let price = self.price_entry.text().trim().parse::<f64>().map_err(|_| {
            format!(
                "price \"{}\" is not a number",
                self.price_entry.text().trim()
            )
        })?;
        let port = self.port_spin.value() as u16;
        let backend = backend_from_dropdown(&self.backend_dd);
        let (network_preset, network) =
            network_from_dropdown(&self.network_dd, &self.network_custom_entry);
        let settings = Settings {
            mode: mode.into(),
            currency: currency.into(),
            price_per_cpu_hour: price,
            payout_id: self.payout_entry.text().trim().to_string(),
            port,
            endpoint: self.endpoint_entry.text().trim().to_string(),
            escrow_account: self.escrow_entry.text().trim().to_string(),
            network,
            network_preset,
            sample_interval_secs: self.interval_spin.value() as u64,
            backend: backend.into(),
            metering_consent: self.settings.borrow().metering_consent,
            accept_workloads: self.accept_switch.is_active(),
            consent_version: self.settings.borrow().consent_version,
            marketplace_url: self.marketplace_url_entry.text().trim().to_string(),
            local_network: self.local_network_switch.is_active(),
            allowed_cidrs: cidr_list_from_entry(&self.cidr_entry),
        };
        settings.validate()?;
        Ok(settings)
    }

    fn apply_settings(&self, s: &Settings) {
        self.free_btn.set_active(s.is_free());
        self.payout_entry.set_text(&s.payout_id);
        self.price_entry
            .set_text(&format_price(s.price_per_cpu_hour));
        self.currency_dd
            .set_selected(if s.currency == "usdc" { 1 } else { 0 });
        self.port_spin.set_value(s.port as f64);
        if s.endpoint.is_empty() {
            self.endpoint_entry.set_text("");
        } else {
            self.endpoint_entry.set_text(&s.endpoint);
        }
        self.escrow_entry.set_text(&s.escrow_account);
        self.network_dd
            .set_selected(network_preset_to_index(&s.network_preset));
        self.network_custom_entry.set_text(&s.network);
        self.network_custom_entry
            .set_visible(s.network_preset == "custom");
        self.local_network_switch.set_active(s.local_network);
        self.marketplace_url_entry.set_text(&s.marketplace_url);
        self.cidr_entry.set_text(&s.allowed_cidrs.join(", "));
        self.sync_network_sensitivity();
        self.interval_spin.set_value(s.sample_interval_secs as f64);
        self.backend_dd
            .set_selected(if s.backend == "local-cpu" { 1 } else { 0 });
        self.accept_switch.set_active(s.accept_workloads);
        self.sync_mode_sensitivity();
    }
}

/// Map the backend dropdown's selected index to the `--backend` string.
fn backend_from_dropdown(dd: &gtk4::DropDown) -> &'static str {
    if dd.selected() == 1 {
        "local-cpu"
    } else {
        "noop-cpu"
    }
}

/// Map the network dropdown index to `(preset, network_string)`.
fn network_from_dropdown(dd: &gtk4::DropDown, custom_entry: &gtk4::Entry) -> (String, String) {
    match dd.selected() {
        0 => ("devnet".into(), "solana-devnet".into()),
        1 => ("mainnet".into(), "solana-mainnet".into()),
        _ => ("custom".into(), custom_entry.text().trim().to_string()),
    }
}

fn network_preset_to_index(preset: &str) -> u32 {
    match preset {
        "devnet" => 0,
        "mainnet" => 1,
        _ => 2,
    }
}

/// Parse comma-separated CIDR list from entry text.
fn cidr_list_from_entry(entry: &gtk4::Entry) -> Vec<String> {
    entry
        .text()
        .split(',')
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect()
}

fn format_price(p: f64) -> String {
    let s = format!("{p:.4}");
    let s = s.trim_end_matches('0').trim_end_matches('.');
    if s.is_empty() {
        "0".into()
    } else {
        s.to_string()
    }
}

/// `node_id` of the identity key already on disk, if any.
fn current_node_id() -> Option<String> {
    let raw = std::fs::read(settings::key_path()).ok()?;
    if raw.len() != ed25519_dalek::SECRET_KEY_LENGTH {
        return None;
    }
    let mut arr = [0u8; ed25519_dalek::SECRET_KEY_LENGTH];
    arr.copy_from_slice(&raw);
    let key = ed25519_dalek::SigningKey::from_bytes(&arr);
    Some(vtessera_offer::derive_node_id(
        &key.verifying_key().to_bytes(),
    ))
}

/// Base58-encode a byte slice (Bitcoin alphabet).  Matches the
/// implementation in `vtesserad::key_registry`.
fn base58_encode(input: &[u8]) -> String {
    assert!(
        input.len() <= 32,
        "base58_encode supports at most 32 bytes, got {}",
        input.len()
    );
    const ALPHABET: &[u8] = b"123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
    let headroom = 4;
    let mut digits = vec![0u8; headroom + input.len() * 2];
    let mut start = headroom;

    for &byte in input {
        let mut carry = byte as u16;
        let mut j = digits.len() - 1;
        while j >= start {
            carry += (digits[j] as u16) << 8;
            digits[j] = (carry % 58) as u8;
            carry /= 58;
            j -= 1;
        }
        while carry > 0 {
            start -= 1;
            digits[start] = (carry % 58) as u8;
            carry /= 58;
        }
    }

    while start < digits.len() && digits[start] == 0 {
        start += 1;
    }

    let leading_ones = input.iter().take_while(|&&b| b == 0).count();
    let mut result = String::with_capacity(leading_ones + digits.len() - start);
    for _ in 0..leading_ones {
        result.push(ALPHABET[0] as char);
    }
    for &d in &digits[start..] {
        result.push(ALPHABET[d as usize] as char);
    }
    result
}

/// Detect the machine's LAN IP address by opening a UDP socket to a public
/// IP (no actual traffic is sent). Returns `None` if detection fails.
fn detect_lan_ip() -> Option<std::net::IpAddr> {
    let socket = std::net::UdpSocket::bind("0.0.0.0:0").ok()?;
    socket.connect("8.8.8.8:80").ok()?;
    let addr = socket.local_addr().ok()?;
    Some(addr.ip())
}

/// Auto-register the node's pubkey in the marketplace keys.toml so
/// vtessera-node can publish its offer without manual key setup.
fn register_node_in_marketplace(pubkey: &[u8; 32], node_id: &str) {
    let keys_path = settings::state_dir().join("marketplace").join("keys.toml");
    if let Err(e) = std::fs::create_dir_all(keys_path.parent().unwrap()) {
        eprintln!("marketplace dir create: {e}");
        return;
    }
    let b58 = base58_encode(pubkey);
    if let Ok(existing) = std::fs::read_to_string(&keys_path) {
        if existing.contains(&b58) {
            return;
        }
    }
    let entry = format!("\n[[keys]]\nname = \"{node_id}\"\npubkey = \"{b58}\"\n");
    let mut content = std::fs::read_to_string(&keys_path).unwrap_or_default();
    content.push_str(&entry);
    if let Err(e) = std::fs::write(&keys_path, &content) {
        eprintln!("marketplace keys write: {e}");
    }
}

/// The three observable states (§2.3 of `docs/CONSENT.md`):
/// **Off**, **Metering only** (vtesserad sampling; nothing accepts jobs),
/// **Accepting jobs** (vtesserad + vtessera-node serving).
fn current_state(state: &NodeState) -> &'static str {
    let borrowed = state.daemons.borrow();
    let Some(daemons) = borrowed.as_ref() else {
        return "Off";
    };
    if daemons.node.is_some() || daemons.node_reused {
        "Accepting jobs"
    } else {
        "Metering only"
    }
}

fn refresh_status(ui: &Ui, state: &NodeState) {
    let running = state.daemons.borrow().is_some();
    ui.start_btn.set_sensitive(!running);
    ui.stop_btn.set_sensitive(running);

    // Update dashboard status card.
    let state_text = current_state(state);
    ui.status_value.set_text(state_text);
    // Remove old status classes and add the correct one.
    ui.status_value.remove_css_class("status-off");
    ui.status_value.remove_css_class("status-metering");
    ui.status_value.remove_css_class("status-active");
    match state_text {
        "Off" => ui.status_value.add_css_class("status-off"),
        "Metering only" => ui.status_value.add_css_class("status-metering"),
        "Accepting jobs" => ui.status_value.add_css_class("status-active"),
        _ => {}
    }

    // Update dashboard node ID card.
    let node_id = current_node_id().unwrap_or_else(|| "—".into());
    ui.nodeid_value.set_text(&node_id);
}

/// Read the latest receipt file from state_dir and update dashboard CPU/memory cards.
fn refresh_dashboard(ui: &Ui) {
    let state_dir = settings::state_dir();
    let mut latest_ts = 0u64;
    let mut cpu_pct = 0.0f64;
    let mut mem_kb = 0u64;

    if let Ok(rd) = std::fs::read_dir(&state_dir) {
        for entry in rd.flatten() {
            let name = entry.file_name();
            let name_str = name.to_string_lossy();
            if !name_str.starts_with("receipt_") || !name_str.ends_with(".json") {
                continue;
            }
            if let Ok(raw) = std::fs::read_to_string(entry.path()) {
                if let Ok(val) = serde_json::from_str::<serde_json::Value>(&raw) {
                    let ts = val["receipt"]["window_end"].as_u64().unwrap_or(0);
                    if ts >= latest_ts {
                        latest_ts = ts;
                        cpu_pct = val["receipt"]["totals"]["cpu_pct_avg"]
                            .as_f64()
                            .unwrap_or(0.0);
                        mem_kb = val["receipt"]["totals"]["mem_used_kb_avg"]
                            .as_u64()
                            .unwrap_or(0);
                    }
                }
            }
        }
    }

    if latest_ts > 0 {
        ui.cpu_value.set_text(&format!("{:.1}%", cpu_pct));
        let mem_gb = mem_kb as f64 / 1_048_576.0;
        ui.mem_value.set_text(&format!("{:.1} GB", mem_gb));

        // Update progress bars (cap at 100% for CPU, assume 8 GB total for mem).
        let cpu_width = ((cpu_pct).min(100.0) / 100.0 * 200.0) as i32;
        ui.cpu_fill.set_size_request(cpu_width, 4);
        let mem_pct = (mem_gb / 8.0 * 100.0).min(100.0);
        let mem_width = (mem_pct / 100.0 * 200.0) as i32;
        ui.mem_fill.set_size_request(mem_width, 4);
    }
}

/// Read job receipts and populate the jobs table + summary bar.
///
/// Handles both `SignedJobReceipt` (from vtessera-node, with
/// `receipt.metering.cpu_seconds` / `receipt.metering.peak_mem_kb`) and
/// legacy vtesserad window-receipts (with
/// `receipt.totals.cpu_pct_avg` / `receipt.totals.mem_used_kb_avg`).
fn refresh_jobs_table(ui: &Ui) {
    use std::time::SystemTime;

    let dir = settings::state_dir().join("job-receipts");
    let now = SystemTime::now();

    #[derive(serde::Deserialize)]
    struct Metering {
        cpu_seconds: f64,
        peak_mem_kb: u64,
        elapsed_secs: u64,
    }
    #[derive(serde::Deserialize)]
    struct JobReceiptInner {
        metering: Metering,
    }
    #[derive(serde::Deserialize)]
    struct SignedJobReceipt {
        receipt: JobReceiptInner,
    }

    let mut jobs: Vec<(u64, String, f64, u64, u64)> = Vec::new();
    if let Ok(rd) = std::fs::read_dir(&dir) {
        for entry in rd.flatten() {
            let path = entry.path();
            if path.extension().is_some_and(|x| x == "json") {
                let age = std::fs::metadata(&path)
                    .and_then(|m| m.modified())
                    .ok()
                    .and_then(|t| now.duration_since(t).ok())
                    .map(|d| d.as_secs())
                    .unwrap_or(u64::MAX);
                let name = path
                    .file_name()
                    .unwrap_or_default()
                    .to_string_lossy()
                    .into_owned();
                if let Ok(raw) = std::fs::read_to_string(&path) {
                    if let Ok(job) = serde_json::from_str::<SignedJobReceipt>(&raw) {
                        let m = &job.receipt.metering;
                        // Convert cpu_seconds to approximate CPU% using elapsed_secs.
                        let cpu_pct = if m.elapsed_secs > 0 {
                            (m.cpu_seconds / m.elapsed_secs as f64 * 100.0).min(100.0)
                        } else {
                            0.0
                        };
                        jobs.push((age, name, cpu_pct, m.peak_mem_kb, m.elapsed_secs));
                    } else {
                        jobs.push((age, name, 0.0, 0, 0));
                    }
                }
            }
        }
    }
    jobs.sort_by_key(|j| j.0);

    // Summary metrics.
    let total = jobs.len();
    let avg_cpu = if total > 0 {
        jobs.iter().map(|j| j.2).sum::<f64>() / total as f64
    } else {
        0.0
    };
    ui.total_val.set_text(&format!("{}", total));
    ui.avgcpu_val.set_text(&format!("{:.1}%", avg_cpu));
    // Earnings — placeholder since v0 receipts don't carry price info.
    ui.earnings_val.set_text("\u{2014}");

    // Last job indicator — show the age of the most recent receipt.
    if let Some((age, name, _, _, _)) = jobs.last() {
        let short_id = name
            .trim_end_matches(".json")
            .chars()
            .take(12)
            .collect::<String>();
        let stamp = if *age == u64::MAX {
            "unknown".to_string()
        } else if *age < 60 {
            format!("{}s ago — {}", age, short_id)
        } else if *age < 3600 {
            format!("{}m ago — {}", age / 60, short_id)
        } else {
            format!("{}h ago — {}", age / 3600, short_id)
        };
        ui.last_job_label.set_text(&stamp);
    } else {
        ui.last_job_label.set_text("No jobs yet");
    }

    // Clear old rows.
    while let Some(child) = ui.jobs_list.first_child() {
        ui.jobs_list.remove(&child);
    }

    // Add rows.
    for (age, name, cpu, mem_kb, _elapsed) in jobs.into_iter().rev().take(50) {
        let row = gtk4::Box::new(gtk4::Orientation::Horizontal, 0);
        row.add_css_class("job-table-row");

        let stamp = if age == u64::MAX {
            "unknown".to_string()
        } else if age < 60 {
            format!("{}s ago", age)
        } else if age < 3600 {
            format!("{}m ago", age / 60)
        } else {
            format!("{}h ago", age / 3600)
        };

        let mem_gb = mem_kb as f64 / 1_048_576.0;
        let short_id = name
            .trim_end_matches(".json")
            .chars()
            .take(12)
            .collect::<String>();

        for text in [
            "\u{25cf}".to_string(), // status dot
            short_id,
            format!("{:.1}%", cpu),
            format!("{:.1} GB", mem_gb),
            "\u{2014}".to_string(), // earnings placeholder
            stamp,
        ] {
            let l = gtk4::Label::new(Some(&text));
            l.set_xalign(0.0);
            l.set_hexpand(true);
            l.set_margin_start(8);
            l.set_margin_end(8);
            l.add_css_class("job-table-cell");
            if text == "\u{25cf}" {
                l.add_css_class("status-green");
            }
            row.append(&l);
        }
        ui.jobs_list.append(&row);
    }
}

fn start_node(ui: &Ui, state: &NodeState) {
    let mut settings = match ui.read_settings() {
        Ok(s) => s,
        Err(e) => {
            ui.set_error(&e);
            return;
        }
    };
    ui.clear_error();
    *ui.settings.borrow_mut() = settings.clone();

    if let Err(e) = settings.save(&settings::settings_path()) {
        ui.set_error(&e);
        return;
    }
    if let Err(e) = config::write_daemon_config(
        &settings,
        &settings::daemon_config_path(),
        &settings::key_path(),
        &settings::state_dir(),
    ) {
        ui.set_error(&e);
        return;
    }

    let key = match offer::load_or_generate_key(&settings::key_path()) {
        Ok(k) => k,
        Err(e) => {
            ui.set_error(&e);
            return;
        }
    };

    // The signed offer advertises "this machine accepts your jobs" — that is
    // the second consent gate (§2.2). With `accept_workloads` off we write no
    // offer and spawn no node; only vtesserad meters.
    let accepting = settings.accept_workloads;

    // Detect LAN IP for multi-machine discovery. When the endpoint is
    // empty (the default), auto-fill with the LAN address so agents on
    // the same network can reach this node.
    let lan_ip = detect_lan_ip()
        .map(|ip| ip.to_string())
        .unwrap_or_else(|| "127.0.0.1".into());
    if settings.endpoint.is_empty() {
        settings.endpoint = format!("http://{lan_ip}:{}", settings.port);
    }

    if accepting {
        let offer_json = offer::build_offer_json(&settings, &key);
        if let Err(e) = std::fs::write(settings::offer_path(), &offer_json) {
            ui.set_error(&e.to_string());
            return;
        }
    }

    let node_id = vtessera_offer::derive_node_id(&key.verifying_key().to_bytes());

    // Auto-register this node's pubkey in the marketplace key registry
    // so vtessera-node can publish without manual keys.toml setup.
    register_node_in_marketplace(&key.verifying_key().to_bytes(), &node_id);

    // Bind to all interfaces so other machines can reach the node.
    let bind = format!("0.0.0.0:{}", settings.port);

    // Offer-index — the discovery service that agents query and nodes
    // publish to. Bind to all interfaces for LAN/internet reachability.
    let index_port: u16 = 8403;
    let index_bind = format!("0.0.0.0:{index_port}");

    // Publish URL points to the offer-index, auto-detected from LAN IP.
    let publish = Some(format!("http://{lan_ip}:{index_port}"));

    let bin_dir = daemon::bin_dir();
    let opts = daemon::StartOptions {
        bin_dir: bin_dir.as_deref(),
        daemon_config: &settings::daemon_config_path(),
        offer: &settings::offer_path(),
        bind,
        escrow: &settings.escrow_account,
        network: &settings.network,
        backend: &settings.backend,
        key_path: settings::key_path(),
        state_dir: settings::state_dir(),
        spawn_node: accepting,
        publish,
        index_bind: Some(index_bind),
    };
    let mut daemons = match daemon::start(&opts) {
        Ok(d) => d,
        Err(e) => {
            ui.set_error(&e);
            return;
        }
    };
    let node_reused = daemons.node_reused;

    let tx = state.log_pending.clone();
    daemon::pump_output(&mut daemons, move |line| {
        tx.lock().unwrap().push(line);
    });
    *state.daemons.borrow_mut() = Some(daemons);

    if !accepting {
        ui.log_line(&format!(
            "metering started — vtesserad sampling every {}s; NOT accepting jobs \
             (turn on \"Accept workloads from others\" and Start again to serve).",
            settings.sample_interval_secs
        ));
    } else if node_reused {
        ui.log_line(&format!(
            "existing node already serving port {} — reusing it (left by a previous session). \
             Its offer predates these settings; Stop then Start to reload.",
            settings.port
        ));
    } else {
        let kind = if settings.is_free() {
            "free (donating compute)".to_string()
        } else {
            format!(
                "paid — {} {}/CPU-hour, payout {}",
                settings.currency.to_uppercase(),
                format_price(settings.price_per_cpu_hour),
                settings.payout_id
            )
        };
        ui.log_line(&format!(
            "node started — {kind} | node_id {node_id} | backend {} | serving {}",
            settings.backend, settings.endpoint
        ));
        ui.log_line(&format!(
            "offer written to {}",
            settings::offer_path().display()
        ));

    }

    refresh_status(ui, state);
}

fn stop_node(ui: &Ui, state: &NodeState) {
    if let Some(mut d) = state.daemons.borrow_mut().take() {
        daemon::stop(&mut d, &|line| ui.log_line(&line));
        refresh_status(ui, state);
    }
}

fn build_ui(app: &gtk4::Application) {
    install_css();
    // Force dark theme for native GTK widgets (header bar, tabs, scrollbars).
    if let Some(ctx) = gtk4::Settings::default() {
        ctx.set_property("gtk-application-prefer-dark-theme", true);
    }

    let log_pending: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));

    let initial = Settings::load_or_default(&settings::settings_path());

    // Create dashboard card widgets before Ui init so they can be stored.
    fn make_card(title: &str) -> (gtk4::Box, gtk4::Label, gtk4::Label) {
        let card = gtk4::Box::new(gtk4::Orientation::Vertical, 4);
        card.add_css_class("dashboard-card");
        let title_label = gtk4::Label::new(Some(title));
        title_label.set_xalign(0.0);
        title_label.add_css_class("dashboard-card-title");
        card.append(&title_label);
        let value_label = gtk4::Label::new(Some("—"));
        value_label.set_xalign(0.0);
        value_label.add_css_class("dashboard-card-value");
        card.append(&value_label);
        (card, title_label, value_label)
    }

    fn make_card_with_bar(title: &str, accent: &str) -> (gtk4::Box, gtk4::Label, gtk4::Box) {
        let card = gtk4::Box::new(gtk4::Orientation::Vertical, 4);
        card.add_css_class("dashboard-card");
        let title_label = gtk4::Label::new(Some(title));
        title_label.set_xalign(0.0);
        title_label.add_css_class("dashboard-card-title");
        card.append(&title_label);
        let value_label = gtk4::Label::new(Some("—"));
        value_label.set_xalign(0.0);
        value_label.add_css_class("dashboard-card-value");
        value_label.add_css_class(accent);
        card.append(&value_label);
        let track = gtk4::Box::new(gtk4::Orientation::Horizontal, 0);
        track.add_css_class("progress-track");
        track.set_size_request(-1, 4);
        let fill = gtk4::Box::new(gtk4::Orientation::Horizontal, 0);
        fill.add_css_class("progress-fill");
        fill.add_css_class(&format!("progress-fill-{accent}"));
        fill.set_size_request(0, 4);
        track.append(&fill);
        card.append(&track);
        (card, value_label, fill)
    }

    let (status_card, _, status_value) = make_card("STATUS");
    let (nodeid_card, _, nodeid_value) = make_card("NODE ID");
    let (cpu_card, cpu_value, cpu_fill) = make_card_with_bar("CPU", "cpu-accent");
    let (mem_card, mem_value, mem_fill) = make_card_with_bar("MEMORY", "mem-accent");

    // Last-job indicator card — spans full width below the 2×2 grid.
    let last_job_card = make_card("LAST JOB");
    let last_job_label = last_job_card.2.clone();

    // Create jobs page widgets before Ui init.
    fn make_summary(label: &str) -> (gtk4::Box, gtk4::Label) {
        let col = gtk4::Box::new(gtk4::Orientation::Vertical, 2);
        let val = gtk4::Label::new(Some("0"));
        val.add_css_class("summary-metric-value");
        val.set_xalign(0.0);
        col.append(&val);
        let lbl = gtk4::Label::new(Some(label));
        lbl.add_css_class("summary-metric-label");
        lbl.set_xalign(0.0);
        col.append(&lbl);
        (col, val)
    }

    let (total_box, total_val) = make_summary("Total Jobs");
    let (earnings_box, earnings_val) = make_summary("Earnings");
    let (avgcpu_box, avgcpu_val) = make_summary("Avg CPU");
    let jobs_list = gtk4::Box::new(gtk4::Orientation::Vertical, 0);

    let ui = Rc::new(Ui {
        settings: Rc::new(RefCell::new(initial.clone())),
        free_btn: gtk4::ToggleButton::with_label("Donate (free)"),
        paid_btn: gtk4::ToggleButton::with_label("Sell (paid)"),
        payout_entry: gtk4::Entry::new(),
        price_entry: gtk4::Entry::new(),
        currency_dd: gtk4::DropDown::from_strings(&["EURC", "USDC"]),
        port_spin: gtk4::SpinButton::new(
            Some(&gtk4::Adjustment::new(8402.0, 1.0, 65535.0, 1.0, 10.0, 0.0)),
            0.0,
            0,
        ),
        endpoint_entry: gtk4::Entry::new(),
        escrow_entry: gtk4::Entry::new(),
        network_dd: gtk4::DropDown::from_strings(&["Solana Devnet", "Solana Mainnet", "Custom"]),
        network_custom_entry: gtk4::Entry::new(),
        local_network_switch: gtk4::Switch::new(),
        marketplace_url_entry: gtk4::Entry::new(),
        cidr_entry: gtk4::Entry::new(),
        interval_spin: gtk4::SpinButton::new(
            Some(&gtk4::Adjustment::new(60.0, 1.0, 3600.0, 1.0, 10.0, 0.0)),
            0.0,
            0,
        ),
        backend_dd: gtk4::DropDown::from_strings(&[
            "noop-cpu (simulate)",
            "local-cpu (run on host)",
        ]),
        accept_switch: gtk4::Switch::new(),
        error_label: gtk4::Label::new(None),
        settlement_label: gtk4::Label::new(None),
        log_view: gtk4::TextView::new(),
        start_btn: gtk4::Button::with_label("Start"),
        stop_btn: gtk4::Button::with_label("Stop"),
        status_value,
        nodeid_value,
        cpu_value,
        cpu_fill,
        mem_value,
        mem_fill,
        jobs_list,
        total_val,
        earnings_val,
        avgcpu_val,
        last_job_label,
    });

    // ---- Settings page ---------------------------------------------------
    let settings_page = gtk4::Box::new(gtk4::Orientation::Vertical, 12);
    settings_page.set_margin_top(24);
    settings_page.set_margin_bottom(24);
    settings_page.set_margin_start(24);
    settings_page.set_margin_end(24);

    let intro = gtk4::Label::new(Some(
        "Turn this machine into a Vtessera compute node. Agents buy CPU time \
         (paid) or get it for free (donate). Settings are saved when you press Start.",
    ));
    intro.set_wrap(true);
    intro.set_xalign(0.0);
    intro.add_css_class("dim-label");
    settings_page.append(&intro);

    let grid = gtk4::Grid::new();
    grid.set_column_spacing(12);
    grid.set_row_spacing(12);
    grid.set_hexpand(true);

    let mut row = 0;

    let mode_label = gtk4::Label::new(Some("Mode"));
    mode_label.set_xalign(0.0);
    grid.attach(&mode_label, 0, row, 1, 1);

    ui.free_btn.set_group(None::<&gtk4::ToggleButton>);
    ui.paid_btn.set_group(Some(&ui.free_btn));
    ui.free_btn.set_active(true);
    let mode_box = gtk4::Box::new(gtk4::Orientation::Horizontal, 0);
    mode_box.add_css_class("mode-segmented");
    mode_box.append(&ui.free_btn);
    mode_box.append(&ui.paid_btn);
    mode_box.set_halign(gtk4::Align::Start);
    grid.attach(&mode_box, 1, row, 1, 1);
    row += 1;

    let payout_caption = gtk4::Label::new(Some("Solana payout address"));
    payout_caption.set_xalign(0.0);
    grid.attach(&payout_caption, 0, row, 1, 1);
    ui.payout_entry
        .set_placeholder_text(Some("Base58 Solana address"));
    ui.payout_entry.set_hexpand(true);
    grid.attach(&ui.payout_entry, 1, row, 1, 1);
    row += 1;

    let price_caption = gtk4::Label::new(Some("Price per CPU-hour"));
    price_caption.set_xalign(0.0);
    grid.attach(&price_caption, 0, row, 1, 1);
    ui.price_entry.set_placeholder_text(Some("0.05"));
    ui.price_entry.set_hexpand(true);
    let price_box = gtk4::Box::new(gtk4::Orientation::Horizontal, 8);
    price_box.append(&ui.price_entry);
    price_box.append(&ui.currency_dd);
    grid.attach(&price_box, 1, row, 1, 1);
    row += 1;

    let port_caption = gtk4::Label::new(Some("Port"));
    port_caption.set_xalign(0.0);
    grid.attach(&port_caption, 0, row, 1, 1);
    ui.port_spin.set_hexpand(true);
    ui.port_spin.set_halign(gtk4::Align::Start);
    grid.attach(&ui.port_spin, 1, row, 1, 1);
    row += 1;

    let endpoint_caption = gtk4::Label::new(Some("Advertised endpoint"));
    endpoint_caption.set_xalign(0.0);
    grid.attach(&endpoint_caption, 0, row, 1, 1);
    ui.endpoint_entry
        .set_placeholder_text(Some("http://your-public-ip:8402"));
    ui.endpoint_entry.set_hexpand(true);
    grid.attach(&ui.endpoint_entry, 1, row, 1, 1);
    row += 1;

    let escrow_caption = gtk4::Label::new(Some("Escrow account"));
    escrow_caption.set_xalign(0.0);
    grid.attach(&escrow_caption, 0, row, 1, 1);
    ui.escrow_entry.set_hexpand(true);
    grid.attach(&ui.escrow_entry, 1, row, 1, 1);
    row += 1;

    let network_caption = gtk4::Label::new(Some("Network"));
    network_caption.set_xalign(0.0);
    grid.attach(&network_caption, 0, row, 1, 1);
    let network_box = gtk4::Box::new(gtk4::Orientation::Vertical, 4);
    let network_row = gtk4::Box::new(gtk4::Orientation::Horizontal, 8);
    ui.network_dd.set_halign(gtk4::Align::Start);
    network_row.append(&ui.network_dd);
    ui.network_custom_entry
        .set_placeholder_text(Some("e.g. solana-devnet"));
    ui.network_custom_entry.set_hexpand(true);
    ui.network_custom_entry.set_visible(false);
    network_row.append(&ui.network_custom_entry);
    network_box.append(&network_row);
    grid.attach(&network_box, 1, row, 1, 1);
    row += 1;

    let local_network_caption = gtk4::Label::new(Some("Local network only"));
    local_network_caption.set_xalign(0.0);
    grid.attach(&local_network_caption, 0, row, 1, 1);
    ui.local_network_switch.set_halign(gtk4::Align::Start);
    ui.local_network_switch.set_valign(gtk4::Align::Center);
    grid.attach(&ui.local_network_switch, 1, row, 1, 1);
    row += 1;

    let local_network_hint = gtk4::Label::new(Some(
        "ON = operate on a private LAN only (no internet discovery, no marketplace \
         publishing). OFF = public internet mode with offer-index and iroh connectivity.",
    ));
    local_network_hint.set_wrap(true);
    local_network_hint.set_xalign(0.0);
    local_network_hint.add_css_class("dim-label");
    grid.attach(&local_network_hint, 0, row, 2, 1);
    row += 1;

    let marketplace_caption = gtk4::Label::new(Some("Marketplace URL"));
    marketplace_caption.set_xalign(0.0);
    grid.attach(&marketplace_caption, 0, row, 1, 1);
    ui.marketplace_url_entry
        .set_placeholder_text(Some("http://<index-ip>:8443"));
    ui.marketplace_url_entry.set_hexpand(true);
    grid.attach(&ui.marketplace_url_entry, 1, row, 1, 1);
    row += 1;

    let cidr_caption = gtk4::Label::new(Some("Allowed CIDRs"));
    cidr_caption.set_xalign(0.0);
    grid.attach(&cidr_caption, 0, row, 1, 1);
    ui.cidr_entry
        .set_placeholder_text(Some("192.168.1.0/24, 10.0.0.0/8"));
    ui.cidr_entry.set_hexpand(true);
    ui.cidr_entry.set_visible(false);
    grid.attach(&ui.cidr_entry, 1, row, 1, 1);
    row += 1;

    let interval_caption = gtk4::Label::new(Some("Sample interval (s)"));
    interval_caption.set_xalign(0.0);
    grid.attach(&interval_caption, 0, row, 1, 1);
    ui.interval_spin.set_hexpand(true);
    ui.interval_spin.set_halign(gtk4::Align::Start);
    grid.attach(&ui.interval_spin, 1, row, 1, 1);
    row += 1;

    let backend_caption = gtk4::Label::new(Some("Job backend"));
    backend_caption.set_xalign(0.0);
    grid.attach(&backend_caption, 0, row, 1, 1);
    ui.backend_dd.set_hexpand(true);
    ui.backend_dd.set_halign(gtk4::Align::Start);
    grid.attach(&ui.backend_dd, 1, row, 1, 1);
    row += 1;

    let accept_caption = gtk4::Label::new(Some("Accept workloads from others"));
    accept_caption.set_xalign(0.0);
    grid.attach(&accept_caption, 0, row, 1, 1);
    ui.accept_switch.set_halign(gtk4::Align::Start);
    ui.accept_switch.set_valign(gtk4::Align::Center);
    grid.attach(&ui.accept_switch, 1, row, 1, 1);
    row += 1;

    // Honest isolation copy (§2.2): local-cpu runs job commands on this
    // machine with the user's privileges and NO sandbox. Never overstate
    // the isolation — see docs/CONSENT.md.
    let accept_hint = gtk4::Label::new(Some(
        "OFF by default, and off until you flip it. Turning this on makes this \
         machine visible to other agents, which can then send it jobs. Jobs run \
         through the selected backend: 'noop-cpu' simulates, 'local-cpu' executes \
         the job's commands on this machine with your user's privileges and NO \
         sandbox. Only enable this if you trust the workloads you will receive.",
    ));
    accept_hint.set_wrap(true);
    accept_hint.set_xalign(0.0);
    accept_hint.add_css_class("dim-label");
    grid.attach(&accept_hint, 0, row, 2, 1);
    row += 1;

    let hint = gtk4::Label::new(Some(
        "The endpoint must be reachable from the internet for agents to connect. \
         Paid mode only collects a price + payout address; settlement runs \
         off-chain in v0. 'local-cpu' runs job commands on this machine with \
         no isolation — only enable it for trusted workloads.",
    ));
    hint.set_wrap(true);
    hint.set_xalign(0.0);
    hint.add_css_class("dim-label");
    grid.attach(&hint, 0, row, 2, 1);
    row += 1;

    ui.error_label.add_css_class("error");
    ui.error_label.set_wrap(true);
    ui.error_label.set_xalign(0.0);
    ui.error_label.set_visible(false);
    grid.attach(&ui.error_label, 0, row, 2, 1);

    settings_page.append(&grid);

    // ---- Dashboard page ---------------------------------------------------
    let dashboard_page = gtk4::Box::new(gtk4::Orientation::Vertical, 12);
    dashboard_page.set_margin_top(24);
    dashboard_page.set_margin_bottom(24);
    dashboard_page.set_margin_start(24);
    dashboard_page.set_margin_end(24);

    // Dashboard grid cards — 2x2 layout.
    let dashboard_grid = gtk4::Grid::new();
    dashboard_grid.set_column_spacing(12);
    dashboard_grid.set_row_spacing(12);
    dashboard_grid.set_hexpand(true);

    dashboard_grid.attach(&status_card, 0, 0, 1, 1);
    dashboard_grid.attach(&nodeid_card, 1, 0, 1, 1);
    dashboard_grid.attach(&cpu_card, 0, 1, 1, 1);
    dashboard_grid.attach(&mem_card, 1, 1, 1, 1);
    dashboard_grid.attach(&last_job_card.0, 0, 2, 2, 1);

    dashboard_page.append(&dashboard_grid);

    // Settlement honesty (§3 of docs/CONSENT.md): who picks f, and the hard
    // limit of that power. Static copy, no on-chain call needed for v0.
    ui.settlement_label.set_wrap(true);
    ui.settlement_label.set_xalign(0.0);
    ui.settlement_label.add_css_class("dim-label");
    ui.settlement_label.set_text(
        "Settlement: when a paid job finishes, the completion fraction f is chosen by \
         the settlement authority pinned in the escrow program's Config at deploy. That \
         authority can set f, but it cannot redirect escrowed funds to itself. v0 settles \
         off-chain; on-chain pro-rata settlement lands with Module 4.",
    );
    dashboard_page.append(&ui.settlement_label);

    let log_caption = gtk4::Label::new(Some("Live log"));
    log_caption.set_xalign(0.0);
    log_caption.add_css_class("dim-label");
    dashboard_page.append(&log_caption);

    ui.log_view.set_editable(false);
    ui.log_view.set_cursor_visible(false);
    ui.log_view.set_wrap_mode(gtk4::WrapMode::WordChar);
    ui.log_view.set_monospace(true);

    let scroller = gtk4::ScrolledWindow::new();
    scroller.set_child(Some(&ui.log_view));
    scroller.set_vexpand(true);
    scroller.set_policy(gtk4::PolicyType::Automatic, gtk4::PolicyType::Automatic);
    dashboard_page.append(&scroller);

    // ---- Jobs page --------------------------------------------------------
    let jobs_page = gtk4::Box::new(gtk4::Orientation::Vertical, 12);
    jobs_page.set_margin_top(24);
    jobs_page.set_margin_bottom(24);
    jobs_page.set_margin_start(24);
    jobs_page.set_margin_end(24);

    // Summary bar — three metrics.
    let summary_bar = gtk4::Box::new(gtk4::Orientation::Horizontal, 24);
    summary_bar.set_halign(gtk4::Align::Start);
    summary_bar.append(&total_box);
    summary_bar.append(&earnings_box);
    summary_bar.append(&avgcpu_box);
    jobs_page.append(&summary_bar);

    // Jobs table — header row + scrollable list.
    let header_row = gtk4::Box::new(gtk4::Orientation::Horizontal, 0);
    header_row.add_css_class("job-table-header");
    for w in ["Status", "Job ID", "CPU", "Memory", "Earnings", "Time"] {
        let l = gtk4::Label::new(Some(w));
        l.set_xalign(0.0);
        l.set_hexpand(true);
        l.set_margin_start(8);
        l.set_margin_end(8);
        header_row.append(&l);
    }
    jobs_page.append(&header_row);

    let jobs_scroller = gtk4::ScrolledWindow::new();
    jobs_scroller.set_child(Some(&ui.jobs_list));
    jobs_scroller.set_vexpand(true);
    jobs_scroller.set_policy(gtk4::PolicyType::Automatic, gtk4::PolicyType::Automatic);
    jobs_page.append(&jobs_scroller);

    // ---- Notebook + window ----------------------------------------------
    let notebook = gtk4::Notebook::new();
    notebook.append_page(&settings_page, Some(&gtk4::Label::new(Some("Settings"))));
    notebook.append_page(&dashboard_page, Some(&gtk4::Label::new(Some("Dashboard"))));
    notebook.append_page(&jobs_page, Some(&gtk4::Label::new(Some("Jobs"))));

    let title = gtk4::Label::new(Some("Vtessera"));
    let header = gtk4::HeaderBar::new();
    header.set_title_widget(Some(&title));
    header.pack_start(&ui.start_btn);
    header.pack_start(&ui.stop_btn);

    let window = gtk4::ApplicationWindow::new(app);
    window.set_title(Some("Vtessera — sell or donate your compute"));
    window.set_default_size(860, 860);
    window.set_titlebar(Some(&header));
    window.set_child(Some(&notebook));

    // ---- Wiring ----------------------------------------------------------
    let state = Rc::new(NodeState {
        daemons: Rc::new(RefCell::new(None)),
        log_pending: log_pending.clone(),
    });

    ui.free_btn.connect_toggled({
        let ui = ui.clone();
        let state = state.clone();
        move |_| {
            ui.sync_mode_sensitivity();
            let running = state.daemons.borrow().is_some();
            if running {
                ui.log_line("Mode changed — restarting node to apply...");
                stop_node(&ui, &state);
                start_node(&ui, &state);
            }
        }
    });
    ui.paid_btn.connect_toggled({
        let ui = ui.clone();
        let state = state.clone();
        move |_| {
            ui.sync_mode_sensitivity();
            let running = state.daemons.borrow().is_some();
            if running {
                ui.log_line("Mode changed — restarting node to apply...");
                stop_node(&ui, &state);
                start_node(&ui, &state);
            }
        }
    });

    // Network dropdown: show/hide custom entry and handle mainnet confirmation.
    ui.network_dd.connect_selected_notify({
        let ui = ui.clone();
        move |dd| {
            ui.sync_network_sensitivity();
            if dd.selected() == 1 {
                let window = ui
                    .start_btn
                    .root()
                    .and_then(|r| r.downcast_ref::<gtk4::Window>().cloned());
                if let Some(win) = window {
                    show_mainnet_confirm(&ui, &win);
                }
            }
        }
    });

    // Local network toggle: show/hide marketplace and CIDR fields.
    ui.local_network_switch.connect_active_notify({
        let ui = ui.clone();
        let state = state.clone();
        move |_switch| {
            ui.sync_network_sensitivity();
            let running = state.daemons.borrow().is_some();
            if running {
                ui.log_line("Network scope changed — restarting node to apply...");
                stop_node(&ui, &state);
                start_node(&ui, &state);
            }
        }
    });

    // The second consent gate (§2.2) is a persisted, explicit switch. OFF by
    // default; changes persist immediately and only take effect on Start.
    ui.accept_switch.connect_active_notify({
        let ui = ui.clone();
        let state = state.clone();
        move |sw| {
            let on = sw.is_active();
            let prev = ui.settings.borrow().accept_workloads;
            if on != prev {
                let running = state.daemons.borrow().is_some();
                ui.log_line(if on {
                    "Accept workloads from others: ON — jobs run on this machine without a sandbox"
                } else {
                    "Accept workloads from others: OFF — no jobs accepted until re-enabled and Started"
                });
                ui.settings.borrow_mut().accept_workloads = on;
                let _ = ui.settings.borrow().save(&settings::settings_path());
                if running {
                    ui.log_line("Restarting node to apply change...");
                    stop_node(&ui, &state);
                    start_node(&ui, &state);
                }
            }
        }
    });

    ui.apply_settings(&initial);

    let state_for_start = state.clone();
    ui.start_btn.connect_clicked({
        let ui = ui.clone();
        move |_| start_node(&ui, &state_for_start)
    });

    let state_for_stop = state.clone();
    ui.stop_btn.connect_clicked({
        let ui = ui.clone();
        move |_| stop_node(&ui, &state_for_stop)
    });

    // Drain the shared log buffer into the log tab (worker threads push
    // lines; only the main thread touches widgets).
    glib::timeout_add_local(Duration::from_millis(200), {
        let log_rx = log_pending.clone();
        let ui = ui.clone();
        move || {
            let lines: Vec<String> = log_rx.lock().unwrap().drain(..).collect();
            for line in lines {
                ui.log_line(&line);
            }
            glib::ControlFlow::Continue
        }
    });

    // Periodic status refresh (receipt count, running state, dashboard metrics, jobs table).
    glib::timeout_add_local(Duration::from_secs(2), {
        let ui = ui.clone();
        let state = state.clone();
        move || {
            refresh_status(&ui, &state);
            refresh_dashboard(&ui);
            refresh_jobs_table(&ui);
            glib::ControlFlow::Continue
        }
    });

    // Kill children when the window closes.
    window.connect_close_request({
        let ui = ui.clone();
        let state = state.clone();
        move |_| {
            if let Some(mut d) = state.daemons.borrow_mut().take() {
                daemon::stop(&mut d, &|line| {
                    eprintln!("vtessera-gui: {line}");
                });
            }
            let _ = &ui;
            glib::Propagation::Proceed
        }
    });

    refresh_status(&ui, &state);

    // First-run consent gate (§2.1). Without recorded consent — or after a
    // copy bump (`CURRENT_CONSENT_VERSION`) — nothing is shown or started
    // until the user presses "Enable metering". "Not now" quits; there is no
    // silent resume.
    if settings::needs_consent(&initial) {
        show_consent_gate(app, &ui, &window);
    } else {
        window.present();
    }
}

/// The first-run consent window (docs/CONSENT.md §2.1).
fn show_consent_gate(app: &gtk4::Application, ui: &Rc<Ui>, main_window: &gtk4::ApplicationWindow) {
    let gate = gtk4::Window::new();
    gate.set_title(Some("Vtessera — your permission, first"));
    gate.set_modal(true);
    gate.set_transient_for(Some(main_window));
    gate.set_default_size(580, 500);

    let boxed = gtk4::Box::new(gtk4::Orientation::Vertical, 12);
    boxed.set_margin_top(24);
    boxed.set_margin_bottom(24);
    boxed.set_margin_start(24);
    boxed.set_margin_end(24);

    let heading = gtk4::Label::new(Some(
        "This machine is about to become a Vtessera compute node.",
    ));
    heading.set_xalign(0.0);
    heading.add_css_class("title-2");
    heading.set_wrap(true);
    boxed.append(&heading);

    let what = gtk4::Label::new(Some(
        "What Vtessera does with your permission:\n\
         \u{2022} samples this machine's CPU, memory, and disk usage and writes signed receipts \
         to a local state folder\n\
         \u{2022} can run compute jobs for other agents \u{2014} only after you separately turn on \
         \u{201C}Accept workloads\u{201D} in Settings\n\
         \u{2022} settles paid jobs on Solana (devnet in v0)\n\n\
         What it never does:\n\
         \u{2022} starts itself, or restarts after you stop it\n\
         \u{2022} runs programs on this machine without your permission\n\
         \u{2022} opens network sockets in v0 (metering alone)\n\n\
         You can stop everything at any time with one Stop button, and uninstall at any time \
         without leaving anything running.",
    ));
    what.set_wrap(true);
    what.set_xalign(0.0);
    what.set_selectable(true);
    boxed.append(&what);

    let not_now = gtk4::Button::with_label("Not now");
    not_now.add_css_class("destructive-action");
    let enable = gtk4::Button::with_label("Enable metering");
    enable.add_css_class("suggested-action");

    let actions = gtk4::Box::new(gtk4::Orientation::Horizontal, 8);
    actions.set_halign(gtk4::Align::End);
    actions.append(&not_now);
    actions.append(&enable);
    boxed.append(&actions);

    gate.set_child(Some(&boxed));

    not_now.connect_clicked({
        let gate = gate.clone();
        let app = app.clone();
        move |_| {
            gate.close();
            app.quit();
        }
    });

    enable.connect_clicked({
        let gate = gate.clone();
        let ui = ui.clone();
        let window = main_window.clone();
        move |_| {
            {
                let mut s = ui.settings.borrow_mut();
                s.metering_consent = true;
                s.consent_version = settings::CURRENT_CONSENT_VERSION;
            }
            if let Err(e) = ui.settings.borrow().save(&settings::settings_path()) {
                eprintln!("vtessera-gui: could not record consent ({e})");
            }
            ui.log_line(
                "Metering consent recorded. Nothing has started yet — press Start when ready.",
            );
            gate.close();
            window.present();
        }
    });

    gate.present();
}

/// Show a confirmation dialog when the user selects mainnet.
/// Reverts to devnet if they cancel.
fn show_mainnet_confirm(ui: &Ui, parent: &gtk4::Window) {
    let dialog = gtk4::AlertDialog::builder()
        .message("Switch to Solana Mainnet?")
        .detail(
            "Mainnet uses real SOL and real money. Settlement is irreversible. \
             Only switch if you understand the risks and have tested on devnet.",
        )
        .buttons(["Cancel", "Switch to Mainnet"])
        .build();

    let dd_weak = gtk4::prelude::ObjectExt::downgrade(&ui.network_dd);
    let sync_target = ui.network_custom_entry.clone();
    let sync_target2 = ui.local_network_switch.clone();
    let sync_target3 = ui.marketplace_url_entry.clone();
    let sync_target4 = ui.cidr_entry.clone();
    dialog.choose(
        Some(parent),
        None::<&gtk4::gio::Cancellable>,
        move |result| {
            // Index 0 = Cancel, Index 1 = Switch to Mainnet.
            let cancel = match result {
                Ok(idx) => idx != 1,
                Err(_) => true,
            };
            if cancel {
                if let Some(dd) = dd_weak.upgrade() {
                    dd.set_selected(0);
                }
                sync_target.set_visible(false);
                sync_target2.set_active(false);
                sync_target3.set_visible(true);
                sync_target4.set_visible(false);
            }
        },
    );
}

fn install_css() {
    let provider = gtk4::CssProvider::new();
    provider.load_from_string(
        "window { background-color: #0d1117; } \
         .error { color: @error_color; } \
         .dim-label { opacity: 0.7; } \
         .mode-segmented button { border-radius: 0; } \
         .mode-segmented button:checked { background: @theme_selected_bg_color; \
             color: @theme_selected_fg_color; } \
         .mode-segmented button:first-child { border-radius: 8px 0 0 8px; } \
         .mode-segmented button:last-child { border-radius: 0 8px 8px 0; } \
         .dashboard-card { background-color: #161b22; border-radius: 6px; \
             border: 1px solid #30363d; padding: 16px; } \
         .dashboard-card-title { color: #8b949e; font-size: 11px; \
             font-weight: 600; } \
         .dashboard-card-value { color: #e6edf3; font-size: 22px; \
             font-weight: 700; } \
         .dashboard-card-subtitle { color: #8b949e; font-size: 11px; } \
         .progress-track { background-color: #30363d; border-radius: 2px; \
             min-height: 4px; } \
         .progress-fill { border-radius: 2px; min-height: 4px; } \
         .progress-fill-cpu { background-color: #58a6ff; } \
         .progress-fill-mem { background-color: #d2a8ff; } \
         .status-dot { font-size: 14px; } \
         .status-off { color: #8b949e; } \
         .status-metering { color: #fbbf24; } \
         .status-active { color: #3fb950; } \
         .job-table-header { background-color: #161b22; color: #8b949e; \
             font-size: 11px; font-weight: 600; } \
         .job-table-row { border-bottom: 1px solid #30363d; } \
         .job-table-row:hover { background-color: #1c2128; } \
         .job-table-cell { color: #e6edf3; font-size: 12px; padding: 8px; } \
         .summary-metric-value { color: #e6edf3; font-size: 18px; \
             font-weight: 700; } \
         .summary-metric-label { color: #8b949e; font-size: 11px; } \
         .cpu-accent { color: #58a6ff; } \
         .mem-accent { color: #d2a8ff; } \
         .status-green { color: #3fb950; } \
         .earnings-gold { color: #fbbf24; } \
         .tab-label { color: #8b949e; }",
    );
    if let Some(display) = gtk4::gdk::Display::default() {
        gtk4::style_context_add_provider_for_display(
            &display,
            &provider,
            gtk4::STYLE_PROVIDER_PRIORITY_APPLICATION,
        );
    }
}

fn main() -> glib::ExitCode {
    let app = gtk4::Application::builder().application_id(APP_ID).build();
    app.connect_activate(build_ui);
    app.run()
}
