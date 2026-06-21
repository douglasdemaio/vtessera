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

/// Prune `receipt_*.json` files in `state_dir` to keep at most `keep`
/// of the most recent ones. The filename layout
/// (`receipt_<unix_ns>.json`) is monotonic, so we can sort on the name
/// without stat()ing every file. No-op if `keep == 0` (treated as
/// "unlimited" — the daemon's config layer is responsible for not
/// passing 0 by accident; `Option<usize>` carries the "unlimited"
/// signal).
///
/// Only `.tmp` files matching our own atomic-write pattern and finalized
/// `receipt_*.json` files are considered. Anything else (operator
/// scratch, archived tarballs, etc.) is ignored — the rotator never
/// deletes files it doesn't recognize as receipts.
pub fn rotate(state_dir: &Path, keep: usize) -> io::Result<usize> {
    if keep == 0 {
        return Ok(0);
    }
    let dir = match fs::read_dir(state_dir) {
        Ok(d) => d,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(0),
        Err(e) => return Err(e),
    };

    let mut receipts: Vec<String> = Vec::new();
    for entry in dir {
        let entry = entry?;
        let name = entry.file_name().to_string_lossy().into_owned();
        if name.starts_with("receipt_") && name.ends_with(".json") {
            receipts.push(name);
        }
    }
    // Filename sort is timestamp sort — `receipt_<unix_ns>.json` is
    // monotonic.
    receipts.sort();

    if receipts.len() <= keep {
        return Ok(0);
    }
    let drop_count = receipts.len() - keep;
    let mut removed = 0;
    for name in &receipts[..drop_count] {
        let path = state_dir.join(name);
        match fs::remove_file(&path) {
            Ok(()) => removed += 1,
            Err(e) if e.kind() == io::ErrorKind::NotFound => {
                // Another process beat us to it; not an error.
            }
            Err(e) => return Err(e),
        }
    }
    Ok(removed)
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
            payout_id: "test-payout".into(),
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

    #[test]
    fn rotate_drops_oldest_beyond_keep() {
        let dir = std::env::temp_dir().join("vtessera_test_rotate");
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        // Write five receipt files with monotonically increasing names.
        for i in 0..5 {
            let name = format!("receipt_{i:020}.json");
            fs::write(dir.join(&name), b"{}").unwrap();
        }
        let removed = rotate(&dir, 3).unwrap();
        assert_eq!(removed, 2);
        let mut remaining: Vec<_> = fs::read_dir(&dir)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        remaining.sort();
        assert_eq!(
            remaining,
            vec![
                "receipt_00000000000000000002.json",
                "receipt_00000000000000000003.json",
                "receipt_00000000000000000004.json",
            ]
        );
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn rotate_keeps_non_receipt_files() {
        let dir = std::env::temp_dir().join("vtessera_test_rotate_unrelated");
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        for i in 0..3 {
            let name = format!("receipt_{i:020}.json");
            fs::write(dir.join(&name), b"{}").unwrap();
        }
        fs::write(dir.join("operator_notes.txt"), b"hi").unwrap();
        fs::write(dir.join("archive.tar"), b"hi").unwrap();
        rotate(&dir, 1).unwrap();
        assert!(dir.join("operator_notes.txt").exists());
        assert!(dir.join("archive.tar").exists());
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn rotate_no_op_when_under_cap() {
        let dir = std::env::temp_dir().join("vtessera_test_rotate_under");
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        for i in 0..2 {
            let name = format!("receipt_{i:020}.json");
            fs::write(dir.join(&name), b"{}").unwrap();
        }
        let removed = rotate(&dir, 10).unwrap();
        assert_eq!(removed, 0);
        let _ = fs::remove_dir_all(&dir);
    }
}
