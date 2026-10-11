#[cfg(unix)]
use std::os::unix::process::CommandExt;
use std::{
    collections::VecDeque,
    fs::{self, OpenOptions},
    io::Write,
    net::TcpListener,
    path::{Path, PathBuf},
    process::Stdio,
    sync::Arc,
    time::Duration,
};

use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use futures_util::StreamExt;
use rand::RngCore;
use reqwest::Client;
use serde::Serialize;
use tokio::{
    process::{Child, Command},
    sync::{Mutex, Notify, mpsc},
    time::{Instant, sleep, sleep_until},
};
use tokio_util::sync::CancellationToken;

use crate::{
    NodeError, Result,
    catalog::CatalogManifest,
    config::{Config, data_dir},
    protocol::{InferenceStart, MAX_DELTA_BYTES},
};

const RUNNER_READY_TIMEOUT: Duration = Duration::from_secs(60);
const MAX_COMPLETION_BYTES: usize = 1_048_576;

#[derive(Clone, Debug)]
pub struct Generated {
    pub last_chunk_index: Option<u32>,
    pub finish_reason: &'static str,
    pub output_tokens: u32,
}

#[derive(Debug)]
pub struct Delta {
    pub chunk_index: u32,
    pub text: String,
}

pub struct LlamaRunner {
    pub model_id: String,
    pub artifact_sha256: String,
    pub base_url: String,
    api_key: String,
    client: Client,
    process: Mutex<Child>,
    #[cfg(unix)]
    process_id: u32,
    key_file: PathBuf,
    slots: SlotPool,
    slot_count: u32,
}

struct SlotPool {
    free: Mutex<VecDeque<u32>>,
    changed: Notify,
}

struct RunnerKeyFile(Option<PathBuf>);

impl RunnerKeyFile {
    fn path(&self) -> &Path {
        self.0.as_deref().expect("runner key file path")
    }

    fn into_path(mut self) -> PathBuf {
        self.0.take().expect("runner key file path")
    }
}

impl Drop for RunnerKeyFile {
    fn drop(&mut self) {
        if let Some(path) = self.0.take() {
            let _ = fs::remove_file(path);
        }
    }
}

impl SlotPool {
    fn new(count: u32) -> Self {
        Self {
            free: Mutex::new((0..count).collect()),
            changed: Notify::new(),
        }
    }

    async fn acquire(&self, cancel: &CancellationToken, deadline: Instant) -> Result<u32> {
        loop {
            let notified = self.changed.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if let Some(slot) = self.free.lock().await.pop_front() {
                return Ok(slot);
            }
            tokio::select! {
                _ = cancel.cancelled() => return Err(NodeError::Model("cancelled".into())),
                _ = sleep_until(deadline) => return Err(NodeError::Model("timeout".into())),
                _ = notified.as_mut() => {}
            }
        }
    }

    async fn release(&self, slot: u32) {
        self.free.lock().await.push_back(slot);
        self.changed.notify_one();
    }
}

#[derive(Serialize)]
struct CompletionRequest<'a> {
    prompt: &'a [u32],
    n_predict: u32,
    temperature: f64,
    top_p: f64,
    stop: &'a [String],
    seed: i64,
    stream: bool,
    cache_prompt: bool,
    id_slot: u32,
}

impl LlamaRunner {
    pub async fn start(
        config: &Config,
        manifest: &CatalogManifest,
        artifact: &Path,
    ) -> Result<Arc<Self>> {
        let slots = 1u32;
        crate::hardware::verify_manifest_headroom(
            manifest.min_memory_bytes,
            manifest.memory_per_slot_bytes,
            slots as u8,
        )?;
        let binary = config
            .llama_server
            .clone()
            .unwrap_or_else(|| PathBuf::from("llama-server"));
        verify_runner_version(&binary, &manifest.llama_commit)?;
        let port = random_loopback_port()?;
        let mut key_bytes = [0u8; 32];
        rand::rng().fill_bytes(&mut key_bytes);
        let api_key = URL_SAFE_NO_PAD.encode(key_bytes);
        let key_file = create_api_key_file(&api_key)?;
        let args = [
            "--model".to_owned(),
            artifact.to_string_lossy().into_owned(),
            "--alias".to_owned(),
            manifest.model.id.clone(),
            "--host".to_owned(),
            "127.0.0.1".to_owned(),
            "--port".to_owned(),
            port.to_string(),
            "--parallel".to_owned(),
            slots.to_string(),
            "--api-key-file".to_owned(),
            key_file.path().to_string_lossy().into_owned(),
            "--no-cache-prompt".to_owned(),
            "--no-cache-idle-slots".to_owned(),
        ];
        let mut command = Command::new(&binary);
        command
            .args(args)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .kill_on_drop(true);
        #[cfg(unix)]
        command.as_std_mut().process_group(0);
        let mut child = command.spawn().map_err(|_| {
            NodeError::Model("could not start the configured llama-server executable".into())
        })?;
        #[cfg(unix)]
        let process_id = child
            .id()
            .ok_or_else(|| NodeError::Model("llama-server did not expose a process id".into()))?;
        let client = Client::builder()
            .no_proxy()
            .redirect(reqwest::redirect::Policy::none())
            .timeout(Duration::from_secs(130))
            .build()
            .map_err(|_| NodeError::Model("could not initialize the local runner client".into()))?;
        let base_url = format!("http://127.0.0.1:{port}");
        let ready_at = Instant::now() + RUNNER_READY_TIMEOUT;
        loop {
            if Instant::now() >= ready_at {
                let _ = child.kill().await;
                return Err(NodeError::Model(
                    "llama-server did not become ready in time".into(),
                ));
            }
            if let Some(status) = child
                .try_wait()
                .map_err(|_| NodeError::Model("could not inspect llama-server process".into()))?
            {
                return Err(NodeError::Model(format!(
                    "llama-server exited during startup with status {}",
                    status.code().unwrap_or(-1)
                )));
            }
            let ready = tokio::time::timeout(
                Duration::from_secs(2),
                client
                    .get(format!("{base_url}/health"))
                    .bearer_auth(&api_key)
                    .send(),
            )
            .await
            .is_ok_and(|response| response.is_ok_and(|r| r.status().is_success()));
            if ready {
                break;
            }
            sleep(Duration::from_millis(250)).await;
        }
        Ok(Arc::new(Self {
            model_id: manifest.model.id.clone(),
            artifact_sha256: manifest.artifact_sha256.clone(),
            base_url,
            api_key,
            client,
            process: Mutex::new(child),
            #[cfg(unix)]
            process_id,
            key_file: key_file.into_path(),
            slots: SlotPool::new(slots),
            slot_count: slots,
        }))
    }

    pub fn max_slots(&self) -> u32 {
        self.slot_count
    }

    pub async fn active_slots(&self) -> u32 {
        self.slot_count
            .saturating_sub(self.slots.free.lock().await.len() as u32)
    }

    pub async fn generate(
        &self,
        start: &InferenceStart,
        cancellation: CancellationToken,
        deadline: Instant,
        delta_tx: mpsc::Sender<Delta>,
    ) -> Result<Generated> {
        let slot = self.slots.acquire(&cancellation, deadline).await?;
        let result = self
            .generate_in_slot(start, cancellation.clone(), deadline, slot, delta_tx)
            .await;
        self.finish_slot(slot, result).await
    }

    pub async fn generate_benchmark(
        &self,
        input_token_ids: &[u32],
        max_tokens: u32,
        seed: u32,
        cancellation: CancellationToken,
        deadline: Instant,
        delta_tx: mpsc::Sender<Delta>,
    ) -> Result<Generated> {
        let slot = self.slots.acquire(&cancellation, deadline).await?;
        let result = self
            .generate_ids(
                input_token_ids,
                max_tokens,
                0.0,
                1.0,
                &[],
                Some(seed),
                cancellation.clone(),
                deadline,
                slot,
                delta_tx,
            )
            .await;
        self.finish_slot(slot, result).await
    }

    async fn finish_slot(&self, slot: u32, result: Result<Generated>) -> Result<Generated> {
        let erased = self.erase_slot(slot).await;
        if erased.is_ok() {
            self.slots.release(slot).await;
        } else {
            self.kill().await;
        }
        match (result, erased) {
            (Ok(generated), Ok(())) => Ok(generated),
            (Err(error), _) => Err(error),
            (Ok(_), Err(error)) => Err(error),
        }
    }

    async fn generate_in_slot(
        &self,
        start: &InferenceStart,
        cancellation: CancellationToken,
        deadline: Instant,
        slot: u32,
        delta_tx: mpsc::Sender<Delta>,
    ) -> Result<Generated> {
        self.generate_ids(
            &start.input_token_ids,
            start.max_tokens,
            start.parameters.temperature,
            start.parameters.top_p,
            &start.parameters.stop,
            start.parameters.seed.0,
            cancellation,
            deadline,
            slot,
            delta_tx,
        )
        .await
    }

    #[allow(clippy::too_many_arguments)]
    async fn generate_ids(
        &self,
        input_token_ids: &[u32],
        max_tokens: u32,
        temperature: f64,
        top_p: f64,
        stop: &[String],
        seed: Option<u32>,
        cancellation: CancellationToken,
        deadline: Instant,
        slot: u32,
        delta_tx: mpsc::Sender<Delta>,
    ) -> Result<Generated> {
        let request = CompletionRequest {
            prompt: input_token_ids,
            n_predict: max_tokens,
            temperature,
            top_p,
            stop,
            seed: seed.map(i64::from).unwrap_or(-1),
            stream: true,
            cache_prompt: false,
            id_slot: slot,
        };
        let response = tokio::select! {
            _ = cancellation.cancelled() => return Err(NodeError::Model("cancelled".into())),
            _ = sleep_until(deadline) => return Err(NodeError::Model("timeout".into())),
            response = self.client.post(format!("{}/completion", self.base_url)).bearer_auth(&self.api_key).json(&request).send() => response,
        }.map_err(|_| NodeError::Model("llama-server request failed".into()))?;
        if !response.status().is_success() {
            return Err(NodeError::Model("llama-server rejected inference".into()));
        }
        let mut stream = response.bytes_stream();
        let mut buffered = Vec::<u8>::new();
        let mut text = String::new();
        let mut chunk_index = 0u32;
        let mut finish_reason = None;
        'stream: loop {
            let next = tokio::select! {
                _ = cancellation.cancelled() => return Err(NodeError::Model("cancelled".into())),
                _ = sleep_until(deadline) => return Err(NodeError::Model("timeout".into())),
                next = stream.next() => next,
            };
            let Some(next) = next else {
                break;
            };
            let bytes = next.map_err(|_| NodeError::Model("llama-server stream failed".into()))?;
            buffered.extend_from_slice(&bytes);
            if buffered.len() > MAX_COMPLETION_BYTES {
                return Err(NodeError::Model(
                    "llama-server event exceeded its bound".into(),
                ));
            }
            while let Some(end) = find_event_end(&buffered) {
                let event = buffered.drain(..end.0).collect::<Vec<_>>();
                buffered.drain(..end.1);
                let event = std::str::from_utf8(&event)
                    .map_err(|_| NodeError::Model("llama-server event was not UTF-8".into()))?;
                let data = event
                    .lines()
                    .filter_map(|line| line.strip_prefix("data:"))
                    .map(str::trim_start)
                    .collect::<Vec<_>>()
                    .join("\n");
                if data.is_empty() || data == "[DONE]" {
                    continue;
                }
                let value: serde_json::Value = serde_json::from_str(&data).map_err(|_| {
                    NodeError::Model("llama-server returned an invalid completion event".into())
                })?;
                if let Some(content) = value.get("content").and_then(serde_json::Value::as_str) {
                    if !content.is_empty() {
                        if text.len().saturating_add(content.len()) > MAX_COMPLETION_BYTES {
                            return Err(NodeError::Model("output_limit".into()));
                        }
                        text.push_str(content);
                        for piece in split_utf8(content, MAX_DELTA_BYTES) {
                            tokio::select! {
                                _ = cancellation.cancelled() => return Err(NodeError::Model("cancelled".into())),
                                _ = sleep_until(deadline) => return Err(NodeError::Model("timeout".into())),
                                sent = delta_tx.send(Delta {
                                    chunk_index,
                                    text: piece.to_owned(),
                                }) => sent.map_err(|_| NodeError::Model("server connection closed".into()))?,
                            }
                            chunk_index = chunk_index.checked_add(1).ok_or_else(|| {
                                NodeError::Model("completion chunk index overflowed".into())
                            })?;
                        }
                    }
                } else if value.get("error").is_some() {
                    return Err(NodeError::Model(
                        "llama-server reported inference failure".into(),
                    ));
                }
                if value.get("stop").and_then(serde_json::Value::as_bool) == Some(true) {
                    finish_reason = value
                        .get("stop_type")
                        .and_then(serde_json::Value::as_str)
                        .map(str::to_owned);
                    buffered.clear();
                    break 'stream;
                }
            }
        }
        if !buffered.is_empty() && !buffered.iter().all(u8::is_ascii_whitespace) {
            return Err(NodeError::Model(
                "llama-server stream ended inside an event".into(),
            ));
        }
        let finish_reason = finish_reason.ok_or_else(|| {
            NodeError::Model("llama-server stream ended without a terminal event".into())
        })?;
        if !matches!(finish_reason.as_str(), "eos" | "limit" | "word") {
            return Err(NodeError::Model(
                "llama-server returned an unsupported finish reason".into(),
            ));
        }
        let output_tokens = self
            .count_output_tokens(&text, cancellation, deadline)
            .await?;
        if output_tokens > max_tokens {
            return Err(NodeError::Model("output_limit".into()));
        }
        Ok(Generated {
            last_chunk_index: chunk_index.checked_sub(1),
            finish_reason: if finish_reason == "limit" {
                "length"
            } else {
                "stop"
            },
            output_tokens,
        })
    }

    async fn count_output_tokens(
        &self,
        text: &str,
        cancellation: CancellationToken,
        deadline: Instant,
    ) -> Result<u32> {
        #[derive(Serialize)]
        struct TokenizeRequest<'a> {
            content: &'a str,
            add_special: bool,
            parse_special: bool,
            with_pieces: bool,
        }
        #[derive(serde::Deserialize)]
        #[serde(deny_unknown_fields)]
        struct TokenizeResponse {
            tokens: Vec<u32>,
        }
        let response = tokio::select! {
            _ = cancellation.cancelled() => return Err(NodeError::Model("cancelled".into())),
            _ = sleep_until(deadline) => return Err(NodeError::Model("timeout".into())),
            response = self.client
                .post(format!("{}/tokenize", self.base_url))
                .bearer_auth(&self.api_key)
                .json(&TokenizeRequest {
                    content: text,
                    add_special: false,
                    parse_special: false,
                    with_pieces: false,
                })
                .send() => response,
        }
        .map_err(|_| NodeError::Model("local output tokenization failed".into()))?;
        if !response.status().is_success() {
            return Err(NodeError::Model("local output tokenization failed".into()));
        }
        let decoded: TokenizeResponse = tokio::select! {
            _ = cancellation.cancelled() => return Err(NodeError::Model("cancelled".into())),
            _ = sleep_until(deadline) => return Err(NodeError::Model("timeout".into())),
            decoded = response.json() => decoded,
        }
        .map_err(|_| NodeError::Model("local output tokenization response was invalid".into()))?;
        u32::try_from(decoded.tokens.len())
            .map_err(|_| NodeError::Model("output token count overflowed".into()))
    }

    async fn erase_slot(&self, slot: u32) -> Result<()> {
        let response = tokio::time::timeout(
            Duration::from_secs(10),
            self.client
                .post(format!("{}/slots/{slot}?action=erase", self.base_url))
                .bearer_auth(&self.api_key)
                .send(),
        )
        .await
        .ok()
        .and_then(std::result::Result::ok)
        .ok_or_else(|| NodeError::Model("slot cleanup failed".into()))?;
        if !response.status().is_success() {
            return Err(NodeError::Model("slot cleanup failed".into()));
        }
        Ok(())
    }

    pub async fn kill(&self) {
        #[cfg(unix)]
        {
            let _ = nix::sys::signal::killpg(
                nix::unistd::Pid::from_raw(self.process_id as i32),
                nix::sys::signal::Signal::SIGKILL,
            );
        }
        let mut process = self.process.lock().await;
        let _ = process.kill().await;
        let _ = process.wait().await;
        let _ = fs::remove_file(&self.key_file);
    }

    pub async fn is_running(&self) -> bool {
        let mut process = self.process.lock().await;
        process
            .try_wait()
            .map(|status| status.is_none())
            .unwrap_or(false)
    }
}

fn create_api_key_file(api_key: &str) -> Result<RunnerKeyFile> {
    let dir = data_dir().join("runtime");
    fs::create_dir_all(&dir)
        .map_err(|_| NodeError::Local("could not create private runner directory".into()))?;
    #[cfg(unix)]
    if std::env::var_os("CORDOVA_NODE_DATA_DIR").is_none() {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o700)).map_err(|_| {
            NodeError::Local("could not restrict runner directory permissions".into())
        })?;
    }
    let mut random = [0u8; 12];
    rand::rng().fill_bytes(&mut random);
    let path = dir.join(format!("runner-{}.key", hex::encode(random)));
    let mut options = OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options
        .open(&path)
        .map_err(|_| NodeError::Local("could not create a private runner key file".into()))?;
    let key_file = RunnerKeyFile(Some(path));
    file.write_all(api_key.as_bytes())
        .and_then(|_| file.write_all(b"\n"))
        .map_err(|_| NodeError::Local("could not write a private runner key file".into()))?;
    Ok(key_file)
}

fn verify_runner_version(binary: &Path, expected_commit: &str) -> Result<()> {
    let output = std::process::Command::new(binary)
        .arg("--version")
        .output()
        .map_err(|_| {
            NodeError::Model(
                "llama-server is unavailable; install the catalog-pinned runner".into(),
            )
        })?;
    if !output.status.success() {
        return Err(NodeError::Model("llama-server version check failed".into()));
    }
    let version = String::from_utf8_lossy(&output.stdout);
    let commit = version
        .split("commit ")
        .nth(1)
        .and_then(|tail| tail.split_ascii_whitespace().next())
        .ok_or_else(|| NodeError::Model("llama-server does not report its source commit".into()))?;
    if !runner_commit_matches(expected_commit, commit) {
        return Err(NodeError::Model(
            "installed llama-server does not match the signed catalog revision".into(),
        ));
    }
    Ok(())
}

fn runner_commit_matches(expected: &str, reported: &str) -> bool {
    expected.len() == 40
        && expected
            .bytes()
            .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase())
        && (9..=40).contains(&reported.len())
        && reported
            .bytes()
            .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase())
        && expected.starts_with(reported)
}

fn random_loopback_port() -> Result<u16> {
    let listener = TcpListener::bind(("127.0.0.1", 0))
        .map_err(|_| NodeError::Local("could not reserve a loopback port".into()))?;
    listener
        .local_addr()
        .map(|addr| addr.port())
        .map_err(|_| NodeError::Local("could not read the reserved loopback port".into()))
}

fn find_event_end(data: &[u8]) -> Option<(usize, usize)> {
    let lf = data
        .windows(2)
        .position(|window| window == b"\n\n")
        .map(|index| (index, 2));
    let crlf = data
        .windows(4)
        .position(|window| window == b"\r\n\r\n")
        .map(|index| (index, 4));
    match (lf, crlf) {
        (Some(a), Some(b)) if b.0 < a.0 => Some(b),
        (Some(a), _) => Some(a),
        (_, Some(b)) => Some(b),
        _ => None,
    }
}

fn split_utf8(text: &str, limit: usize) -> Vec<&str> {
    let mut pieces = Vec::new();
    let mut start = 0;
    let mut end = 0;
    for (index, ch) in text.char_indices() {
        let next = index + ch.len_utf8();
        if next - start > limit && end > start {
            pieces.push(&text[start..end]);
            start = index;
        }
        end = next;
    }
    if end > start {
        pieces.push(&text[start..end]);
    }
    pieces
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn splits_only_at_utf8_boundaries_and_under_protocol_bound() {
        let text = format!("{}é", "a".repeat(MAX_DELTA_BYTES));
        let pieces = split_utf8(&text, MAX_DELTA_BYTES);
        assert_eq!(pieces.len(), 2);
        assert!(pieces.iter().all(|piece| piece.len() <= MAX_DELTA_BYTES));
    }

    #[test]
    fn finds_lf_and_crlf_sse_boundaries() {
        assert_eq!(find_event_end(b"data: x\n\nrest"), Some((7, 2)));
        assert_eq!(find_event_end(b"data: x\r\n\r\nrest"), Some((7, 4)));
    }

    #[test]
    fn accepts_only_a_long_prefix_of_the_catalog_commit() {
        let expected = "d81235049abcdef0123456789abcdef012345678";
        assert!(runner_commit_matches(expected, "d81235049"));
        assert!(runner_commit_matches(expected, expected));
        assert!(!runner_commit_matches(expected, "d812350"));
        assert!(!runner_commit_matches(expected, "d81235048"));
    }
}
