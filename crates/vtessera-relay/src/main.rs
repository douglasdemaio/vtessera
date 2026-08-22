use std::collections::HashMap;
use std::io::{BufRead, BufReader, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use vtessera_mini_http::{serve, Method, Request, Response};

struct NodeConnection {
    stream: TcpStream,
    last_heartbeat: Instant,
}

struct RelayState {
    nodes: Mutex<HashMap<String, NodeConnection>>,
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let mut node_port: u16 = 8411;
    let mut agent_port: u16 = 8410;

    let mut i = 1;
    while i < args.len() {
        match args[i].as_str() {
            "--node-port" => {
                i += 1;
                node_port = args[i].parse().expect("invalid --node-port");
            }
            "--agent-port" => {
                i += 1;
                agent_port = args[i].parse().expect("invalid --agent-port");
            }
            "--help" => {
                eprintln!("vtessera-relay --node-port 8411 --agent-port 8410");
                std::process::exit(0);
            }
            other => {
                eprintln!("unknown arg: {other}");
                std::process::exit(1);
            }
        }
        i += 1;
    }

    let state = Arc::new(RelayState {
        nodes: Mutex::new(HashMap::new()),
    });

    eprintln!("vtessera-relay: node port {node_port}, agent port {agent_port}");

    let node_listener =
        TcpListener::bind(("0.0.0.0", node_port)).expect("failed to bind node port");
    let agent_listener =
        TcpListener::bind(("0.0.0.0", agent_port)).expect("failed to bind agent port");

    let state_clone = state.clone();
    std::thread::spawn(move || accept_nodes(node_listener, state_clone));

    let handler = move |req: Request| dispatch(&state, req);
    serve(agent_listener, handler, 128);
}

fn accept_nodes(listener: TcpListener, state: Arc<RelayState>) {
    for incoming in listener.incoming() {
        match incoming {
            Ok(stream) => {
                let state = state.clone();
                std::thread::spawn(move || handle_node_connection(stream, state));
            }
            Err(e) => {
                eprintln!("node accept error: {e}");
            }
        }
    }
}

fn handle_node_connection(stream: TcpStream, state: Arc<RelayState>) {
    stream.set_read_timeout(Some(Duration::from_secs(90))).ok();

    let mut reader = BufReader::new(stream.try_clone().expect("clone stream"));
    let mut line = String::new();

    line.clear();
    if reader.read_line(&mut line).unwrap_or(0) == 0 {
        eprintln!("node disconnected before REGISTER");
        return;
    }
    let node_id_str = line.trim();
    let node_id = match node_id_str.strip_prefix("REGISTER ") {
        Some(id) => id.to_string(),
        None => {
            eprintln!("node sent invalid first line: {line}");
            return;
        }
    };

    eprintln!("node registered: {node_id}");

    {
        let mut nodes = state.nodes.lock().unwrap();
        nodes.insert(
            node_id.clone(),
            NodeConnection {
                stream: stream.try_clone().expect("clone stream"),
                last_heartbeat: Instant::now(),
            },
        );
    }

    loop {
        line.clear();
        match reader.read_line(&mut line) {
            Ok(0) => break,
            Ok(_) => {}
            Err(e) => {
                eprintln!("node {node_id} read error: {e}");
                break;
            }
        }
        let trimmed = line.trim();
        if trimmed == "HEARTBEAT" {
            let mut nodes = state.nodes.lock().unwrap();
            if let Some(nc) = nodes.get_mut(&node_id) {
                nc.last_heartbeat = Instant::now();
                let _ = nc.stream.write_all(b"HEARTBEAT_ACK\n");
            }
        }
    }

    eprintln!("node {node_id} disconnected");
    let mut nodes = state.nodes.lock().unwrap();
    nodes.remove(&node_id);
}

fn dispatch(state: &Arc<RelayState>, req: Request) -> Response {
    match (req.method, req.path.as_str()) {
        (Method::Get, "/healthz") => Response::text(200, "ok"),
        (Method::Get, "/nodes") => handle_list_nodes(state),
        (Method::Get, p) if p.starts_with("/nodes/") => {
            let node_id = &p[7..];
            handle_node_info(state, node_id)
        }
        (Method::Post, p) if p.starts_with("/nodes/") && p.ends_with("/proxy") => {
            let rest = &p[7..];
            let node_id = match rest.strip_suffix("/proxy") {
                Some(id) => id.to_string(),
                None => return Response::text(400, "invalid path"),
            };
            handle_proxy(state, &node_id, req)
        }
        _ => Response::text(404, "not found"),
    }
}

fn handle_list_nodes(state: &Arc<RelayState>) -> Response {
    let nodes = state.nodes.lock().unwrap();
    let ids: Vec<&str> = nodes.keys().map(|s| s.as_str()).collect();
    let body = serde_json::json!({
        "count": ids.len(),
        "nodes": ids,
    });
    Response::json(200, body.to_string())
}

fn handle_node_info(state: &Arc<RelayState>, node_id: &str) -> Response {
    let nodes = state.nodes.lock().unwrap();
    match nodes.get(node_id) {
        Some(nc) => {
            let body = serde_json::json!({
                "node_id": node_id,
                "connected": true,
                "last_heartbeat_secs_ago": nc.last_heartbeat.elapsed().as_secs(),
            });
            Response::json(200, body.to_string())
        }
        None => Response::json(
            404,
            serde_json::json!({"error": "node not connected"}).to_string(),
        ),
    }
}

fn handle_proxy(state: &Arc<RelayState>, node_id: &str, req: Request) -> Response {
    let node_stream = {
        let nodes = state.nodes.lock().unwrap();
        match nodes.get(node_id) {
            Some(nc) => nc.stream.try_clone().expect("clone stream"),
            None => {
                return Response::json(
                    404,
                    serde_json::json!({"error": "node not connected"}).to_string(),
                );
            }
        }
    };

    let inner: serde_json::Value = match serde_json::from_slice(&req.body) {
        Ok(v) => v,
        Err(e) => {
            return Response::json(
                400,
                serde_json::json!({"error": format!("invalid inner request: {e}")}).to_string(),
            );
        }
    };

    let method = inner["method"].as_str().unwrap_or("GET");
    let path = inner["path"].as_str().unwrap_or("/");
    let inner_headers: Vec<(String, String)> = inner["headers"]
        .as_array()
        .map(|arr| {
            arr.iter()
                .filter_map(|pair| {
                    let a = pair.as_array()?;
                    if a.len() >= 2 {
                        Some((a[0].as_str()?.to_string(), a[1].as_str()?.to_string()))
                    } else {
                        None
                    }
                })
                .collect()
        })
        .unwrap_or_default();

    let inner_body_b64 = inner["body"].as_str().unwrap_or("");
    let inner_body = match base64_decode(inner_body_b64) {
        Some(b) => b,
        None => return Response::json(400, r#"{"error":"invalid base64 body"}"#.into()),
    };

    // Serialize the inner request as JSON for the node
    let relay_req = serde_json::json!({
        "method": method,
        "path": path,
        "headers": inner_headers,
        "body_len": inner_body.len(),
    });

    let mut stream = node_stream;
    stream.set_read_timeout(Some(Duration::from_secs(30))).ok();
    stream.set_write_timeout(Some(Duration::from_secs(10))).ok();

    // Send REQUEST <json>\n followed by raw body bytes
    let header_line = format!("REQUEST {}\n", relay_req);
    if stream.write_all(header_line.as_bytes()).is_err() {
        return Response::json(502, r#"{"error":"failed to write to node"}"#.into());
    }
    if !inner_body.is_empty() && stream.write_all(&inner_body).is_err() {
        return Response::json(502, r#"{"error":"failed to write body to node"}"#.into());
    }

    // Read RESPONSE <json>\n from node
    let mut resp_reader = BufReader::new(stream);
    let mut resp_line = String::new();
    match resp_reader.read_line(&mut resp_line) {
        Ok(0) => {
            return Response::json(502, r#"{"error":"node closed connection"}"#.into());
        }
        Ok(_) => {}
        Err(e) => {
            return Response::json(
                502,
                serde_json::json!({"error": format!("read response: {e}")}).to_string(),
            );
        }
    }

    let resp_trimmed = resp_line.trim();
    let resp_json: serde_json::Value = match resp_trimmed.strip_prefix("RESPONSE ") {
        Some(j) => match serde_json::from_str(j) {
            Ok(v) => v,
            Err(e) => {
                return Response::json(
                    502,
                    serde_json::json!({"error": format!("invalid response json: {e}")}).to_string(),
                );
            }
        },
        None => {
            return Response::json(
                502,
                serde_json::json!({"error": format!("unexpected node response: {resp_trimmed}")})
                    .to_string(),
            );
        }
    };

    let status = resp_json["status"].as_u64().unwrap_or(500) as u16;
    let resp_body_b64 = resp_json["body"].as_str().unwrap_or("");
    let resp_body = base64_decode(resp_body_b64).unwrap_or_default();

    Response {
        status,
        headers: vec![
            ("content-type".into(), "application/json".into()),
            ("content-length".into(), resp_body.len().to_string()),
        ],
        body: resp_body,
    }
}

fn base64_decode(s: &str) -> Option<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(s).ok()
}
