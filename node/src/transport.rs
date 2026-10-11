use std::{
    collections::{HashSet, VecDeque},
    net::IpAddr,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use futures_util::{SinkExt, StreamExt};
use rand::RngCore;
use reqwest::header::{AUTHORIZATION, HeaderValue};
use serde::{Serialize, de::DeserializeOwned};
use serde_json::Value;
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpStream, lookup_host},
    sync::{Mutex, mpsc, oneshot},
    task::JoinSet,
    time::{Instant, interval_at, sleep, sleep_until, timeout},
};
use tokio_tungstenite::{
    WebSocketStream,
    tungstenite::{Message, client::IntoClientRequest, protocol::WebSocketConfig},
};
use tokio_util::sync::CancellationToken;
use url::Url;

use crate::{
    NodeError, Result,
    config::{Config, validate_server_url},
    credentials, enrollment, hardware,
    model_manager::{ModelManager, ModelStatePayload},
    protocol::{
        self, CatalogGet, CatalogSnapshot, Drain, EnvelopeIn, HeartbeatAck, InferenceCancel,
        InferenceStart, ModelCommand, ModelRef, OperationErrorPayload, PROTOCOL_VERSION,
        RequiredNullable, Welcome, WireError, uuid_v4,
    },
    service::{ModelStatus, NodeStatus, write_status},
};

const HEARTBEAT_MS: u64 = 15_000;
const HEARTBEAT_TIMEOUT: Duration = Duration::from_secs(45);
const MAX_QUEUE_FRAMES: usize = 32;
const MAX_QUEUE_BYTES: usize = 512 * 1024;
const MAX_ACTIVE_MESSAGE_IDS: usize = 4096;

trait AsyncIo: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin + Send {}
impl<T> AsyncIo for T where T: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin + Send {}
type BoxedIo = Box<dyn AsyncIo>;

#[derive(Clone)]
struct OutboundSender {
    tx: mpsc::Sender<WsWrite>,
    queued_bytes: Arc<AtomicUsize>,
}

enum WsWrite {
    Envelope {
        kind: String,
        payload: Value,
        request_id: Option<String>,
        session_id: Option<String>,
        error: Option<WireError>,
        weight: usize,
    },
    Control(Message),
    Flush(oneshot::Sender<()>),
}

impl OutboundSender {
    async fn send<T: Serialize>(
        &self,
        kind: &str,
        payload: T,
        request_id: Option<String>,
        session_id: Option<String>,
    ) -> Result<()> {
        self.send_with_error(kind, payload, request_id, session_id, None)
            .await
    }

    #[allow(clippy::too_many_arguments)]
    async fn send_error(
        &self,
        kind: &str,
        payload: OperationErrorPayload,
        request_id: Option<String>,
        session_id: Option<String>,
        code: &str,
        message: &str,
        retryable: bool,
    ) -> Result<()> {
        self.send_with_error(
            kind,
            payload,
            request_id,
            session_id,
            Some(WireError {
                code: code.to_owned(),
                message: message.to_owned(),
                retryable,
            }),
        )
        .await
    }

    async fn send_with_error<T: Serialize>(
        &self,
        kind: &str,
        payload: T,
        request_id: Option<String>,
        session_id: Option<String>,
        error: Option<WireError>,
    ) -> Result<()> {
        let value = serde_json::to_value(payload)
            .map_err(|_| NodeError::Protocol("outgoing payload could not be encoded".into()))?;
        let weight = serde_json::to_vec(&value)
            .map_err(|_| NodeError::Protocol("outgoing payload could not be encoded".into()))?
            .len()
            .saturating_add(256);
        if weight > MAX_QUEUE_BYTES {
            return Err(NodeError::Protocol(
                "outgoing frame exceeds queue bound".into(),
            ));
        }
        if self
            .queued_bytes
            .fetch_add(weight, Ordering::AcqRel)
            .saturating_add(weight)
            > MAX_QUEUE_BYTES
        {
            self.queued_bytes.fetch_sub(weight, Ordering::AcqRel);
            return Err(NodeError::Protocol("outgoing queue is full".into()));
        }
        let message = WsWrite::Envelope {
            kind: kind.to_owned(),
            payload: value,
            request_id,
            session_id,
            error,
            weight,
        };
        if self.tx.send(message).await.is_err() {
            self.queued_bytes.fetch_sub(weight, Ordering::AcqRel);
            return Err(NodeError::WebSocket("connection writer stopped".into()));
        }
        Ok(())
    }

    async fn control(&self, message: Message) -> Result<()> {
        self.tx
            .send(WsWrite::Control(message))
            .await
            .map_err(|_| NodeError::WebSocket("connection writer stopped".into()))
    }

    async fn flush(&self) -> Result<()> {
        let (tx, rx) = oneshot::channel();
        self.tx
            .send(WsWrite::Flush(tx))
            .await
            .map_err(|_| NodeError::WebSocket("connection writer stopped".into()))?;
        rx.await
            .map_err(|_| NodeError::WebSocket("connection writer stopped".into()))
    }
}

pub async fn run_forever(config: Config) -> Result<()> {
    config.validate()?;
    let filter = tracing_subscriber::EnvFilter::try_new(&config.log_level)
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    let _ = tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_writer(std::io::stderr)
        .try_init();
    let node_id = config
        .node_id
        .clone()
        .ok_or_else(|| NodeError::Config("run login first".into()))?;
    let node_secret =
        credentials::get(&credentials::node_secret_name(&node_id))?.ok_or_else(|| {
            NodeError::Credential(
                "node secret is missing from the OS credential store; run login again".into(),
            )
        })?;
    let manager = Arc::new(ModelManager::new(config.clone()));
    let shutdown = CancellationToken::new();
    let signal_shutdown = shutdown.clone();
    tokio::spawn(async move {
        wait_for_shutdown().await;
        signal_shutdown.cancel();
    });
    let mut backoff_attempt: u32 = 0;
    loop {
        if shutdown.is_cancelled() {
            break;
        }
        let session_started = Instant::now();
        let result = tokio::select! {
            _ = shutdown.cancelled() => break,
            result = run_session(&config, &node_id, &node_secret, manager.clone(), shutdown.clone()) => result,
        };
        if shutdown.is_cancelled() {
            break;
        }
        if session_started.elapsed() >= Duration::from_secs(60) {
            backoff_attempt = 0;
        }
        match result {
            Ok(()) => backoff_attempt = 0,
            Err(NodeError::Http(ref msg)) if msg.contains("authentication was rejected") => {
                manager.shutdown().await;
                write_stopped_status(&config, &node_id, Some("unauthenticated")).await;
                return Err(NodeError::Credential(
                    "server rejected node authentication; revoke or re-enroll this node".into(),
                ));
            }
            Err(error) => {
                tracing::warn!(error_code = safe_error_code(&error), "node session ended");
                write_stopped_status(&config, &node_id, Some(safe_error_code(&error))).await;
            }
        }
        let cap = 60u64.min(1u64.checked_shl(backoff_attempt.min(6)).unwrap_or(60));
        let wait = if cap <= 1 {
            0
        } else {
            rand::rng().next_u64() % (cap * 1000)
        };
        backoff_attempt = backoff_attempt.saturating_add(1);
        tokio::select! {
            _ = shutdown.cancelled() => break,
            _ = sleep(Duration::from_millis(wait)) => {}
        }
    }
    manager.shutdown().await;
    write_stopped_status(&config, &node_id, None).await;
    Ok(())
}

async fn run_session(
    config: &Config,
    node_id: &str,
    node_secret: &str,
    manager: Arc<ModelManager>,
    shutdown: CancellationToken,
) -> Result<()> {
    let access = enrollment::issue_access_token(config, node_id, node_secret).await?;
    let hardware = hardware::detect()?;
    let hwid_hash = hardware::hwid_hash_for(config, &hardware)?;
    let server_url = validate_server_url(
        config
            .server
            .as_deref()
            .ok_or_else(|| NodeError::Config("server is not configured".into()))?,
    )?;
    let ws_url = websocket_url(server_url)?;
    let socket = timeout(
        Duration::from_secs(15),
        open_socket(config, ws_url, &access.access_token),
    )
    .await
    .map_err(|_| NodeError::WebSocket("WebSocket connection timed out".into()))??;
    let (sink, mut stream) = socket.split();
    let (tx, rx) = mpsc::channel(MAX_QUEUE_FRAMES);
    let queued_bytes = Arc::new(AtomicUsize::new(0));
    let outbound = OutboundSender {
        tx,
        queued_bytes: queued_bytes.clone(),
    };
    let writer = tokio::spawn(writer_loop(sink, rx, queued_bytes));

    let hello = serde_json::json!({
        "node_id": node_id,
        "node_version": env!("CARGO_PKG_VERSION"),
        "protocol_versions": [PROTOCOL_VERSION],
        "hardware": hardware,
        "hwid_hash": hwid_hash
    });
    outbound.send("hello", hello, None, None).await?;
    let first = timeout(
        Duration::from_secs(5),
        receive_envelope(&mut stream, &outbound),
    )
    .await
    .map_err(|_| NodeError::WebSocket("server welcome timed out".into()))??;
    protocol::validate_incoming(&first, 1, None)?;
    if first.kind == "error" {
        return Err(server_error(&first));
    }
    if first.kind != "welcome" {
        return Err(NodeError::Protocol(
            "server did not begin with welcome".into(),
        ));
    }
    let welcome: Welcome = decode(&first)?;
    let session_id = first
        .session_id
        .0
        .clone()
        .ok_or_else(|| NodeError::Protocol("welcome omitted session id".into()))?;
    protocol::validate_uuid(&session_id)?;
    protocol::validate_uuid(&welcome.catalog_version)?;
    let connection_expires =
        chrono::DateTime::parse_from_rfc3339(&welcome.connection_expires_at)
            .map_err(|_| NodeError::Protocol("connection expiry is invalid".into()))?;
    if connection_expires <= chrono::Utc::now()
        || connection_expires > chrono::Utc::now() + chrono::TimeDelta::hours(24)
    {
        return Err(NodeError::Protocol(
            "connection lifetime is outside the 24-hour contract".into(),
        ));
    }
    if welcome.session_id != session_id
        || welcome.heartbeat_interval_ms != HEARTBEAT_MS as u32
        || welcome.heartbeat_timeout_ms != 45_000
        || welcome.max_frame_bytes as usize != protocol::MAX_FRAME_BYTES
    {
        return Err(NodeError::Protocol(
            "welcome changed a fixed C3 transport limit".into(),
        ));
    }
    let session = Some(session_id.clone());
    let catalog_request_id = uuid_v4();
    outbound
        .send(
            "catalog.get",
            CatalogGet {
                known_version: RequiredNullable(None),
            },
            Some(catalog_request_id.clone()),
            session.clone(),
        )
        .await?;

    let mut heartbeat = interval_at(
        Instant::now() + Duration::from_millis(welcome.heartbeat_interval_ms as u64),
        Duration::from_millis(welcome.heartbeat_interval_ms as u64),
    );
    heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let mut heartbeat_deadline = Instant::now() + HEARTBEAT_TIMEOUT;
    let heartbeat_timeout = sleep_until(heartbeat_deadline);
    tokio::pin!(heartbeat_timeout);
    let mut requested_drain_deadline = None;
    let mut expected_seq = 2u32;
    let mut pending_heartbeats = HashSet::<String>::new();
    let active = HashMapActive::default();
    let mut tasks = JoinSet::<()>::new();
    let mut seen_ids = HashSet::new();
    let mut seen_order = VecDeque::new();
    let mut graceful_exit = None;
    let mut status = NodeStatus {
        server: config.server.clone(),
        node_id: Some(node_id.to_owned()),
        connected: true,
        state: "online".into(),
        active_slots: 0,
        models: Vec::new(),
        last_error_code: None,
        updated_at: chrono::Utc::now().to_rfc3339(),
    };
    write_status(&status)?;

    let result: Result<()> = async {
        loop {
        tokio::select! {
            _ = shutdown.cancelled() => {
                graceful_exit = Some("shutdown");
                break Ok(());
            }
            _ = heartbeat_timeout.as_mut() => break Err(NodeError::WebSocket("server heartbeat timeout".into())),
            joined = tasks.join_next(), if !tasks.is_empty() => {
                if joined.is_some_and(|j| j.is_err()) { break Err(NodeError::Local("node task stopped unexpectedly".into())); }
                status.active_slots = active.lease_ids().await.len() as u32;
                status.models = model_statuses(manager.node_models().await);
                status.updated_at = chrono::Utc::now().to_rfc3339();
                let _ = write_status(&status);
            }
            _ = heartbeat.tick() => {
                let request_id = uuid_v4();
                pending_heartbeats.insert(request_id.clone());
                let leases = active.lease_ids().await;
                let models = manager.node_models().await;
                let payload = serde_json::json!({
                    "active_leases": leases,
                    "hardware": hardware::detect()?,
                    "models": models
                });
                status.active_slots = leases.len() as u32;
                status.models = model_statuses(models);
                status.updated_at = chrono::Utc::now().to_rfc3339();
                let _ = write_status(&status);
                if pending_heartbeats.len() > 4 { pending_heartbeats.clear(); }
                outbound.send("heartbeat", payload, Some(request_id), session.clone()).await?;
            }
            message = stream.next() => {
                let Some(message) = message else { break Err(NodeError::WebSocket("server closed the node socket".into())); };
                let message = message.map_err(|_| NodeError::WebSocket("node socket read failed".into()))?;
                match message {
                    Message::Ping(data) => { outbound.control(Message::Pong(data)).await?; continue; }
                    Message::Pong(_) => continue,
                    Message::Close(_) => break Err(NodeError::WebSocket("server closed the node socket".into())),
                    Message::Binary(_) => break Err(NodeError::Protocol("binary WebSocket messages are not supported".into())),
                    Message::Text(text) => {
                        let envelope = protocol::decode_text(text.as_bytes())?;
                        protocol::validate_incoming(&envelope, expected_seq, Some(&session_id))?;
                        expected_seq = expected_seq.checked_add(1).ok_or_else(|| NodeError::Protocol("server sequence overflowed".into()))?;
                        if !seen_ids.insert(envelope.message_id.clone()) { continue; }
                        seen_order.push_back(envelope.message_id.clone());
                        if seen_order.len() > MAX_ACTIVE_MESSAGE_IDS {
                            if let Some(old) = seen_order.pop_front() { seen_ids.remove(&old); }
                        }
                        if envelope.kind == "error" { break Err(server_error(&envelope)); }
                        match envelope.kind.as_str() {
                            "heartbeat.ack" => {
                                let ack: HeartbeatAck = decode(&envelope)?;
                                let server_time = chrono::DateTime::parse_from_rfc3339(&ack.server_time)
                                    .map_err(|_| NodeError::Protocol("heartbeat acknowledgement time is invalid".into()))?;
                                if server_time.signed_duration_since(chrono::Utc::now()).num_seconds().unsigned_abs() > 30 {
                                    return Err(NodeError::Protocol("server clock differs by more than 30 seconds".into()));
                                }
                                let session_expiry = chrono::DateTime::parse_from_rfc3339(&ack.session_expires_at)
                                    .map_err(|_| NodeError::Protocol("session expiry is invalid".into()))?;
                                if session_expiry <= chrono::Utc::now()
                                    || session_expiry > chrono::Utc::now() + chrono::TimeDelta::hours(24)
                                {
                                    return Err(NodeError::Protocol("session lifetime is outside the 24-hour contract".into()));
                                }
                                let echoed = envelope.request_id.0.as_deref().ok_or_else(|| NodeError::Protocol("heartbeat acknowledgement omitted request id".into()))?;
                                if !pending_heartbeats.remove(echoed) { return Err(NodeError::Protocol("heartbeat acknowledgement was not requested".into())); }
                                heartbeat_deadline = Instant::now() + HEARTBEAT_TIMEOUT;
                                heartbeat_timeout.as_mut().reset(heartbeat_deadline);
                            }
                            "catalog.snapshot" => {
                                if envelope.request_id.0.as_deref() != Some(catalog_request_id.as_str()) {
                                    return Err(NodeError::Protocol("catalog response did not match its request".into()));
                                }
                                let snapshot: CatalogSnapshot = decode(&envelope)?;
                                if snapshot.version != welcome.catalog_version {
                                    return Err(NodeError::Protocol("catalog snapshot differs from the welcome version".into()));
                                }
                                manager.install_catalog(snapshot).await?;
                                status.models = model_statuses(manager.node_models().await);
                                status.updated_at = chrono::Utc::now().to_rfc3339();
                                write_status(&status)?;
                            }
                            "model.query" => {
                                let request_id = required_request_id(&envelope)?;
                                let query: ModelRef = decode(&envelope)?;
                                let state = match manager.verified_state(&query.model_id).await {
                                    Ok(state) if manager.catalog_version().await.as_deref() == Some(query.catalog_version.as_str()) => state,
                                    _ => ModelStatePayload { model_id: query.model_id.clone(), state: "absent".into(), artifact_sha256: None, active_slots: 0, max_slots: 0, reason_code: Some("catalog_mismatch".into()) },
                                };
                                outbound.send("model.state", state, Some(request_id), session.clone()).await?;
                            }
                            "model.load" | "model.download" => {
                                let command: ModelCommand = decode(&envelope)?;
                                protocol::validate_uuid(&command.command_id)?;
                                if envelope.request_id.0.is_none() { return Err(NodeError::Protocol("model command omitted request id".into())); }
                                let deadline = match protocol::deadline_instant(&command.deadline_at) {
                                    Ok(deadline) => deadline,
                                    Err(_) => {
                                        send_model_reject(&outbound, &session, &command, envelope.request_id.0.clone(), "load_timeout", false).await?;
                                        continue;
                                    }
                                };
                                let operation = if envelope.kind == "model.load" { "load" } else { "download" };
                                outbound.send("model.accepted", serde_json::json!({
                                    "model_id": command.model_id,
                                    "command_id": command.command_id,
                                    "operation": operation
                                }), envelope.request_id.0.clone(), session.clone()).await?;
                                let job_manager = manager.clone();
                                let job_out = outbound.clone();
                                let job_session = session.clone();
                                let job_request = envelope.request_id.0.clone();
                                let job_kind = operation.to_owned();
                                tasks.spawn(async move {
                                    let result = tokio::time::timeout_at(deadline, async {
                                        if job_kind == "download" {
                                            let manifest = job_manager.manifest(&command.model_id, &command.catalog_version).await?;
                                            let total = manifest.artifact_size_bytes;
                                            let _ = job_out.send("model.progress", serde_json::json!({
                                                "model_id": command.model_id, "command_id": command.command_id,
                                                "phase": "downloading", "completed_bytes": 0, "total_bytes": total
                                            }), job_request.clone(), job_session.clone()).await;
                                            job_manager.download(&command.model_id, &command.catalog_version).await?;
                                        } else {
                                            let _ = job_out.send("model.progress", serde_json::json!({
                                                "model_id": command.model_id, "command_id": command.command_id,
                                                "phase": "loading", "completed_bytes": 0, "total_bytes": 1
                                            }), job_request.clone(), job_session.clone()).await;
                                        }
                                        job_manager.load(&command.model_id, &command.catalog_version).await
                                    }).await;
                                    match result {
                                        Ok(Ok((manifest, runner))) => {
                                            let _ = job_out.send("model.ready", serde_json::json!({
                                                "model_id": command.model_id,
                                                "command_id": command.command_id,
                                                "artifact_sha256": manifest.artifact_sha256,
                                                "tokenizer_sha256": manifest.tokenizer_sha256,
                                                "template_sha256": manifest.template_sha256,
                                                "max_slots": runner.max_slots(),
                                                "verified_capacity_bytes": 0
                                            }), job_request, job_session).await;
                                        }
                                        Ok(Err(error)) => {
                                            let code = model_reject_code(&error);
                                            let _ = send_model_reject(&job_out, &job_session, &command, job_request, code, model_reject_retryable(code)).await;
                                        }
                                        Err(_) => {
                                            let _ = send_model_reject(&job_out, &job_session, &command, job_request, "load_timeout", true).await;
                                        }
                                    }
                                });
                            }
                            "inference.start" => {
                                let start: InferenceStart = decode(&envelope)?;
                                validate_inference(&start, envelope.request_id.0.as_deref())?;
                                let manifest = match manager
                                    .manifest(&start.model_id, &start.catalog_version)
                                    .await
                                {
                                    Ok(manifest) => manifest,
                                    Err(_) => {
                                        send_inference_error(
                                            &outbound,
                                            &session,
                                            &start,
                                            envelope.request_id.0.clone(),
                                            "model_not_found",
                                            true,
                                        )
                                        .await?;
                                        continue;
                                    }
                                };
                                if start.input_tokens.saturating_add(start.max_tokens)
                                    > manifest.model.context_tokens
                                    || start.max_tokens > manifest.model.max_output_tokens
                                {
                                    return Err(NodeError::Protocol(
                                        "inference.start exceeds signed model limits".into(),
                                    ));
                                }
                                if active.contains(&start.attempt_id).await {
                                    send_inference_error(&outbound, &session, &start, envelope.request_id.0.clone(), "replayed_message", false).await?;
                                    continue;
                                }
                                let deadline = match protocol::deadline_instant(&start.deadline_at) {
                                    Ok(deadline) => deadline.min(Instant::now() + Duration::from_secs(120)),
                                    Err(_) => {
                                        send_inference_error(&outbound, &session, &start, envelope.request_id.0.clone(), "timeout", true).await?;
                                        continue;
                                    }
                                };
                                let runner = match manager.loaded_runner(&start.model_id, &start.catalog_version).await {
                                    Ok(runner) => runner,
                                    Err(_) => {
                                        send_inference_error(&outbound, &session, &start, envelope.request_id.0.clone(), "model_not_found", true).await?;
                                        continue;
                                    }
                                };
                                let request_id = envelope.request_id.0.clone();
                                let fallback_request_id = required_request_id(&envelope)?;
                                let cancel = CancellationToken::new();
                                active.insert(start.attempt_id.clone(), start.lease_id.clone(), cancel.clone()).await;
                                status.active_slots = active.lease_ids().await.len() as u32;
                                status.updated_at = chrono::Utc::now().to_rfc3339();
                                let _ = write_status(&status);
                                outbound.send("inference.accepted", serde_json::json!({
                                    "attempt_id": start.attempt_id,
                                    "lease_id": start.lease_id
                                }), request_id.clone(), session.clone()).await?;
                                let job_out = outbound.clone();
                                let job_session = session.clone();
                                let job_active = active.clone();
                                tasks.spawn(async move {
                                    let (delta_tx, mut delta_rx) = mpsc::channel::<crate::llama::Delta>(32);
                                    let pump_out = job_out.clone();
                                    let pump_session = job_session.clone();
                                    let pump_start = start.clone();
                                    let pump_request = request_id.clone();
                                    let pump = tokio::spawn(async move {
                                        while let Some(delta) = delta_rx.recv().await {
                                            if pump_out.send("inference.delta", serde_json::json!({
                                                "attempt_id": pump_start.attempt_id,
                                                "lease_id": pump_start.lease_id,
                                                "chunk_index": delta.chunk_index,
                                                "text": delta.text
                                            }), pump_request.clone(), pump_session.clone()).await.is_err() { break; }
                                        }
                                    });
                                    let generated = runner.generate(&start, cancel.clone(), deadline, delta_tx).await;
                                    let _ = pump.await;
                                    match generated {
                                        Ok(generated) => {
                                            let _ = job_out.send("inference.complete", serde_json::json!({
                                                "attempt_id": start.attempt_id,
                                                "lease_id": start.lease_id,
                                                "last_chunk_index": generated.last_chunk_index,
                                                "finish_reason": generated.finish_reason,
                                                "claimed_input_tokens": start.input_tokens,
                                                "claimed_output_tokens": generated.output_tokens
                                            }), request_id.clone(), job_session.clone()).await;
                                        }
                                        Err(error) if error.to_string() == "cancelled" => {
                                            let cancel_request_id = job_active
                                                .cancel_request_id(&start.attempt_id)
                                                .await
                                                .unwrap_or_else(|| fallback_request_id.clone());
                                            let _ = job_out.send("inference.cancelled", serde_json::json!({
                                                "attempt_id": start.attempt_id, "lease_id": start.lease_id
                                            }), Some(cancel_request_id), job_session.clone()).await;
                                        }
                                        Err(error) => {
                                            let (code, retryable) = inference_error_code(&error);
                                            let _ = send_inference_error(&job_out, &job_session, &start, request_id.clone(), code, retryable).await;
                                        }
                                    }
                                    job_active.remove(&start.attempt_id).await;
                                });
                            }
                            "inference.cancel" => {
                                let request_id = required_request_id(&envelope)?;
                                let cancel: InferenceCancel = decode(&envelope)?;
                                if !matches!(cancel.reason.as_str(), "client_cancelled" | "deadline" | "node_revoked" | "output_limit" | "server_shutdown" | "superseded") {
                                    return Err(NodeError::Protocol("inference cancellation reason is invalid".into()));
                                }
                                if !active.cancel(&cancel.attempt_id, &cancel.lease_id, request_id.clone()).await {
                                    send_operation_error(&outbound, &session, Some(request_id), "inference.cancel", Some(cancel.attempt_id), Some(cancel.lease_id), None, "stale_lease", false).await?;
                                }
                            }
                            "benchmark.start" => {
                                let start: crate::protocol::BenchmarkStart = decode(&envelope)?;
                                handle_benchmark(
                                    start,
                                    envelope.request_id.0.clone(),
                                    session.clone(),
                                    manager.clone(),
                                    active.clone(),
                                    &mut tasks,
                                    &outbound,
                                )
                                .await?;
                            }
                            "drain" => {
                                let request_id = required_request_id(&envelope)?;
                                let drain: Drain = decode(&envelope)?;
                                if !matches!(drain.reason.as_str(), "maintenance" | "revoked" | "upgrade") { return Err(NodeError::Protocol("drain reason is invalid".into())); }
                                let server_deadline = protocol::deadline_instant(&drain.deadline_at)?;
                                requested_drain_deadline = Some(server_deadline.min(Instant::now() + Duration::from_secs(30)));
                                graceful_exit = Some(if drain.reason == "upgrade" { "upgrade" } else { "shutdown" });
                                outbound.send("drain.ack", serde_json::json!({"active_leases": active.lease_ids().await}), Some(request_id), session.clone()).await?;
                                break Ok(());
                            }
                            _ => return Err(NodeError::Protocol("server sent a message not accepted by the node".into())),
                        }
                    }
                    _ => continue,
                }
            }
        }
        }
    }
    .await;

    let grace_deadline =
        requested_drain_deadline.unwrap_or_else(|| Instant::now() + Duration::from_secs(30));
    if result.is_ok() && graceful_exit.is_some() {
        while !tasks.is_empty() && Instant::now() < grace_deadline {
            if timeout(grace_deadline - Instant::now(), tasks.join_next())
                .await
                .is_err()
            {
                break;
            }
        }
    }
    if result.is_err() || !tasks.is_empty() {
        for token in active.cancel_all().await {
            token.cancel();
        }
        let _ = timeout(Duration::from_secs(2), async {
            while tasks.join_next().await.is_some() {}
        })
        .await;
    }
    if !tasks.is_empty() {
        tasks.abort_all();
        while tasks.join_next().await.is_some() {}
    }
    if let Some(reason) = graceful_exit {
        send_goodbye(&outbound, &session, &active, reason).await;
    }
    let _ = timeout(Duration::from_secs(5), outbound.flush()).await;
    writer.abort();
    let _ = writer.await;
    if result.is_err() {
        manager.shutdown().await;
    }
    result
}

type ActiveInference = (String, CancellationToken, Option<String>);

#[derive(Clone, Default)]
struct HashMapActive(Arc<Mutex<std::collections::HashMap<String, ActiveInference>>>);

impl HashMapActive {
    async fn insert(&self, attempt: String, lease: String, token: CancellationToken) {
        self.0.lock().await.insert(attempt, (lease, token, None));
    }
    async fn contains(&self, attempt: &str) -> bool {
        self.0.lock().await.contains_key(attempt)
    }
    async fn remove(&self, attempt: &str) {
        self.0.lock().await.remove(attempt);
    }
    async fn lease_ids(&self) -> Vec<String> {
        let mut leases = self
            .0
            .lock()
            .await
            .values()
            .map(|(lease, _, _)| lease.clone())
            .collect::<Vec<_>>();
        leases.sort();
        leases.dedup();
        leases
    }
    async fn cancel(&self, attempt: &str, lease: &str, request_id: String) -> bool {
        let mut active = self.0.lock().await;
        if let Some((active_lease, token, cancel_request_id)) = active.get_mut(attempt) {
            if active_lease == lease {
                *cancel_request_id = Some(request_id);
                token.cancel();
                return true;
            }
        }
        false
    }

    async fn cancel_request_id(&self, attempt: &str) -> Option<String> {
        self.0
            .lock()
            .await
            .get(attempt)
            .and_then(|(_, _, request_id)| request_id.clone())
    }

    async fn cancel_all(&self) -> Vec<CancellationToken> {
        self.0
            .lock()
            .await
            .values()
            .map(|(_, token, _)| token.clone())
            .collect()
    }
}

async fn writer_loop<S>(
    mut sink: S,
    mut rx: mpsc::Receiver<WsWrite>,
    queued_bytes: Arc<AtomicUsize>,
) -> Result<()>
where
    S: futures_util::Sink<Message> + Unpin,
{
    let mut sequence = 0u32;
    while let Some(frame) = rx.recv().await {
        match frame {
            WsWrite::Control(message) => sink
                .send(message)
                .await
                .map_err(|_| NodeError::WebSocket("node socket write failed".into()))?,
            WsWrite::Flush(done) => {
                sink.flush()
                    .await
                    .map_err(|_| NodeError::WebSocket("node socket flush failed".into()))?;
                let _ = done.send(());
            }
            WsWrite::Envelope {
                kind,
                payload,
                request_id,
                session_id,
                error,
                weight,
            } => {
                queued_bytes.fetch_sub(weight, Ordering::AcqRel);
                sequence = sequence
                    .checked_add(1)
                    .ok_or_else(|| NodeError::Protocol("outgoing sequence overflowed".into()))?;
                let bytes = protocol::encode_with_error(
                    &kind, payload, request_id, session_id, sequence, error,
                )?;
                let text = String::from_utf8(bytes)
                    .map_err(|_| NodeError::Protocol("encoded envelope was not UTF-8".into()))?;
                sink.send(Message::Text(text.into()))
                    .await
                    .map_err(|_| NodeError::WebSocket("node socket write failed".into()))?;
            }
        }
    }
    Ok(())
}

async fn receive_envelope<S>(stream: &mut S, outbound: &OutboundSender) -> Result<EnvelopeIn>
where
    S: futures_util::Stream<
            Item = std::result::Result<Message, tokio_tungstenite::tungstenite::Error>,
        > + Unpin,
{
    loop {
        match stream.next().await {
            Some(Ok(Message::Text(text))) => return protocol::decode_text(text.as_bytes()),
            Some(Ok(Message::Ping(data))) => outbound.control(Message::Pong(data)).await?,
            Some(Ok(Message::Pong(_))) => continue,
            Some(Ok(Message::Binary(_))) => {
                return Err(NodeError::Protocol(
                    "binary WebSocket messages are not supported".into(),
                ));
            }
            Some(Ok(Message::Close(_))) | None => {
                return Err(NodeError::WebSocket(
                    "server closed the node socket before welcome".into(),
                ));
            }
            Some(Ok(_)) => continue,
            Some(Err(_)) => return Err(NodeError::WebSocket("node socket read failed".into())),
        }
    }
}

async fn open_socket(
    config: &Config,
    url: Url,
    access_token: &str,
) -> Result<WebSocketStream<tokio_tungstenite::MaybeTlsStream<BoxedIo>>> {
    let mut request = url
        .as_str()
        .into_client_request()
        .map_err(|_| NodeError::WebSocket("node WebSocket URL is invalid".into()))?;
    let authorization = HeaderValue::from_str(&format!("Bearer {access_token}"))
        .map_err(|_| NodeError::WebSocket("node access token is invalid".into()))?;
    request.headers_mut().insert(AUTHORIZATION, authorization);
    let host = url
        .host_str()
        .ok_or_else(|| NodeError::WebSocket("node WebSocket URL has no host".into()))?
        .to_owned();
    let port = url
        .port_or_known_default()
        .ok_or_else(|| NodeError::WebSocket("node WebSocket URL has no port".into()))?;
    let stream = if let Some(proxy) = &config.proxy {
        let proxy = Url::parse(proxy)
            .map_err(|_| NodeError::Config("SOCKS5h proxy URL is invalid".into()))?;
        let proxy_host = proxy
            .host_str()
            .ok_or_else(|| NodeError::Config("SOCKS5h proxy URL has no host".into()))?;
        let proxy_port = proxy
            .port()
            .ok_or_else(|| NodeError::Config("SOCKS5h proxy URL has no port".into()))?;
        let tcp = TcpStream::connect((proxy_host, proxy_port))
            .await
            .map_err(|_| {
                NodeError::WebSocket(
                    "SOCKS5h proxy connection failed; direct fallback is disabled".into(),
                )
            })?;
        let username = credentials::get("proxy-username")?;
        let password = credentials::get("proxy-password")?;
        socks5_connect(tcp, &host, port, username.as_deref(), password.as_deref()).await?
    } else {
        let address = lookup_host((host.as_str(), port))
            .await
            .map_err(|_| NodeError::WebSocket("server name resolution failed".into()))?
            .next()
            .ok_or_else(|| NodeError::WebSocket("server name has no address".into()))?;
        TcpStream::connect(address)
            .await
            .map_err(|_| NodeError::WebSocket("server connection failed".into()))?
    };
    let mut config = WebSocketConfig::default();
    config.max_message_size = Some(protocol::MAX_FRAME_BYTES);
    config.max_frame_size = Some(protocol::MAX_FRAME_BYTES);
    let (socket, _) = tokio_tungstenite::client_async_tls_with_config(
        request,
        Box::new(stream) as BoxedIo,
        Some(config),
        None,
    )
    .await
    .map_err(|_| NodeError::WebSocket("TLS or WebSocket handshake failed".into()))?;
    Ok(socket)
}

async fn socks5_connect(
    mut stream: TcpStream,
    host: &str,
    port: u16,
    username: Option<&str>,
    password: Option<&str>,
) -> Result<TcpStream> {
    let has_auth = username.is_some() || password.is_some();
    if username.is_some() != password.is_some() {
        return Err(NodeError::Credential(
            "SOCKS5h username and password must both be stored".into(),
        ));
    }
    let methods: &[u8] = if has_auth { &[0, 2] } else { &[0] };
    let mut hello = vec![5, methods.len() as u8];
    hello.extend_from_slice(methods);
    stream
        .write_all(&hello)
        .await
        .map_err(|_| NodeError::WebSocket("SOCKS5h negotiation failed".into()))?;
    let mut selected = [0u8; 2];
    stream
        .read_exact(&mut selected)
        .await
        .map_err(|_| NodeError::WebSocket("SOCKS5h negotiation failed".into()))?;
    if selected[0] != 5 || selected[1] == 0xff {
        return Err(NodeError::WebSocket(
            "SOCKS5h proxy rejected authentication methods".into(),
        ));
    }
    match selected[1] {
        0 => {}
        2 if has_auth => {
            let user = username.unwrap_or_default().as_bytes();
            let pass = password.unwrap_or_default().as_bytes();
            if user.len() > 255 || pass.len() > 255 {
                return Err(NodeError::Credential(
                    "SOCKS5h credentials exceed protocol limits".into(),
                ));
            }
            let mut auth = vec![1, user.len() as u8];
            auth.extend_from_slice(user);
            auth.push(pass.len() as u8);
            auth.extend_from_slice(pass);
            stream
                .write_all(&auth)
                .await
                .map_err(|_| NodeError::WebSocket("SOCKS5h authentication failed".into()))?;
            let mut reply = [0u8; 2];
            stream
                .read_exact(&mut reply)
                .await
                .map_err(|_| NodeError::WebSocket("SOCKS5h authentication failed".into()))?;
            if reply != [1, 0] {
                return Err(NodeError::Credential(
                    "SOCKS5h authentication was rejected".into(),
                ));
            }
        }
        _ => {
            return Err(NodeError::Credential(
                "SOCKS5h proxy requires unsupported credentials".into(),
            ));
        }
    }
    let mut request = vec![5, 1, 0];
    match host.parse::<IpAddr>() {
        Ok(IpAddr::V4(ip)) => {
            request.push(1);
            request.extend_from_slice(&ip.octets());
        }
        Ok(IpAddr::V6(ip)) => {
            request.push(4);
            request.extend_from_slice(&ip.octets());
        }
        Err(_) => {
            let name = host.as_bytes();
            if name.is_empty() || name.len() > 255 {
                return Err(NodeError::WebSocket(
                    "server hostname is outside SOCKS5h limits".into(),
                ));
            }
            request.push(3);
            request.push(name.len() as u8);
            request.extend_from_slice(name);
        }
    }
    request.extend_from_slice(&port.to_be_bytes());
    stream
        .write_all(&request)
        .await
        .map_err(|_| NodeError::WebSocket("SOCKS5h tunnel request failed".into()))?;
    let mut response = [0u8; 4];
    stream
        .read_exact(&mut response)
        .await
        .map_err(|_| NodeError::WebSocket("SOCKS5h tunnel request failed".into()))?;
    if response[0] != 5 || response[1] != 0 {
        return Err(NodeError::WebSocket(
            "SOCKS5h proxy could not connect to server".into(),
        ));
    }
    match response[3] {
        1 => {
            let mut rest = [0u8; 6];
            stream
                .read_exact(&mut rest)
                .await
                .map_err(|_| NodeError::WebSocket("SOCKS5h tunnel reply was invalid".into()))?;
        }
        4 => {
            let mut rest = [0u8; 18];
            stream
                .read_exact(&mut rest)
                .await
                .map_err(|_| NodeError::WebSocket("SOCKS5h tunnel reply was invalid".into()))?;
        }
        3 => {
            let mut length = [0u8; 1];
            stream
                .read_exact(&mut length)
                .await
                .map_err(|_| NodeError::WebSocket("SOCKS5h tunnel reply was invalid".into()))?;
            let mut rest = vec![0u8; length[0] as usize + 2];
            stream
                .read_exact(&mut rest)
                .await
                .map_err(|_| NodeError::WebSocket("SOCKS5h tunnel reply was invalid".into()))?;
        }
        _ => {
            return Err(NodeError::WebSocket(
                "SOCKS5h tunnel reply used an invalid address type".into(),
            ));
        }
    }
    Ok(stream)
}

fn websocket_url(mut url: Url) -> Result<Url> {
    let scheme = match url.scheme() {
        "https" => "wss",
        "http" => "ws",
        _ => return Err(NodeError::Config("server URL scheme is invalid".into())),
    };
    url.set_scheme(scheme)
        .map_err(|_| NodeError::Config("server URL scheme is invalid".into()))?;
    url.set_path("/api/node/ws");
    url.set_query(None);
    url.set_fragment(None);
    Ok(url)
}

fn decode<T: DeserializeOwned>(envelope: &EnvelopeIn) -> Result<T> {
    protocol::decode(&envelope.payload)
}

fn required_request_id(envelope: &EnvelopeIn) -> Result<String> {
    envelope
        .request_id
        .0
        .clone()
        .filter(|request_id| !request_id.is_empty())
        .ok_or_else(|| NodeError::Protocol("control message omitted request id".into()))
}

fn server_error(envelope: &EnvelopeIn) -> NodeError {
    let code = envelope
        .error
        .0
        .as_ref()
        .map(|error| error.code.as_str())
        .unwrap_or("internal_error");
    NodeError::WebSocket(format!("server returned protocol error {code}"))
}

async fn send_model_reject(
    out: &OutboundSender,
    session: &Option<String>,
    command: &ModelCommand,
    request_id: Option<String>,
    code: &str,
    retryable: bool,
) -> Result<()> {
    if !matches!(
        code,
        "insufficient_memory"
            | "disk_full"
            | "unsupported_backend"
            | "catalog_mismatch"
            | "download_failed"
            | "load_timeout"
            | "busy"
    ) {
        return Err(NodeError::Protocol(
            "local model failure has no safe protocol code".into(),
        ));
    }
    out.send(
        "model.reject",
        serde_json::json!({
            "model_id": command.model_id,
            "command_id": command.command_id,
            "code": code,
            "retryable": retryable
        }),
        request_id,
        session.clone(),
    )
    .await
}

async fn send_inference_error(
    out: &OutboundSender,
    session: &Option<String>,
    start: &InferenceStart,
    request_id: Option<String>,
    code: &str,
    retryable: bool,
) -> Result<()> {
    send_operation_error(
        out,
        session,
        request_id,
        "inference.start",
        Some(start.attempt_id.clone()),
        Some(start.lease_id.clone()),
        None,
        code,
        retryable,
    )
    .await
}

#[allow(clippy::too_many_arguments)]
async fn send_operation_error(
    out: &OutboundSender,
    session: &Option<String>,
    request_id: Option<String>,
    operation: &str,
    attempt: Option<String>,
    lease: Option<String>,
    command: Option<String>,
    code: &str,
    retryable: bool,
) -> Result<()> {
    let safe = match code {
        "replayed_message" => ("replayed_message", "Request was already handled."),
        "stale_lease" => ("stale_lease", "Lease is no longer active."),
        "model_not_found" => ("model_not_found", "Requested model is unavailable."),
        "catalog_mismatch" => ("catalog_mismatch", "Model catalog verification failed."),
        "timeout" => ("timeout", "Request deadline expired."),
        "cancelled" => ("cancelled", "Request was cancelled."),
        "output_limit" => ("output_limit", "Generated output exceeded the limit."),
        "no_capacity" => ("no_capacity", "Node capacity is not available."),
        _ => ("inference_failed", "Inference failed."),
    };
    out.send_error(
        "error",
        OperationErrorPayload {
            operation_type: operation.to_owned(),
            attempt_id: attempt,
            lease_id: lease,
            command_id: command,
        },
        request_id,
        session.clone(),
        safe.0,
        safe.1,
        retryable,
    )
    .await
}

fn validate_inference(start: &InferenceStart, request_id: Option<&str>) -> Result<()> {
    if request_id.is_none() {
        return Err(NodeError::Protocol(
            "inference.start omitted request id".into(),
        ));
    }
    protocol::validate_uuid(&start.attempt_id)?;
    protocol::validate_uuid(&start.lease_id)?;
    if start.model_id.is_empty()
        || start.model_id.len() > 96
        || start.input_token_ids.is_empty()
        || start.input_token_ids.len() > 8192
        || start.input_tokens as usize != start.input_token_ids.len()
        || !(1..=2048).contains(&start.max_tokens)
    {
        return Err(NodeError::Protocol(
            "inference.start limits are invalid".into(),
        ));
    }
    if !((0.0..=2.0).contains(&start.parameters.temperature)
        && 0.0 < start.parameters.top_p
        && start.parameters.top_p <= 1.0)
        || start.parameters.stop.len() > 4
        || start
            .parameters
            .stop
            .iter()
            .any(|s| s.chars().count() > 128)
        || !matches!(start.kind.as_str(), "user" | "shadow")
    {
        return Err(NodeError::Protocol(
            "inference.start parameters are invalid".into(),
        ));
    }
    if let Some(seed) = start.parameters.seed.0 {
        if seed > i32::MAX as u32 {
            return Err(NodeError::Protocol(
                "inference seed is outside the contract".into(),
            ));
        }
    }
    Ok(())
}

fn inference_error_code(error: &NodeError) -> (&'static str, bool) {
    match error {
        NodeError::Model(message) if message == "cancelled" => ("cancelled", false),
        NodeError::Model(message) if message == "timeout" => ("timeout", true),
        NodeError::Model(message) if message == "output_limit" => ("output_limit", false),
        _ => ("inference_failed", true),
    }
}

fn model_reject_code(error: &NodeError) -> &'static str {
    match error {
        NodeError::Catalog(_) => "catalog_mismatch",
        NodeError::Model(message) if message == "insufficient_memory" => "insufficient_memory",
        NodeError::Model(message) if message == "disk_full" => "disk_full",
        NodeError::Model(message) if message == "busy" => "busy",
        NodeError::Model(message) if message == "unsupported_backend" => "unsupported_backend",
        NodeError::Model(message) if message == "catalog_mismatch" => "catalog_mismatch",
        NodeError::Model(message) if message == "timeout" => "load_timeout",
        _ => "download_failed",
    }
}

fn model_reject_retryable(code: &str) -> bool {
    !matches!(code, "unsupported_backend" | "catalog_mismatch")
}

async fn send_goodbye(
    out: &OutboundSender,
    session: &Option<String>,
    active: &HashMapActive,
    reason: &str,
) {
    let _ = out
        .send(
            "goodbye",
            serde_json::json!({"reason":reason,"active_leases":active.lease_ids().await}),
            None,
            session.clone(),
        )
        .await;
}

async fn handle_benchmark(
    start: crate::protocol::BenchmarkStart,
    request_id: Option<String>,
    session: Option<String>,
    manager: Arc<ModelManager>,
    active: HashMapActive,
    tasks: &mut JoinSet<()>,
    outbound: &OutboundSender,
) -> Result<()> {
    let Some(request_id) = request_id else {
        return Err(NodeError::Protocol(
            "benchmark.start omitted request id".into(),
        ));
    };
    protocol::validate_uuid(&start.benchmark_id)?;
    if start.max_tokens != 256
        || start.required_concurrency != 1
        || start.leases.len() != 1
        || start.leases[0].slot_index != 0
        || start.input_token_ids.is_empty()
        || start.input_token_ids.len() > 8192
        || start.seed > i32::MAX as u32
    {
        return send_benchmark_error(
            outbound,
            &session,
            Some(request_id),
            "no_capacity",
            "Benchmark challenge exceeds the node's calibrated limits.",
            true,
        )
        .await;
    }
    protocol::validate_uuid(&start.leases[0].lease_id)?;
    let deadline = match protocol::deadline_instant(&start.deadline_at) {
        Ok(deadline) => deadline.min(Instant::now() + Duration::from_secs(120)),
        Err(_) => {
            return send_benchmark_error(
                outbound,
                &session,
                Some(request_id),
                "timeout",
                "Benchmark challenge expired.",
                false,
            )
            .await;
        }
    };
    let manifest = match manager
        .manifest(&start.model_id, &start.catalog_version)
        .await
    {
        Ok(manifest) => manifest,
        Err(_) => {
            return send_benchmark_error(
                outbound,
                &session,
                Some(request_id),
                "model_not_found",
                "Benchmark model is not in the current signed catalog.",
                true,
            )
            .await;
        }
    };
    if start.input_token_ids.len() as u64 + u64::from(start.max_tokens)
        > u64::from(manifest.model.context_tokens)
    {
        return send_benchmark_error(
            outbound,
            &session,
            Some(request_id),
            "no_capacity",
            "Benchmark challenge exceeds the model context limit.",
            true,
        )
        .await;
    }
    let runner = match manager
        .loaded_runner(&start.model_id, &start.catalog_version)
        .await
    {
        Ok(runner) if runner.max_slots() >= start.required_concurrency => runner,
        _ => {
            return send_benchmark_error(
                outbound,
                &session,
                Some(request_id),
                "no_capacity",
                "Benchmark model is not loaded or has insufficient slots.",
                true,
            )
            .await;
        }
    };
    let active_key = format!("benchmark:{}", start.benchmark_id);
    if active.contains(&active_key).await {
        return send_benchmark_error(
            outbound,
            &session,
            Some(request_id),
            "replayed_message",
            "Benchmark challenge was already started.",
            false,
        )
        .await;
    }
    let lease = start.leases[0].lease_id.clone();
    let cancellation = CancellationToken::new();
    active
        .insert(active_key.clone(), lease.clone(), cancellation.clone())
        .await;
    let job_out = outbound.clone();
    let job_session = session.clone();
    tasks.spawn(async move {
        let (delta_tx, mut delta_rx) = mpsc::channel::<crate::llama::Delta>(32);
        let pump_out = job_out.clone();
        let pump_session = job_session.clone();
        let pump_start = start.clone();
        let pump_request = request_id.clone();
        let pump = tokio::spawn(async move {
            while let Some(delta) = delta_rx.recv().await {
                let payload = crate::protocol::BenchmarkDelta {
                    benchmark_id: pump_start.benchmark_id.clone(),
                    slot_index: 0,
                    lease_id: pump_start.leases[0].lease_id.clone(),
                    chunk_index: delta.chunk_index,
                    text: delta.text,
                };
                if pump_out
                    .send(
                        "benchmark.delta",
                        payload,
                        Some(pump_request.clone()),
                        pump_session.clone(),
                    )
                    .await
                    .is_err()
                {
                    break;
                }
            }
        });
        let generated = runner
            .generate_benchmark(
                &start.input_token_ids,
                start.max_tokens,
                start.seed,
                cancellation,
                deadline,
                delta_tx,
            )
            .await;
        let _ = pump.await;
        match generated {
            Ok(generated) => {
                let _ = job_out
                    .send(
                        "benchmark.result",
                        crate::protocol::BenchmarkResult {
                            benchmark_id: start.benchmark_id,
                            slots: vec![crate::protocol::BenchmarkSlotResult {
                                slot_index: 0,
                                lease_id: lease,
                                claimed_output_tokens: generated.output_tokens,
                                finish_reason: generated.finish_reason.into(),
                            }],
                            reported_peak_vram_bytes: 0,
                            completed_slots: 1,
                        },
                        Some(request_id),
                        job_session,
                    )
                    .await;
            }
            Err(error) => {
                let (code, retryable) = inference_error_code(&error);
                let _ = send_benchmark_error(
                    &job_out,
                    &job_session,
                    Some(request_id),
                    code,
                    "Benchmark inference failed on this node.",
                    retryable,
                )
                .await;
            }
        }
        active.remove(&active_key).await;
    });
    Ok(())
}

async fn send_benchmark_error(
    outbound: &OutboundSender,
    session: &Option<String>,
    request_id: Option<String>,
    code: &str,
    message: &str,
    retryable: bool,
) -> Result<()> {
    outbound
        .send_error(
            "error",
            OperationErrorPayload {
                operation_type: "benchmark.start".into(),
                attempt_id: None,
                lease_id: None,
                command_id: None,
            },
            request_id,
            session.clone(),
            code,
            message,
            retryable,
        )
        .await
}

async fn write_stopped_status(config: &Config, node_id: &str, error: Option<&str>) {
    let _ = write_status(&NodeStatus {
        server: config.server.clone(),
        node_id: Some(node_id.to_owned()),
        connected: false,
        state: "offline".into(),
        active_slots: 0,
        models: Vec::new(),
        last_error_code: error.map(str::to_owned),
        updated_at: chrono::Utc::now().to_rfc3339(),
    });
}

fn safe_error_code(error: &NodeError) -> &'static str {
    match error {
        NodeError::Credential(_) => "credential_error",
        NodeError::Config(_) => "configuration_error",
        NodeError::Catalog(_) => "catalog_mismatch",
        NodeError::Model(message) if message == "timeout" => "timeout",
        NodeError::Model(_) => "model_error",
        NodeError::Protocol(_) => "invalid_message",
        NodeError::WebSocket(_) => "connection_error",
        NodeError::Http(_) => "authentication_error",
        NodeError::Local(_) => "local_error",
    }
}

fn model_statuses(models: Vec<protocol::NodeModel>) -> Vec<ModelStatus> {
    models
        .into_iter()
        .map(|model| ModelStatus {
            model_id: model.model_id,
            state: model.state,
            active_slots: model.active_slots,
            max_slots: model.max_slots,
        })
        .collect()
}

async fn wait_for_shutdown() {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{SignalKind, signal};
        let mut term = signal(SignalKind::terminate()).expect("SIGTERM handler");
        tokio::select! { _ = tokio::signal::ctrl_c() => {}, _ = term.recv() => {} }
    }
    #[cfg(windows)]
    {
        let _ = tokio::signal::ctrl_c().await;
    }
}

#[cfg(test)]
mod socks5h_tests {
    use super::*;

    #[tokio::test]
    async fn socks5h_sends_hostname_to_proxy_without_local_resolution() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind local SOCKS test proxy");
        let proxy_address = listener.local_addr().expect("proxy address");
        let proxy = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.expect("accept SOCKS client");
            let mut hello = [0u8; 3];
            stream
                .read_exact(&mut hello)
                .await
                .expect("read SOCKS greeting");
            assert_eq!(hello, [5, 1, 0]);
            stream
                .write_all(&[5, 0])
                .await
                .expect("select no-auth mode");

            let mut header = [0u8; 4];
            stream
                .read_exact(&mut header)
                .await
                .expect("read SOCKS connect header");
            assert_eq!(header, [5, 1, 0, 3], "target must be sent as a domain name");
            let mut name_length = [0u8; 1];
            stream
                .read_exact(&mut name_length)
                .await
                .expect("read target name length");
            let mut name = vec![0u8; usize::from(name_length[0])];
            stream
                .read_exact(&mut name)
                .await
                .expect("read unresolved target name");
            let mut port = [0u8; 2];
            stream
                .read_exact(&mut port)
                .await
                .expect("read target port");
            stream
                .write_all(&[5, 0, 0, 1, 127, 0, 0, 1, 0, 1])
                .await
                .expect("return successful SOCKS tunnel");
            (
                String::from_utf8(name).expect("valid target hostname"),
                u16::from_be_bytes(port),
            )
        });

        let stream = TcpStream::connect(proxy_address)
            .await
            .expect("connect to local SOCKS test proxy");
        let tunnel = socks5_connect(stream, "node.invalid", 443, None, None)
            .await
            .expect("SOCKS5h tunnel should be established");
        drop(tunnel);

        assert_eq!(
            proxy.await.expect("proxy task"),
            ("node.invalid".to_owned(), 443),
            "the proxy must receive the hostname and perform DNS resolution"
        );
    }

    #[tokio::test]
    async fn unavailable_socks5h_proxy_does_not_fall_back_to_direct_connection() {
        let direct_listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind direct-fallback sentinel");
        let direct_address = direct_listener.local_addr().expect("sentinel address");
        let proxy_reservation = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("reserve unavailable proxy port");
        let proxy_address = proxy_reservation
            .local_addr()
            .expect("reserved proxy address");
        drop(proxy_reservation);

        let config = Config {
            proxy: Some(format!("socks5h://{proxy_address}")),
            ..Config::default()
        };
        let target = Url::parse(&format!("ws://{direct_address}/api/node/ws")).expect("target URL");
        let error = match open_socket(&config, target, "test-access-token").await {
            Err(NodeError::WebSocket(message)) => message,
            Err(error) => panic!("unexpected error: {error}"),
            Ok(_) => panic!("connection unexpectedly succeeded"),
        };
        assert!(error.contains("direct fallback is disabled"));
        assert!(
            timeout(Duration::from_millis(100), direct_listener.accept())
                .await
                .is_err(),
            "direct target accepted a connection after proxy failure"
        );
    }
}
