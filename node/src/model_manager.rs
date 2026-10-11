use std::{
    collections::{HashMap, HashSet},
    net::{IpAddr, SocketAddr},
    path::{Path, PathBuf},
    sync::Arc,
};

use fs2::available_space;
use futures_util::StreamExt;
use reqwest::{
    StatusCode,
    header::{ACCEPT_ENCODING, CONTENT_RANGE, RANGE},
};
use serde::Serialize;
use sha2::{Digest, Sha256};
use tokio::{
    fs::{self, File, OpenOptions},
    io::{AsyncReadExt, AsyncWriteExt},
    net::lookup_host,
    sync::{Mutex, RwLock},
};

use crate::{
    NodeError, Result,
    catalog::CatalogManifest,
    config::Config,
    llama::LlamaRunner,
    protocol::{CatalogSnapshot, NodeModel, validate_uuid},
};

#[derive(Clone, Debug, Serialize)]
pub struct ModelStatePayload {
    pub model_id: String,
    pub state: String,
    pub artifact_sha256: Option<String>,
    pub active_slots: u32,
    pub max_slots: u32,
    pub reason_code: Option<String>,
    pub available_disk_bytes: u64,
}

pub struct ModelManager {
    config: Config,
    catalog_version: RwLock<Option<String>>,
    manifests: RwLock<HashMap<String, CatalogManifest>>,
    runners: Mutex<HashMap<String, Arc<LlamaRunner>>>,
    download_gate: Mutex<()>,
}

impl ModelManager {
    pub fn new(config: Config) -> Self {
        Self {
            config,
            catalog_version: RwLock::new(None),
            manifests: RwLock::new(HashMap::new()),
            runners: Mutex::new(HashMap::new()),
            download_gate: Mutex::new(()),
        }
    }

    pub async fn install_catalog(&self, snapshot: CatalogSnapshot) -> Result<()> {
        validate_uuid(&snapshot.version)?;
        let mut next = HashMap::new();
        for manifest in snapshot.models {
            manifest.verify(&self.config)?;
            manifest.check_url(&self.config)?;
            validate_uuid(&manifest.version)?;
            validate_uuid(&manifest.model.price_version)?;
            if next.insert(manifest.model.id.clone(), manifest).is_some() {
                return Err(NodeError::Catalog(
                    "catalog contains duplicate model IDs".into(),
                ));
            }
        }
        *self.manifests.write().await = next;
        *self.catalog_version.write().await = Some(snapshot.version);
        Ok(())
    }

    pub async fn catalog_version(&self) -> Option<String> {
        self.catalog_version.read().await.clone()
    }

    pub async fn manifest(&self, model_id: &str, version: &str) -> Result<CatalogManifest> {
        let current = self.catalog_version.read().await.clone();
        if current.as_deref() != Some(version) {
            return Err(NodeError::Catalog("catalog version is stale".into()));
        }
        self.manifests
            .read()
            .await
            .get(model_id)
            .cloned()
            .ok_or_else(|| NodeError::Catalog("model is not in the signed catalog".into()))
    }

    pub async fn state(&self, model_id: &str) -> Result<ModelStatePayload> {
        let manifest = self
            .manifests
            .read()
            .await
            .get(model_id)
            .cloned()
            .ok_or_else(|| NodeError::Catalog("model is not in the signed catalog".into()))?;
        let root = prepare_model_dir(&self.config.model_dir).await?;
        let available_disk_bytes = available_space(&root).unwrap_or(0).min((1u64 << 53) - 1);
        let runners = self.runners.lock().await;
        if let Some(runner) = runners.get(model_id) {
            if runner.is_running().await {
                return Ok(ModelStatePayload {
                    model_id: model_id.to_owned(),
                    state: "loaded".into(),
                    artifact_sha256: Some(runner.artifact_sha256.clone()),
                    active_slots: runner.active_slots().await,
                    max_slots: runner.max_slots(),
                    reason_code: None,
                    available_disk_bytes,
                });
            }
        }
        drop(runners);
        let path = manifest.artifact_path(&root);
        if regular_file(&path)? {
            Ok(ModelStatePayload {
                model_id: model_id.to_owned(),
                state: "on_disk".into(),
                artifact_sha256: Some(manifest.artifact_sha256),
                active_slots: 0,
                max_slots: 0,
                reason_code: None,
                available_disk_bytes,
            })
        } else {
            let max_cache_bytes = self
                .config
                .max_disk_gib
                .checked_mul(1024 * 1024 * 1024)
                .unwrap_or(0);
            Ok(ModelStatePayload {
                model_id: model_id.to_owned(),
                state: "absent".into(),
                artifact_sha256: None,
                active_slots: 0,
                max_slots: 0,
                reason_code: (manifest.artifact_size_bytes > max_cache_bytes)
                    .then(|| "disk_full".to_owned()),
                available_disk_bytes,
            })
        }
    }

    pub async fn verified_state(&self, model_id: &str) -> Result<ModelStatePayload> {
        let state = self.state(model_id).await?;
        if state.state != "on_disk" {
            return Ok(state);
        }
        let manifest = self
            .manifests
            .read()
            .await
            .get(model_id)
            .cloned()
            .ok_or_else(|| NodeError::Catalog("model is not in the signed catalog".into()))?;
        let path = manifest.artifact_path(&self.config.model_dir);
        if hash_file(&path).await? == manifest.artifact_sha256 {
            Ok(state)
        } else {
            Ok(ModelStatePayload {
                model_id: model_id.to_owned(),
                state: "absent".into(),
                artifact_sha256: None,
                active_slots: 0,
                max_slots: 0,
                reason_code: Some("catalog_mismatch".into()),
                available_disk_bytes: state.available_disk_bytes,
            })
        }
    }

    pub async fn node_models(&self) -> Vec<NodeModel> {
        let ids = self
            .manifests
            .read()
            .await
            .keys()
            .cloned()
            .collect::<Vec<_>>();
        let mut models = Vec::new();
        for id in ids {
            if let Ok(state) = self.state(&id).await {
                models.push(NodeModel {
                    model_id: id,
                    state: match state.state.as_str() {
                        "on_disk" => "on_disk",
                        "loaded" => "loaded",
                        _ => "absent",
                    }
                    .to_owned(),
                    active_slots: state.active_slots,
                    max_slots: state.max_slots,
                    verified_capacity_bytes: 0,
                    updated_at: chrono::Utc::now().to_rfc3339(),
                });
            }
        }
        models.sort_by(|a, b| a.model_id.cmp(&b.model_id));
        models
    }

    pub async fn download(
        &self,
        model_id: &str,
        catalog_version: &str,
    ) -> Result<(CatalogManifest, PathBuf)> {
        let _download_guard = self.download_gate.lock().await;
        let manifest = self.manifest(model_id, catalog_version).await?;
        let url = manifest.check_url(&self.config)?;
        self.download_inner(&manifest, url)
            .await
            .map(|path| (manifest, path))
    }

    async fn download_inner(&self, manifest: &CatalogManifest, url: url::Url) -> Result<PathBuf> {
        let root = prepare_model_dir(&self.config.model_dir).await?;
        let destination = manifest.artifact_path(&root);
        if regular_file(&destination)? {
            if hash_file(&destination).await? == manifest.artifact_sha256 {
                return Ok(destination);
            }
            return Err(NodeError::Model(
                "cached model hash does not match the signed catalog".into(),
            ));
        }
        crate::hardware::verify_manifest_headroom(
            manifest.min_memory_bytes,
            manifest.memory_per_slot_bytes,
            1,
        )?;
        validate_public_destination(&url, self.config.proxy.is_some()).await?;
        let max_bytes = self
            .config
            .max_disk_gib
            .checked_mul(1024 * 1024 * 1024)
            .ok_or_else(|| NodeError::Config("model cache limit overflowed".into()))?;
        if manifest.artifact_size_bytes > max_bytes {
            return Err(NodeError::Model("disk_full".into()));
        }
        let part = destination.with_extension("gguf.part");
        self.evict_idle_models(&root, &part, manifest.artifact_size_bytes, max_bytes)
            .await?;
        let free = available_space(&root).map_err(|_| NodeError::Model("disk_full".into()))?;
        if free < manifest.artifact_size_bytes {
            return Err(NodeError::Model("disk_full".into()));
        }

        if symlink_or_non_file(&part)? {
            return Err(NodeError::Model("catalog_mismatch".into()));
        }
        let offset = if part.exists() {
            fs::metadata(&part)
                .await
                .map_err(|_| NodeError::Model("download_failed".into()))?
                .len()
        } else {
            0
        };
        if offset > manifest.artifact_size_bytes {
            fs::remove_file(&part)
                .await
                .map_err(|_| NodeError::Model("download_failed".into()))?;
        }
        let offset = if offset > manifest.artifact_size_bytes {
            0
        } else {
            offset
        };
        let client = crate::config::http_client(&self.config)?;
        let mut request = client.get(url).header(ACCEPT_ENCODING, "identity");
        if offset > 0 {
            request = request.header(RANGE, format!("bytes={offset}-"));
        }
        let response = request
            .send()
            .await
            .map_err(|_| NodeError::Model("download_failed".into()))?;
        if offset == 0 {
            if response.status() != StatusCode::OK {
                return Err(NodeError::Model("download_failed".into()));
            }
        } else {
            if response.status() != StatusCode::PARTIAL_CONTENT {
                return Err(NodeError::Model("download_failed".into()));
            }
            let range = response
                .headers()
                .get(CONTENT_RANGE)
                .and_then(|h| h.to_str().ok())
                .unwrap_or_default();
            if !range.starts_with(&format!("bytes {offset}-")) {
                return Err(NodeError::Model("download_failed".into()));
            }
        }
        let mut hasher = Sha256::new();
        if offset > 0 {
            let mut previous = File::open(&part)
                .await
                .map_err(|_| NodeError::Model("download_failed".into()))?;
            let mut prefix = vec![0u8; 64 * 1024];
            loop {
                let read = previous
                    .read(&mut prefix)
                    .await
                    .map_err(|_| NodeError::Model("download_failed".into()))?;
                if read == 0 {
                    break;
                }
                hasher.update(&prefix[..read]);
            }
        }
        let mut output = OpenOptions::new()
            .create(true)
            .append(true)
            .open(&part)
            .await
            .map_err(|_| NodeError::Model("download_failed".into()))?;
        let mut stream = response.bytes_stream();
        let mut received = offset;
        while let Some(chunk) = stream.next().await {
            let chunk = chunk.map_err(|_| NodeError::Model("download_failed".into()))?;
            received = received
                .checked_add(chunk.len() as u64)
                .ok_or_else(|| NodeError::Model("download_failed".into()))?;
            if received > manifest.artifact_size_bytes {
                return Err(NodeError::Model("download_failed".into()));
            }
            hasher.update(&chunk);
            output
                .write_all(&chunk)
                .await
                .map_err(|_| NodeError::Model("disk_full".into()))?;
        }
        output
            .flush()
            .await
            .map_err(|_| NodeError::Model("disk_full".into()))?;
        output
            .sync_all()
            .await
            .map_err(|_| NodeError::Model("disk_full".into()))?;
        if received != manifest.artifact_size_bytes
            || hex::encode(hasher.finalize()) != manifest.artifact_sha256
        {
            let _ = fs::remove_file(&part).await;
            return Err(NodeError::Model("catalog_mismatch".into()));
        }
        fs::rename(&part, &destination)
            .await
            .map_err(|_| NodeError::Model("download_failed".into()))?;
        Ok(destination)
    }

    async fn evict_idle_models(
        &self,
        root: &Path,
        new_path: &Path,
        new_size: u64,
        max_bytes: u64,
    ) -> Result<()> {
        let active = self
            .runners
            .lock()
            .await
            .values()
            .map(|runner| (runner.model_id.clone(), runner.artifact_sha256.clone()))
            .collect::<HashSet<_>>();
        let mut files = Vec::new();
        let mut total = 0u64;
        let mut entries = fs::read_dir(root)
            .await
            .map_err(|_| NodeError::Model("disk_full".into()))?;
        while let Some(entry) = entries
            .next_entry()
            .await
            .map_err(|_| NodeError::Model("disk_full".into()))?
        {
            let path = entry.path();
            let meta = entry
                .metadata()
                .await
                .map_err(|_| NodeError::Model("disk_full".into()))?;
            let identity = path
                .file_name()
                .and_then(|n| n.to_str())
                .unwrap_or_default();
            if !meta.is_file()
                || path == new_path
                || (path.extension().is_none_or(|ext| ext != "gguf")
                    && !identity.ends_with(".gguf.part"))
            {
                continue;
            }
            let artifact_identity = identity.strip_suffix(".part").unwrap_or(identity);
            if let Some((id, hash)) = parse_artifact_name(artifact_identity) {
                if active.contains(&(id.to_owned(), hash.to_owned())) {
                    total = total.saturating_add(meta.len());
                    continue;
                }
            }
            if path.extension().is_none_or(|ext| ext != "gguf") && !identity.ends_with(".gguf.part")
            {
                continue;
            }
            total = total.saturating_add(meta.len());
            files.push((meta.modified().ok(), path, meta.len()));
        }
        if total.saturating_add(new_size) <= max_bytes {
            return Ok(());
        }
        files.sort_by_key(|(modified, _, _)| *modified);
        for (_, path, size) in files {
            if total.saturating_add(new_size) <= max_bytes {
                break;
            }
            fs::remove_file(path)
                .await
                .map_err(|_| NodeError::Model("disk_full".into()))?;
            total = total.saturating_sub(size);
        }
        if total.saturating_add(new_size) > max_bytes {
            return Err(NodeError::Model("disk_full".into()));
        }
        Ok(())
    }

    pub async fn load(
        &self,
        model_id: &str,
        catalog_version: &str,
    ) -> Result<(CatalogManifest, Arc<LlamaRunner>)> {
        let manifest = self.manifest(model_id, catalog_version).await?;
        manifest.verify(&self.config)?;
        let path = manifest.artifact_path(&self.config.model_dir);
        if !regular_file(&path)? || hash_file(&path).await? != manifest.artifact_sha256 {
            return Err(NodeError::Model("catalog_mismatch".into()));
        }
        let mut runners = self.runners.lock().await;
        if let Some(runner) = runners.get(model_id) {
            if runner.artifact_sha256 == manifest.artifact_sha256 && runner.is_running().await {
                return Ok((manifest, runner.clone()));
            }
            runner.kill().await;
            runners.remove(model_id);
        }
        let runner = LlamaRunner::start(&self.config, &manifest, &path).await?;
        runners.insert(model_id.to_owned(), runner.clone());
        Ok((manifest, runner))
    }

    pub async fn runner(&self, model_id: &str, catalog_version: &str) -> Result<Arc<LlamaRunner>> {
        let (_, runner) = self.load(model_id, catalog_version).await?;
        Ok(runner)
    }

    pub async fn loaded_runner(
        &self,
        model_id: &str,
        catalog_version: &str,
    ) -> Result<Arc<LlamaRunner>> {
        let _ = self.manifest(model_id, catalog_version).await?;
        let runners = self.runners.lock().await;
        let runner = runners
            .get(model_id)
            .cloned()
            .ok_or_else(|| NodeError::Model("model_not_found".into()))?;
        if !runner.is_running().await {
            return Err(NodeError::Model("model_not_found".into()));
        }
        Ok(runner)
    }

    pub async fn shutdown(&self) {
        let runners = std::mem::take(&mut *self.runners.lock().await);
        for runner in runners.into_values() {
            runner.kill().await;
        }
    }

    pub async fn evict(&self, model_id: &str) -> Result<()> {
        let mut runners = self.runners.lock().await;
        if let Some(runner) = runners.get(model_id) {
            if runner.active_slots().await > 0 {
                return Err(NodeError::Model("busy".into()));
            }
            runner.kill().await;
            runners.remove(model_id);
        }
        let manifest = self.manifests.read().await.get(model_id).cloned();
        if let Some(manifest) = manifest {
            let path = manifest.artifact_path(&self.config.model_dir);
            if regular_file(&path)? {
                fs::remove_file(path)
                    .await
                    .map_err(|_| NodeError::Model("download_failed".into()))?;
            }
        }
        Ok(())
    }
}

async fn prepare_model_dir(path: &Path) -> Result<PathBuf> {
    fs::create_dir_all(path)
        .await
        .map_err(|_| NodeError::Local("could not create model cache directory".into()))?;
    fs::canonicalize(path)
        .await
        .map_err(|_| NodeError::Local("could not resolve model cache directory".into()))
}

fn regular_file(path: &Path) -> Result<bool> {
    match std::fs::symlink_metadata(path) {
        Ok(meta) if meta.file_type().is_symlink() => {
            Err(NodeError::Model("catalog_mismatch".into()))
        }
        Ok(meta) => Ok(meta.is_file()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(_) => Err(NodeError::Model("download_failed".into())),
    }
}

fn symlink_or_non_file(path: &Path) -> Result<bool> {
    match std::fs::symlink_metadata(path) {
        Ok(meta) => Ok(meta.file_type().is_symlink() || !meta.is_file()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(_) => Err(NodeError::Model("download_failed".into())),
    }
}

async fn hash_file(path: &Path) -> Result<String> {
    let mut file = File::open(path)
        .await
        .map_err(|_| NodeError::Model("download_failed".into()))?;
    let mut hasher = Sha256::new();
    let mut buffer = vec![0u8; 1024 * 1024];
    loop {
        let read = file
            .read(&mut buffer)
            .await
            .map_err(|_| NodeError::Model("download_failed".into()))?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    Ok(hex::encode(hasher.finalize()))
}

async fn validate_public_destination(url: &url::Url, proxy_enabled: bool) -> Result<()> {
    let host = url
        .host_str()
        .ok_or_else(|| NodeError::Catalog("artifact URL has no host".into()))?;
    if let Ok(ip) = host.parse::<IpAddr>() {
        if !is_public_ip(ip) {
            return Err(NodeError::Catalog(
                "artifact URL resolves to a non-public address".into(),
            ));
        }
        return Ok(());
    }
    if proxy_enabled {
        return Ok(());
    }
    let port = url
        .port_or_known_default()
        .ok_or_else(|| NodeError::Catalog("artifact URL has no port".into()))?;
    let addresses: Vec<SocketAddr> = lookup_host((host, port))
        .await
        .map_err(|_| NodeError::Model("download_failed".into()))?
        .collect();
    if addresses.is_empty() || addresses.iter().any(|address| !is_public_ip(address.ip())) {
        return Err(NodeError::Catalog(
            "artifact host resolves to a non-public address".into(),
        ));
    }
    Ok(())
}

fn is_public_ip(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(ip) => {
            let [a, b, c, _] = ip.octets();
            !ip.is_private()
                && !ip.is_loopback()
                && !ip.is_link_local()
                && !ip.is_broadcast()
                && !ip.is_unspecified()
                && !ip.is_multicast()
                && !(a == 0
                    || a >= 240
                    || (a == 100 && (64..=127).contains(&b))
                    || (a == 192 && b == 0 && c == 0)
                    || (a == 192 && b == 88 && c == 99)
                    || (a == 198 && (b == 18 || b == 19))
                    || (a == 198 && b == 51 && c == 100)
                    || (a == 203 && b == 0 && c == 113))
        }
        IpAddr::V6(ip) => {
            let segments = ip.segments();
            let global_unicast = segments[0] & 0xe000 == 0x2000;
            let special_use = (segments[0] == 0x2001
                && (segments[1] <= 0x01ff || segments[1] == 0x0db8))
                || segments[0] == 0x2002
                || (segments[0] == 0x3fff && segments[1] & 0xf000 == 0);
            global_unicast
                && !special_use
                && !ip.is_loopback()
                && !ip.is_unspecified()
                && !ip.is_multicast()
                && !ip.is_unique_local()
                && !ip.is_unicast_link_local()
                && ip
                    .to_ipv4_mapped()
                    .map(|v4| is_public_ip(IpAddr::V4(v4)))
                    .unwrap_or(true)
        }
    }
}

fn parse_artifact_name(file: &str) -> Option<(&str, &str)> {
    let base = file.strip_suffix(".gguf")?;
    let (id, hash) = base.rsplit_once('-')?;
    if hash.len() == 64 && hash.bytes().all(|c| c.is_ascii_hexdigit()) {
        Some((id, hash))
    } else {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn blocks_private_and_documentation_ranges() {
        for address in [
            "127.0.0.1",
            "10.0.0.1",
            "169.254.1.1",
            "192.0.0.1",
            "198.51.100.1",
            "203.0.113.1",
            "::1",
            "fc00::1",
            "2001:db8::1",
            "2002::1",
            "3fff::1",
        ] {
            assert!(!is_public_ip(address.parse().unwrap()), "{address}");
        }
        assert!(is_public_ip("8.8.8.8".parse().unwrap()));
        assert!(is_public_ip("2001:4860:4860::8888".parse().unwrap()));
    }

    #[test]
    fn artifact_filename_has_only_model_and_hash() {
        assert_eq!(
            parse_artifact_name(&format!("model-v1-{}.gguf", "a".repeat(64))),
            Some((
                "model-v1",
                "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
            ))
        );
        assert_eq!(parse_artifact_name("../../model.gguf"), None);
    }
}
