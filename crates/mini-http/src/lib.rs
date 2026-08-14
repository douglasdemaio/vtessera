//! Vtessera — tiny hand-rolled HTTP/1.1 server primitives.
//!
//! One audited parser, shared by the agent-facing binaries so the inbound
//! surface stays reviewable: no tokio, no hyper, no axum. For direct
//! internet traffic, front these with something that does TLS termination
//! and request-size caps before this process sees a byte.
//!
//! Contract (matches what the previous node binary enforced):
//!
//! - `MAX_HEADER_BYTES` caps the whole request header section.
//! - `MAX_BODY_BYTES` caps a declared `content-length`.
//! - `READ_TIMEOUT` bounds idle/read and write stalls per connection.
//! - `serve` is thread-per-connection with a hard cap; overload is refused
//!   up front with **503** instead of exhausting threads.
//!
//! The parse layer is pure: `read_request_from` works on any `BufRead`
//! (unit-tested without sockets). `read_request`/`write_response` handle
//! the socket specifics.

#![forbid(unsafe_code)]

use std::io::{BufRead, BufReader, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

pub const MAX_HEADER_BYTES: usize = 16 * 1024;
pub const MAX_BODY_BYTES: usize = 1024 * 1024;
pub const READ_TIMEOUT: Duration = Duration::from_secs(15);
/// Bound on concurrently served connections. A swarm of idle sockets is
/// refused (503) rather than allowed to exhaust threads.
pub const MAX_CONNECTIONS: usize = 32;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Method {
    Get,
    Post,
    Delete,
    Other,
}

#[derive(Debug, Clone)]
pub struct Request {
    pub method: Method,
    pub path: String,
    /// Headers, normalised to lowercase keys.
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
}

#[derive(Debug, Clone)]
pub struct Response {
    pub status: u16,
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
}

impl Response {
    pub fn json(status: u16, body: String) -> Self {
        let body_bytes = body.into_bytes();
        Response {
            status,
            headers: vec![
                ("content-type".into(), "application/json".into()),
                ("content-length".into(), body_bytes.len().to_string()),
            ],
            body: body_bytes,
        }
    }

    pub fn text(status: u16, body: &str) -> Self {
        let body_bytes = body.as_bytes().to_vec();
        Response {
            status,
            headers: vec![
                ("content-type".into(), "text/plain; charset=utf-8".into()),
                ("content-length".into(), body_bytes.len().to_string()),
            ],
            body: body_bytes,
        }
    }
}

pub fn status_text(code: u16) -> &'static str {
    match code {
        200 => "OK",
        201 => "Created",
        202 => "Accepted",
        204 => "No Content",
        400 => "Bad Request",
        402 => "Payment Required",
        404 => "Not Found",
        501 => "Not Implemented",
        503 => "Service Unavailable",
        _ => "Status",
    }
}

/// Read one HTTP/1.1 request from a socket. Sets the read timeout first.
pub fn read_request(stream: &mut TcpStream) -> Result<Request, String> {
    stream
        .set_read_timeout(Some(READ_TIMEOUT))
        .map_err(|e| format!("set read timeout: {e}"))?;
    let mut reader = BufReader::new(stream.try_clone().map_err(|e| format!("clone: {e}"))?);
    read_request_from(&mut reader)
}

/// Parse one request from any buffered reader. Pure and unit-testable.
pub fn read_request_from<R: BufRead>(reader: &mut R) -> Result<Request, String> {
    let mut request_line = String::new();
    reader
        .read_line(&mut request_line)
        .map_err(|e| format!("read request line: {e}"))?;
    if request_line.is_empty() {
        return Err("empty request".into());
    }
    let mut parts = request_line.split_whitespace();
    let method = match parts.next() {
        Some("GET") => Method::Get,
        Some("POST") => Method::Post,
        Some("DELETE") => Method::Delete,
        Some(_) => Method::Other,
        None => return Err("missing method".into()),
    };
    let path = parts
        .next()
        .ok_or_else(|| "missing path".to_string())?
        .to_string();

    let mut headers: Vec<(String, String)> = Vec::new();
    let mut header_bytes = 0usize;
    let mut content_length = 0usize;
    loop {
        let mut line = String::new();
        let n = reader
            .read_line(&mut line)
            .map_err(|e| format!("read header line: {e}"))?;
        if n == 0 {
            break;
        }
        header_bytes += n;
        if header_bytes > MAX_HEADER_BYTES {
            return Err("header section too large".into());
        }
        if line == "\r\n" || line == "\n" {
            break;
        }
        let trimmed = line.trim_end();
        if let Some(idx) = trimmed.find(':') {
            let (k, v) = trimmed.split_at(idx);
            let key = k.trim().to_ascii_lowercase();
            let val = v[1..].trim().to_string();
            if key == "content-length" {
                content_length = val.parse().unwrap_or(0);
            }
            headers.push((key, val));
        }
    }
    if content_length > MAX_BODY_BYTES {
        return Err("body too large".into());
    }
    let mut body = vec![0u8; content_length];
    if content_length > 0 {
        reader
            .read_exact(&mut body)
            .map_err(|e| format!("read body: {e}"))?;
    }
    Ok(Request {
        method,
        path,
        headers,
        body,
    })
}

/// Write a full response, then close the connection.
pub fn write_response(stream: &mut TcpStream, resp: &Response) -> std::io::Result<()> {
    stream.set_write_timeout(Some(READ_TIMEOUT))?;
    let mut head = format!("HTTP/1.1 {} {}\r\n", resp.status, status_text(resp.status));
    for (k, v) in &resp.headers {
        head.push_str(&format!("{k}: {v}\r\n"));
    }
    head.push_str("connection: close\r\n\r\n");
    stream.write_all(head.as_bytes())?;
    stream.write_all(&resp.body)
}

/// Write a bare status response (used for 400/503 paths with no handler).
pub fn write_status(stream: &mut TcpStream, code: u16, msg: &str) -> std::io::Result<()> {
    stream.set_write_timeout(Some(READ_TIMEOUT))?;
    let body = msg.as_bytes();
    let resp = format!(
        "HTTP/1.1 {code} {text}\r\ncontent-type: text/plain; charset=utf-8\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
        body.len(),
        text = status_text(code),
    );
    stream.write_all(resp.as_bytes())?;
    stream.write_all(body)
}

/// Serve `listener` forever, thread-per-connection, capped at
/// `max_connections`. The handler is pure: `Request` in, `Response` out.
pub fn serve<F>(listener: TcpListener, handler: F, max_connections: usize)
where
    F: Fn(Request) -> Response + Clone + Send + Sync + 'static,
{
    let active = Arc::new(AtomicUsize::new(0));
    for incoming in listener.incoming() {
        let mut stream = match incoming {
            Ok(s) => s,
            Err(e) => {
                eprintln!("accept error: {e}");
                continue;
            }
        };

        if active.load(Ordering::Relaxed) >= max_connections {
            if let Err(e) = write_status(&mut stream, 503, "busy: too many concurrent connections")
            {
                eprintln!("refusing overloaded connection: {e}");
            }
            continue;
        }
        active.fetch_add(1, Ordering::Relaxed);

        let handler = handler.clone();
        let active = active.clone();
        std::thread::spawn(move || {
            if let Err(e) = handle_one(&mut stream, &handler) {
                eprintln!("connection error: {e}");
            }
            active.fetch_sub(1, Ordering::Relaxed);
        });
    }
}

fn handle_one<F>(stream: &mut TcpStream, handler: &F) -> std::io::Result<()>
where
    F: Fn(Request) -> Response,
{
    let request = match read_request(stream) {
        Ok(r) => r,
        Err(why) => {
            write_status(stream, 400, &why)?;
            return Ok(());
        }
    };
    let response = handler(request);
    write_response(stream, &response)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Read;

    fn parse(s: &str) -> Request {
        let mut reader = std::io::Cursor::new(s.as_bytes());
        read_request_from(&mut reader).unwrap()
    }

    #[test]
    fn parses_get_with_headers() {
        let req = parse("GET /offer HTTP/1.1\r\naccept: application/json\r\n\r\n");
        assert_eq!(req.method, Method::Get);
        assert_eq!(req.path, "/offer");
        assert_eq!(req.headers[0], ("accept".into(), "application/json".into()));
        assert!(req.body.is_empty());
    }

    #[test]
    fn parses_post_body_via_content_length() {
        let req = parse("POST /jobs HTTP/1.1\r\ncontent-length: 5\r\n\r\nhello");
        assert_eq!(req.method, Method::Post);
        assert_eq!(req.body, b"hello");
    }

    #[test]
    fn missing_path_is_an_error() {
        let mut reader = std::io::Cursor::new(b"GET\r\n\r\n".as_slice());
        assert!(read_request_from(&mut reader).is_err());
    }

    #[test]
    fn empty_request_is_an_error() {
        let mut reader = std::io::Cursor::new(b"".as_slice());
        assert!(read_request_from(&mut reader).is_err());
    }

    #[test]
    fn oversized_header_section_is_rejected() {
        let mut head = String::from("GET / HTTP/1.1\r\n");
        for i in 0..(MAX_HEADER_BYTES / 8) {
            head.push_str(&format!("x-{i}: value\r\n"));
        }
        head.push_str("\r\n");
        let mut reader = std::io::Cursor::new(head.as_bytes());
        assert!(read_request_from(&mut reader).is_err());
    }

    #[test]
    fn oversized_declared_body_is_rejected_without_reading_it() {
        let mut reader = std::io::Cursor::new(
            b"POST /jobs HTTP/1.1\r\ncontent-length: 99999999\r\n\r\n".as_slice(),
        );
        assert!(read_request_from(&mut reader).is_err());
    }

    #[test]
    fn status_text_covers_used_codes() {
        assert_eq!(status_text(200), "OK");
        assert_eq!(status_text(402), "Payment Required");
        assert_eq!(status_text(501), "Not Implemented");
        assert_eq!(status_text(503), "Service Unavailable");
    }

    #[test]
    fn json_response_sets_content_type_and_length() {
        let r = Response::json(200, "{}".into());
        assert_eq!(r.status, 200);
        assert!(r
            .headers
            .iter()
            .any(|(k, v)| k == "content-type" && v == "application/json"));
        assert!(r
            .headers
            .iter()
            .any(|(k, v)| k == "content-length" && v == "2"));
    }

    #[test]
    fn serve_round_trip_over_a_real_socket() {
        use std::net::TcpStream;
        use std::time::Duration;

        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();

        let handler = |req: Request| {
            assert_eq!(req.path, "/healthz");
            Response::text(200, "ok")
        };
        std::thread::spawn(move || serve(listener, handler, 4));

        std::thread::sleep(Duration::from_millis(20));
        let mut stream = TcpStream::connect(addr).unwrap();
        stream.write_all(b"GET /healthz HTTP/1.1\r\n\r\n").unwrap();
        let mut response = String::new();
        let mut reader = BufReader::new(stream.try_clone().unwrap());
        reader
            .read_to_string(&mut response)
            .expect("server should close after response");
        assert!(response.starts_with("HTTP/1.1 200 OK\r\n"), "{response}");
        assert!(response.ends_with("ok"), "{response}");
    }
}
