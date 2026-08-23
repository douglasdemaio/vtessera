//! Minimal iroh client that connects to a vtessera node over QUIC.
//!
//! Usage:
//!   cargo run -p vtessera-transport --example iroh_client --features serve \
//!     -- --node-id <EndpointId> --request "GET /offer"
//!
//! The client creates an iroh endpoint, connects to the node by its
//! EndpointId through the relay network, sends an HTTP request over a
//! bi-directional QUIC stream, and prints the response.
//!
//! This proves the full agent→relay→node flow works over iroh.

use std::time::Duration;

use iroh::{EndpointId, SecretKey};
use vtessera_transport::iroh_sidecar::IrohEndpoint;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let args: Vec<String> = std::env::args().collect();
    let node_id_str = args
        .iter()
        .position(|a| a == "--node-id")
        .and_then(|i| args.get(i + 1))
        .expect("usage: --node-id <EndpointId> [--request \"GET /offer\"]");

    let request_line = args
        .iter()
        .position(|a| a == "--request")
        .and_then(|i| args.get(i + 1))
        .map(|s| s.as_str())
        .unwrap_or("GET /offer");

    // Parse the node ID (hex-encoded Ed25519 public key)
    let node_id_bytes: [u8; 32] = hex::decode(node_id_str)?
        .try_into()
        .map_err(|_| "node_id must be 32 bytes")?;
    let node_id = EndpointId::from_bytes(&node_id_bytes)?;

    // Create our own iroh endpoint with a fresh key
    let ep = IrohEndpoint::new(SecretKey::generate()).await?;
    eprintln!("client: endpoint online, node_id={}", ep.node_id());

    // Wait for relay registration
    tokio::time::sleep(Duration::from_secs(2)).await;

    // Build the endpoint address from just the node ID
    // iroh will resolve it through DNS/relay
    let endpoint_addr = iroh::EndpointAddr::new(node_id);

    // Connect to the node
    eprintln!("client: connecting to {node_id}...");
    let conn = ep.connect(endpoint_addr).await?;
    eprintln!("client: connected!");

    // Open a bi-directional stream
    let (mut send, mut recv) = conn.open_bi().await?;

    // Send the HTTP request
    let http_request = format!("{request_line} HTTP/1.1\r\nHost: vtessera\r\n\r\n");
    eprintln!("client: sending {request_line}");
    send.write_all(http_request.as_bytes()).await?;
    send.finish()?;

    // Read the response
    let response = recv.read_to_end(1024 * 1024).await?;
    let response_str = String::from_utf8_lossy(&response);
    println!("{response_str}");

    // Close the connection
    conn.close(0u32.into(), b"done");

    Ok(())
}
