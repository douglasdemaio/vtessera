use std::fs;
use std::io;
use std::path::Path;

use crate::receipt::{signed_receipt_to_json, SignedReceipt};

/// Atomically write a signed receipt to the state directory.
pub fn write_signed_receipt(state_dir: &Path, sr: &SignedReceipt) -> io::Result<()> {
    fs::create_dir_all(state_dir)?;

    let json_bytes = signed_receipt_to_json(sr);

    let timestamp = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    let filename = format!("receipt_{timestamp}.json");
    let final_path = state_dir.join(&filename);

    let temp_path = state_dir.join(format!(".{filename}.tmp"));
    fs::write(&temp_path, &json_bytes)?;
    fs::rename(&temp_path, &final_path)?;

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::receipt::{Receipt, Totals};
    use crate::sign;
    use ed25519_dalek::SigningKey;
    use rand::rngs::OsRng;

    fn dummy_signed_receipt() -> SignedReceipt {
        let sk = SigningKey::generate(&mut OsRng);
        let receipt = Receipt {
            schema_ver: 1,
            node_id: "test-node".into(),
            window_start: 1000,
            window_end: 2000,
            samples_digest: [0x42; 32],
            totals: Totals {
                cpu_pct_avg: 50.0,
                mem_used_kb_avg: 2048,
                disk_free_kb_avg: 50000,
                sample_count: 5,
            },
        };
        sign::sign(&sk, &receipt)
    }

    #[test]
    fn test_write_signed_receipt() {
        let dir = std::env::temp_dir().join("vtessera_test_spool");
        let _ = fs::remove_dir_all(&dir);
        let sr = dummy_signed_receipt();
        write_signed_receipt(&dir, &sr).expect("should write receipt");
        let entries: Vec<_> = fs::read_dir(&dir).unwrap().collect();
        assert!(!entries.is_empty(), "should have written a file");
        let _ = fs::remove_dir_all(&dir);
    }
}
