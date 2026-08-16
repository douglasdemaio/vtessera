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
    network_entry: gtk4::Entry,
    interval_spin: gtk4::SpinButton,
    backend_dd: gtk4::DropDown,
    /// "Accept workloads from others" — the second consent gate (§2.2 of
    /// `docs/CONSENT.md`). OFF by default; off until explicitly enabled.
    accept_switch: gtk4::Switch,
    error_label: gtk4::Label,
    status_label: gtk4::Label,
    node_id_label: gtk4::Label,
    mode_label: gtk4::Label,
    receipts_label: gtk4::Label,
    settlement_label: gtk4::Label,
    jobs_view: gtk4::TextView,
    log_view: gtk4::TextView,
    start_btn: gtk4::Button,
    stop_btn: gtk4::Button,
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
        let settings = Settings {
            mode: mode.into(),
            currency: currency.into(),
            price_per_cpu_hour: price,
            payout_id: self.payout_entry.text().trim().to_string(),
            port,
            endpoint: self.endpoint_entry.text().trim().to_string(),
            escrow_account: self.escrow_entry.text().trim().to_string(),
            network: self.network_entry.text().trim().to_string(),
            sample_interval_secs: self.interval_spin.value() as u64,
            backend: backend.into(),
            metering_consent: self.settings.borrow().metering_consent,
            accept_workloads: self.accept_switch.is_active(),
            consent_version: self.settings.borrow().consent_version,
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
        self.endpoint_entry.set_text(&s.endpoint);
        self.escrow_entry.set_text(&s.escrow_account);
        self.network_entry.set_text(&s.network);
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

fn count_receipts() -> usize {
    std::fs::read_dir(settings::state_dir())
        .map(|rd| {
            rd.flatten()
                .filter(|e| e.file_name().to_string_lossy().starts_with("receipt_"))
                .count()
        })
        .unwrap_or(0)
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
    ui.status_label.set_text(current_state(state));

    let node_id = current_node_id().unwrap_or_else(|| "—".into());
    ui.node_id_label.set_text(&node_id);

    let s = ui.settings.borrow();
    let mode = if s.is_free() {
        "free — donating compute".into()
    } else {
        format!(
            "paid — {} {}/CPU-hour, → {}",
            s.currency.to_uppercase(),
            format_price(s.price_per_cpu_hour),
            if s.payout_id.is_empty() {
                "no payout address".to_string()
            } else {
                s.payout_id.clone()
            }
        )
    };
    ui.mode_label.set_text(&mode);

    ui.receipts_label
        .set_text(&format!("{} receipt files", count_receipts()));
    refresh_jobs(ui);
}

/// Enumerate the signed per-job receipts the node has written under
/// `<state-dir>/job-receipts/` (newest first) — the legible activity record
/// the status page promises (§1.5 / §2.3).
fn refresh_jobs(ui: &Ui) {
    use std::time::SystemTime;

    let dir = settings::state_dir().join("job-receipts");
    let now = SystemTime::now();
    let mut jobs: Vec<(u64, String)> = match std::fs::read_dir(&dir) {
        Ok(rd) => rd
            .flatten()
            .map(|e| e.path())
            .filter(|p| p.extension().is_some_and(|x| x == "json"))
            .filter_map(|p| {
                let age = std::fs::metadata(&p)
                    .and_then(|m| m.modified())
                    .ok()
                    .and_then(|t| now.duration_since(t).ok())
                    .map(|d| d.as_secs())
                    .unwrap_or(u64::MAX);
                Some((age, p.file_name()?.to_string_lossy().into_owned()))
            })
            .collect(),
        Err(_) => Vec::new(),
    };
    jobs.sort();
    let body = if jobs.is_empty() {
        "No jobs yet — per-job receipts appear here when the node completes work.".to_string()
    } else {
        jobs.into_iter()
            .take(50)
            .map(|(age, name)| {
                let stamp = if age == u64::MAX {
                    "unknown time".to_string()
                } else if age < 60 {
                    format!("{age}s ago")
                } else if age < 3600 {
                    format!("{}m ago", age / 60)
                } else {
                    format!("{}h ago", age / 3600)
                };
                format!("{stamp}  {name}")
            })
            .collect::<Vec<_>>()
            .join("\n")
    };
    let buffer = ui.jobs_view.buffer();
    buffer.set_text(&body);
}

fn start_node(ui: &Ui, state: &NodeState) {
    let settings = match ui.read_settings() {
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
    if accepting {
        let offer_json = offer::build_offer_json(&settings, &key);
        if let Err(e) = std::fs::write(settings::offer_path(), &offer_json) {
            ui.set_error(&e.to_string());
            return;
        }
    }

    let node_id = vtessera_offer::derive_node_id(&key.verifying_key().to_bytes());
    // Bind loopback to match the advertised endpoint (default
    // http://127.0.0.1:8402): the node has no TLS/auth and must not sit on
    // a routable interface. The node binary's `--bind` stays configurable.
    let bind = format!("127.0.0.1:{}", settings.port);

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

    let log_pending: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));

    let initial = Settings::load_or_default(&settings::settings_path());
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
        network_entry: gtk4::Entry::new(),
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
        status_label: gtk4::Label::new(None),
        node_id_label: gtk4::Label::new(None),
        mode_label: gtk4::Label::new(None),
        receipts_label: gtk4::Label::new(None),
        settlement_label: gtk4::Label::new(None),
        jobs_view: gtk4::TextView::new(),
        log_view: gtk4::TextView::new(),
        start_btn: gtk4::Button::with_label("Start"),
        stop_btn: gtk4::Button::with_label("Stop"),
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
    ui.network_entry.set_hexpand(true);
    grid.attach(&ui.network_entry, 1, row, 1, 1);
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

    // ---- Status page -----------------------------------------------------
    let status_page = gtk4::Box::new(gtk4::Orientation::Vertical, 12);
    status_page.set_margin_top(24);
    status_page.set_margin_bottom(24);
    status_page.set_margin_start(24);
    status_page.set_margin_end(24);

    let status_grid = gtk4::Grid::new();
    status_grid.set_column_spacing(12);
    status_grid.set_row_spacing(8);
    status_grid.set_halign(gtk4::Align::Start);

    for (srow, (caption, value)) in [
        ("Status", &ui.status_label),
        ("Node ID", &ui.node_id_label),
        ("Mode", &ui.mode_label),
        ("Receipts", &ui.receipts_label),
    ]
    .into_iter()
    .enumerate()
    {
        let c = gtk4::Label::new(Some(caption));
        c.set_xalign(0.0);
        c.add_css_class("dim-label");
        status_grid.attach(&c, 0, srow as i32, 1, 1);
        value.set_xalign(0.0);
        value.set_selectable(true);
        status_grid.attach(value, 1, srow as i32, 1, 1);
    }
    status_page.append(&status_grid);

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
    status_page.append(&ui.settlement_label);

    let jobs_caption = gtk4::Label::new(Some("Recent jobs"));
    jobs_caption.set_xalign(0.0);
    jobs_caption.add_css_class("dim-label");
    status_page.append(&jobs_caption);

    ui.jobs_view.set_editable(false);
    ui.jobs_view.set_cursor_visible(false);
    ui.jobs_view.set_wrap_mode(gtk4::WrapMode::None);
    ui.jobs_view.set_monospace(true);
    ui.jobs_view.set_height_request(90);
    let jobs_scroller = gtk4::ScrolledWindow::new();
    jobs_scroller.set_child(Some(&ui.jobs_view));
    jobs_scroller.set_policy(gtk4::PolicyType::Automatic, gtk4::PolicyType::Automatic);
    status_page.append(&jobs_scroller);

    let log_caption = gtk4::Label::new(Some("Live log"));
    log_caption.set_xalign(0.0);
    log_caption.add_css_class("dim-label");
    status_page.append(&log_caption);

    ui.log_view.set_editable(false);
    ui.log_view.set_cursor_visible(false);
    ui.log_view.set_wrap_mode(gtk4::WrapMode::WordChar);
    ui.log_view.set_monospace(true);

    let scroller = gtk4::ScrolledWindow::new();
    scroller.set_child(Some(&ui.log_view));
    scroller.set_vexpand(true);
    scroller.set_policy(gtk4::PolicyType::Automatic, gtk4::PolicyType::Automatic);
    status_page.append(&scroller);

    // ---- Notebook + window ----------------------------------------------
    let notebook = gtk4::Notebook::new();
    notebook.append_page(&settings_page, Some(&gtk4::Label::new(Some("Settings"))));
    notebook.append_page(&status_page, Some(&gtk4::Label::new(Some("Status"))));

    let title = gtk4::Label::new(Some("Vtessera"));
    let header = gtk4::HeaderBar::new();
    header.set_title_widget(Some(&title));
    header.pack_start(&ui.start_btn);
    header.pack_start(&ui.stop_btn);

    let window = gtk4::ApplicationWindow::new(app);
    window.set_title(Some("Vtessera — sell or donate your compute"));
    window.set_default_size(780, 720);
    window.set_titlebar(Some(&header));
    window.set_child(Some(&notebook));

    // ---- Wiring ----------------------------------------------------------
    ui.free_btn.connect_toggled({
        let ui = ui.clone();
        move |_| ui.sync_mode_sensitivity()
    });
    ui.paid_btn.connect_toggled({
        let ui = ui.clone();
        move |_| ui.sync_mode_sensitivity()
    });

    let state = Rc::new(NodeState {
        daemons: Rc::new(RefCell::new(None)),
        log_pending: log_pending.clone(),
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
                if running {
                    ui.log_line(
                        "Accept workloads: change takes effect on the next Start (Stop then Start)",
                    );
                }
                ui.log_line(if on {
                    "Accept workloads from others: ON — jobs run on this machine without a sandbox"
                } else {
                    "Accept workloads from others: OFF — no jobs accepted until re-enabled and Started"
                });
                ui.settings.borrow_mut().accept_workloads = on;
                let _ = ui.settings.borrow().save(&settings::settings_path());
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

    // Periodic status refresh (receipt count, running state).
    glib::timeout_add_local(Duration::from_secs(2), {
        let ui = ui.clone();
        let state = state.clone();
        move || {
            refresh_status(&ui, &state);
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

fn install_css() {
    let provider = gtk4::CssProvider::new();
    provider.load_from_string(
        ".error { color: @error_color; } \
         .dim-label { opacity: 0.7; } \
         .mode-segmented button { border-radius: 0; } \
         .mode-segmented button:checked { background: @theme_selected_bg_color; \
             color: @theme_selected_fg_color; } \
         .mode-segmented button:first-child { border-radius: 8px 0 0 8px; } \
         .mode-segmented button:last-child { border-radius: 0 8px 8px 0; }",
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
