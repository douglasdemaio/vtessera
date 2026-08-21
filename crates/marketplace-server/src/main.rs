use axum::extract::{Json, Query, State};
use axum::http::StatusCode;
use axum::routing::{get, post};
use axum::Router;
use serde::Deserialize;
use std::sync::Arc;
use tokio::net::TcpListener;

mod config;
mod receipt_store;

use config::ServerConfig;
use receipt_store::{KeyRegistry, ReceiptStore, SignedReceipt};

/// Shared application state.
struct AppState {
    store: ReceiptStore,
    registry: KeyRegistry,
}

#[derive(Deserialize)]
struct ReceiptQuery {
    node_id: Option<String>,
    since: Option<u64>,
}

/// POST /api/v1/receipts — submit a signed receipt.
async fn submit_receipt(
    State(state): State<Arc<AppState>>,
    Json(sr): Json<SignedReceipt>,
) -> Result<(StatusCode, Json<serde_json::Value>), (StatusCode, Json<serde_json::Value>)> {
    match state.store.store(&sr, &state.registry) {
        Ok(id) => Ok((
            StatusCode::CREATED,
            Json(serde_json::json!({ "status": "stored", "id": id })),
        )),
        Err(receipt_store::StoreError::UnknownKey(key)) => Err((
            StatusCode::FORBIDDEN,
            Json(serde_json::json!({ "error": format!("unknown key: {key}") })),
        )),
        Err(receipt_store::StoreError::Duplicate(dedup)) => Err((
            StatusCode::CONFLICT,
            Json(serde_json::json!({ "error": format!("duplicate receipt: {dedup}") })),
        )),
        Err(receipt_store::StoreError::Io(e)) => Err((
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(serde_json::json!({ "error": e.to_string() })),
        )),
    }
}

/// GET /api/v1/receipts — list stored receipts.
async fn list_receipts(
    State(state): State<Arc<AppState>>,
    Query(params): Query<ReceiptQuery>,
) -> Json<Vec<SignedReceipt>> {
    let node_id = params.node_id.as_deref();
    let since = params.since;
    Json(state.store.list(node_id, since))
}

/// GET /api/v1/health — health check.
async fn health() -> Json<serde_json::Value> {
    Json(serde_json::json!({ "status": "ok" }))
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let args: Vec<String> = std::env::args().collect();
    let config_path = args
        .get(1)
        .map(|s| s.as_str())
        .unwrap_or("marketplace-server.toml");

    let config = ServerConfig::load(config_path)?;
    let registry = KeyRegistry::load(&config.key_registry_path)?;
    let store = ReceiptStore::new(&config.storage_path);

    let state = Arc::new(AppState { store, registry });

    let app = Router::new()
        .route("/api/v1/receipts", post(submit_receipt).get(list_receipts))
        .route("/api/v1/health", get(health))
        .with_state(state);

    let listener = TcpListener::bind(&config.listen_addr).await?;
    println!("marketplace-server listening on {}", config.listen_addr);
    axum::serve(listener, app).await?;

    Ok(())
}
