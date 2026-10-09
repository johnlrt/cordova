use std::collections::HashSet;
use std::sync::atomic::{AtomicU32, Ordering};

use chrono::{DateTime, SecondsFormat, Utc};
use rand::RngCore;
use serde::{
    Deserialize, Serialize,
    de::{self, DeserializeOwned, MapAccess, SeqAccess, Visitor},
};
use serde_json::value::RawValue;

use crate::{NodeError, Result};

pub const PROTOCOL_VERSION: u8 = 1;
pub const MAX_FRAME_BYTES: usize = 1_048_576;
pub const MAX_DELTA_BYTES: usize = 16_384;

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Hardware {
    pub platform: String,
    pub arch: String,
    pub backend: String,
    pub gpu_name: Option<String>,
    pub reported_total_vram_bytes: u64,
    pub reported_free_vram_bytes: u64,
    pub ram_bytes: u64,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NodeModel {
    pub model_id: String,
    pub state: String,
    pub active_slots: u32,
    pub max_slots: u32,
    pub verified_capacity_bytes: u64,
    pub updated_at: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(transparent)]
pub struct RequiredNullable<T>(pub Option<T>);

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EnvelopeIn {
    pub v: u8,
    #[serde(rename = "type")]
    pub kind: String,
    pub message_id: String,
    pub request_id: RequiredNullable<String>,
    pub session_id: RequiredNullable<String>,
    pub seq: u32,
    pub sent_at: String,
    pub payload: Box<RawValue>,
    pub error: RequiredNullable<WireError>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WireError {
    pub code: String,
    pub message: String,
    pub retryable: bool,
}

#[derive(Serialize)]
pub struct EnvelopeOut<'a, T: Serialize> {
    pub v: u8,
    #[serde(rename = "type")]
    pub kind: &'a str,
    pub message_id: String,
    pub request_id: Option<String>,
    pub session_id: Option<String>,
    pub seq: u32,
    pub sent_at: String,
    pub payload: T,
    pub error: Option<WireError>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Welcome {
    pub session_id: String,
    pub heartbeat_interval_ms: u32,
    pub heartbeat_timeout_ms: u32,
    pub catalog_version: String,
    pub max_frame_bytes: u32,
    pub connection_expires_at: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct CatalogGet {
    pub known_version: RequiredNullable<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct CatalogSnapshot {
    pub version: String,
    pub models: Vec<crate::catalog::CatalogManifest>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ModelCommand {
    pub model_id: String,
    pub catalog_version: String,
    pub command_id: String,
    pub deadline_at: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ModelRef {
    pub model_id: String,
    pub catalog_version: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ModelStateCommand {
    pub model_id: String,
    pub state: String,
    pub artifact_sha256: Option<String>,
    pub active_slots: u32,
    pub max_slots: u32,
    pub reason_code: Option<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct OperationErrorPayload {
    pub operation_type: String,
    pub attempt_id: Option<String>,
    pub lease_id: Option<String>,
    pub command_id: Option<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct InferenceParameters {
    pub temperature: f64,
    pub top_p: f64,
    pub stop: Vec<String>,
    pub seed: RequiredNullable<u32>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct InferenceStart {
    pub attempt_id: String,
    pub lease_id: String,
    pub model_id: String,
    pub catalog_version: String,
    pub input_token_ids: Vec<u32>,
    pub input_tokens: u32,
    pub max_tokens: u32,
    pub parameters: InferenceParameters,
    pub deadline_at: String,
    pub kind: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct InferenceCancel {
    pub attempt_id: String,
    pub lease_id: String,
    pub reason: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct BenchmarkLease {
    pub slot_index: u32,
    pub lease_id: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct BenchmarkStart {
    pub benchmark_id: String,
    pub model_id: String,
    pub catalog_version: String,
    pub input_token_ids: Vec<u32>,
    pub max_tokens: u32,
    pub seed: u32,
    pub required_concurrency: u32,
    pub leases: Vec<BenchmarkLease>,
    pub deadline_at: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BenchmarkDelta {
    pub benchmark_id: String,
    pub slot_index: u32,
    pub lease_id: String,
    pub chunk_index: u32,
    pub text: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BenchmarkSlotResult {
    pub slot_index: u32,
    pub lease_id: String,
    pub claimed_output_tokens: u32,
    pub finish_reason: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BenchmarkResult {
    pub benchmark_id: String,
    pub slots: Vec<BenchmarkSlotResult>,
    pub reported_peak_vram_bytes: u64,
    pub completed_slots: u32,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Drain {
    pub deadline_at: String,
    pub reason: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct HeartbeatAck {
    pub server_time: String,
    pub session_expires_at: String,
}

pub fn decode<T: DeserializeOwned>(payload: &RawValue) -> Result<T> {
    check_duplicate_keys(payload.get().as_bytes())?;
    serde_json::from_str(payload.get())
        .map_err(|e| NodeError::Protocol(format!("invalid payload: {e}")))
}

fn check_duplicate_keys(bytes: &[u8]) -> Result<()> {
    let mut deserializer = serde_json::Deserializer::from_slice(bytes);
    CheckDuplicateKeys::deserialize(&mut deserializer)
        .map_err(|_| NodeError::Protocol("JSON contains duplicate object keys".into()))?;
    deserializer
        .end()
        .map_err(|_| NodeError::Protocol("JSON contains trailing data".into()))
}

struct CheckDuplicateKeys;

impl<'de> Deserialize<'de> for CheckDuplicateKeys {
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        deserializer.deserialize_any(DuplicateKeyVisitor)
    }
}

struct DuplicateKeyVisitor;

impl<'de> Visitor<'de> for DuplicateKeyVisitor {
    type Value = CheckDuplicateKeys;

    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("a JSON value without duplicate object keys")
    }

    fn visit_bool<E>(self, _: bool) -> std::result::Result<Self::Value, E> {
        Ok(CheckDuplicateKeys)
    }

    fn visit_i64<E>(self, _: i64) -> std::result::Result<Self::Value, E> {
        Ok(CheckDuplicateKeys)
    }

    fn visit_u64<E>(self, _: u64) -> std::result::Result<Self::Value, E> {
        Ok(CheckDuplicateKeys)
    }

    fn visit_f64<E>(self, _: f64) -> std::result::Result<Self::Value, E> {
        Ok(CheckDuplicateKeys)
    }

    fn visit_str<E>(self, _: &str) -> std::result::Result<Self::Value, E> {
        Ok(CheckDuplicateKeys)
    }

    fn visit_string<E>(self, _: String) -> std::result::Result<Self::Value, E> {
        Ok(CheckDuplicateKeys)
    }

    fn visit_none<E>(self) -> std::result::Result<Self::Value, E> {
        Ok(CheckDuplicateKeys)
    }

    fn visit_unit<E>(self) -> std::result::Result<Self::Value, E> {
        Ok(CheckDuplicateKeys)
    }

    fn visit_some<D>(self, deserializer: D) -> std::result::Result<Self::Value, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        CheckDuplicateKeys::deserialize(deserializer)
    }

    fn visit_seq<A>(self, mut sequence: A) -> std::result::Result<Self::Value, A::Error>
    where
        A: SeqAccess<'de>,
    {
        while sequence.next_element::<CheckDuplicateKeys>()?.is_some() {}
        Ok(CheckDuplicateKeys)
    }

    fn visit_map<A>(self, mut map: A) -> std::result::Result<Self::Value, A::Error>
    where
        A: MapAccess<'de>,
    {
        let mut keys = HashSet::new();
        while let Some(key) = map.next_key::<String>()? {
            if !keys.insert(key) {
                return Err(de::Error::custom("duplicate JSON object key"));
            }
            map.next_value::<CheckDuplicateKeys>()?;
        }
        Ok(CheckDuplicateKeys)
    }
}

pub fn validate_incoming(
    envelope: &EnvelopeIn,
    expected_seq: u32,
    session_id: Option<&str>,
) -> Result<()> {
    if envelope.v != PROTOCOL_VERSION {
        return Err(NodeError::Protocol("unsupported protocol version".into()));
    }
    if envelope.seq != expected_seq {
        return Err(NodeError::Protocol(
            "server sequence is not strictly increasing".into(),
        ));
    }
    if envelope.seq == 0 {
        return Err(NodeError::Protocol("sequence must start at one".into()));
    }
    validate_uuid(&envelope.message_id)?;
    if let Some(request_id) = &envelope.request_id.0 {
        validate_uuid(request_id)?;
    }
    DateTime::parse_from_rfc3339(&envelope.sent_at)
        .map_err(|_| NodeError::Protocol("sent_at is not an RFC3339 timestamp".into()))?;
    if envelope.kind == "welcome" {
        if envelope.session_id.0.is_none() || session_id.is_some() {
            return Err(NodeError::Protocol(
                "welcome has an invalid session binding".into(),
            ));
        }
    } else if let Some(session_id) = session_id {
        if envelope.session_id.0.as_deref() != Some(session_id) {
            return Err(NodeError::Protocol(
                "frame belongs to another session".into(),
            ));
        }
    } else if envelope.kind != "error" {
        return Err(NodeError::Protocol(
            "expected welcome as the first server frame".into(),
        ));
    }
    if (envelope.kind == "error") != envelope.error.0.is_some() {
        return Err(NodeError::Protocol(
            "error field does not match message type".into(),
        ));
    }
    if envelope
        .error
        .0
        .as_ref()
        .is_some_and(|e| e.message.len() > 256 || e.message.is_empty())
    {
        return Err(NodeError::Protocol(
            "server error message is outside the contract".into(),
        ));
    }
    Ok(())
}

pub fn decode_text(bytes: &[u8]) -> Result<EnvelopeIn> {
    if bytes.len() > MAX_FRAME_BYTES {
        return Err(NodeError::Protocol("frame exceeds 1 MiB".into()));
    }
    check_duplicate_keys(bytes)?;
    let frame: EnvelopeIn = serde_json::from_slice(bytes)
        .map_err(|e| NodeError::Protocol(format!("invalid JSON envelope: {e}")))?;
    Ok(frame)
}

pub fn encode<T: Serialize>(
    kind: &str,
    payload: T,
    request_id: Option<String>,
    session_id: Option<String>,
    seq: u32,
) -> Result<Vec<u8>> {
    encode_with_error(kind, payload, request_id, session_id, seq, None)
}

pub fn encode_with_error<T: Serialize>(
    kind: &str,
    payload: T,
    request_id: Option<String>,
    session_id: Option<String>,
    seq: u32,
    error: Option<WireError>,
) -> Result<Vec<u8>> {
    let frame = EnvelopeOut {
        v: PROTOCOL_VERSION,
        kind,
        message_id: uuid_v4(),
        request_id,
        session_id,
        seq,
        sent_at: Utc::now().to_rfc3339_opts(SecondsFormat::Millis, true),
        payload,
        error,
    };
    let bytes = serde_json::to_vec(&frame)
        .map_err(|e| NodeError::Protocol(format!("could not encode envelope: {e}")))?;
    if bytes.len() > MAX_FRAME_BYTES {
        return Err(NodeError::Protocol("outgoing frame exceeds 1 MiB".into()));
    }
    Ok(bytes)
}

pub fn next_seq(seq: &AtomicU32) -> Result<u32> {
    seq.fetch_update(Ordering::SeqCst, Ordering::SeqCst, |n| n.checked_add(1))
        .map_err(|_| NodeError::Protocol("outgoing sequence exhausted".into()))
        .map(|n| n + 1)
}

pub fn uuid_v4() -> String {
    let mut bytes = [0u8; 16];
    rand::rng().fill_bytes(&mut bytes);
    bytes[6] = (bytes[6] & 0x0f) | 0x40;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    format!(
        "{}-{}-{}-{}-{}",
        hex::encode(&bytes[0..4]),
        hex::encode(&bytes[4..6]),
        hex::encode(&bytes[6..8]),
        hex::encode(&bytes[8..10]),
        hex::encode(&bytes[10..16])
    )
}

pub fn validate_uuid(value: &str) -> Result<()> {
    let bytes = value.as_bytes();
    let valid_shape = bytes.len() == 36
        && [8, 13, 18, 23].iter().all(|i| bytes[*i] == b'-')
        && bytes
            .iter()
            .enumerate()
            .all(|(i, b)| [8, 13, 18, 23].contains(&i) || b.is_ascii_hexdigit())
        && bytes[14] == b'4'
        && matches!(bytes[19].to_ascii_lowercase(), b'8' | b'9' | b'a' | b'b')
        && value == value.to_ascii_lowercase();
    if valid_shape {
        Ok(())
    } else {
        Err(NodeError::Protocol(
            "identifier must be a lowercase UUIDv4".into(),
        ))
    }
}

pub fn deadline_instant(deadline_at: &str) -> Result<tokio::time::Instant> {
    let deadline = DateTime::parse_from_rfc3339(deadline_at)
        .map_err(|_| NodeError::Protocol("invalid deadline timestamp".into()))?;
    let remaining = deadline
        .with_timezone(&Utc)
        .signed_duration_since(Utc::now());
    if remaining <= chrono::TimeDelta::zero() {
        return Err(NodeError::Protocol("dispatch deadline has expired".into()));
    }
    let duration = remaining
        .to_std()
        .map_err(|_| NodeError::Protocol("invalid dispatch deadline".into()))?;
    Ok(tokio::time::Instant::now() + duration)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicU32;

    #[test]
    fn uuid_generation_and_sequence_are_valid() {
        let id = uuid_v4();
        validate_uuid(&id).unwrap();
        let sequence = AtomicU32::new(0);
        assert_eq!(next_seq(&sequence).unwrap(), 1);
        assert_eq!(next_seq(&sequence).unwrap(), 2);
    }

    #[test]
    fn rejects_duplicate_and_unknown_envelope_fields() {
        let duplicate = br#"{"v":1,"v":1,"type":"welcome","message_id":"00000000-0000-4000-8000-000000000001","request_id":null,"session_id":null,"seq":1,"sent_at":"2026-10-09T00:00:00Z","payload":{},"error":null}"#;
        assert!(decode_text(duplicate).is_err());
        let unknown = br#"{"v":1,"type":"welcome","message_id":"00000000-0000-4000-8000-000000000001","request_id":null,"session_id":null,"seq":1,"sent_at":"2026-10-09T00:00:00Z","payload":{},"error":null,"x":1}"#;
        assert!(decode_text(unknown).is_err());
        let nested_duplicate = br#"{"v":1,"type":"welcome","message_id":"00000000-0000-4000-8000-000000000001","request_id":null,"session_id":null,"seq":1,"sent_at":"2026-10-09T00:00:00Z","payload":{"x":1,"x":2},"error":null}"#;
        assert!(decode_text(nested_duplicate).is_err());
        let payload = RawValue::from_string(r#"{"x":1,"x":2}"#.to_owned()).unwrap();
        assert!(decode::<serde_json::Value>(&payload).is_err());
    }
}
