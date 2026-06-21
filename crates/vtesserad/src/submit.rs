/// Outbound receipt submission (only compiled with `--features submit`).
use crate::receipt::{signed_receipt_to_json, SignedReceipt};

#[derive(Debug)]
pub enum SubmitError {
    Http(String),
}

impl std::fmt::Display for SubmitError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            SubmitError::Http(e) => write!(f, "HTTP error: {e}"),
        }
    }
}

impl std::error::Error for SubmitError {}

/// POST a signed receipt to the configured endpoint.
pub fn submit_receipt(endpoint: &str, sr: &SignedReceipt) -> Result<(), SubmitError> {
    let body = signed_receipt_to_json(sr);

    let response = ureq::post(endpoint)
        .header("Content-Type", "application/json")
        .send(&body[..])
        .map_err(|e| SubmitError::Http(e.to_string()))?;

    let status = response.status().as_u16();
    if status != 200 && status != 201 && status != 202 {
        return Err(SubmitError::Http(format!("unexpected status: {status}")));
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::receipt::{Receipt, Totals};
    use crate::sign;
    use ed25519_dalek::SigningKey;
    use rand::rngs::OsRng;

    #[test]
    fn test_submit_endpoint_error() {
        let sk = SigningKey::generate(&mut OsRng);
        let receipt = Receipt {
            schema_ver: 1,
            node_id: "test".into(),
            payout_id: "test-payout".into(),
            window_start: 0,
            window_end: 1,
            samples_digest: [0; 32],
            totals: Totals {
                cpu_pct_avg: 0.0,
                mem_used_kb_avg: 0,
                disk_free_kb_avg: 0,
                sample_count: 0,
            },
        };
        let sr = sign::sign(&sk, &receipt);
        let result = submit_receipt("http://127.0.0.1:1/nonexistent", &sr);
        assert!(result.is_err(), "should fail against unreachable endpoint");
    }
}
