use base64::{Engine, engine::general_purpose::STANDARD};
use ed25519_dalek::{Signature, Verifier, VerifyingKey};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use url::Url;

use crate::{NodeError, Result, config::Config};

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ModelDefinition {
    pub id: String,
    pub object: String,
    pub created: u64,
    pub owned_by: String,
    pub context_tokens: u32,
    pub max_output_tokens: u32,
    pub input_rate_piconero_per_million: String,
    pub output_rate_piconero_per_million: String,
    pub owner_share_bps: u16,
    pub price_version: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct CatalogManifest {
    pub model: ModelDefinition,
    pub artifact_url: String,
    pub artifact_sha256: String,
    pub artifact_size_bytes: u64,
    pub tokenizer_sha256: String,
    pub template_sha256: String,
    pub llama_commit: String,
    pub add_bos: bool,
    pub min_memory_bytes: u64,
    pub memory_per_slot_bytes: u64,
    pub license_spdx: String,
    pub version: String,
    pub signature: String,
}

impl CatalogManifest {
    pub fn verify(&self, config: &Config) -> Result<()> {
        validate_manifest_fields(self)?;
        let encoded_key = config.catalog_public_key.as_deref().ok_or_else(|| {
            NodeError::Catalog("no trusted catalog signing key is configured".into())
        })?;
        let raw_key = STANDARD
            .decode(encoded_key)
            .map_err(|_| NodeError::Catalog("catalog public key is not valid base64".into()))?;
        let key_bytes: [u8; 32] = raw_key
            .try_into()
            .map_err(|_| NodeError::Catalog("catalog public key must be 32 bytes".into()))?;
        let key = VerifyingKey::from_bytes(&key_bytes)
            .map_err(|_| NodeError::Catalog("catalog public key is invalid".into()))?;
        let signature_bytes = STANDARD
            .decode(&self.signature)
            .map_err(|_| NodeError::Catalog("manifest signature is not valid base64".into()))?;
        let signature = Signature::from_slice(&signature_bytes)
            .map_err(|_| NodeError::Catalog("manifest signature has an invalid length".into()))?;
        let mut unsigned = serde_json::to_value(self)
            .map_err(|_| NodeError::Catalog("manifest cannot be canonicalized".into()))?;
        unsigned
            .as_object_mut()
            .ok_or_else(|| NodeError::Catalog("manifest must be an object".into()))?
            .remove("signature");
        // The shared schema uses ASCII field names and integer/string values, so serde_json's sorted object keys produce stable canonical bytes.
        let canonical = serde_json::to_vec(&unsigned)
            .map_err(|_| NodeError::Catalog("manifest cannot be canonicalized".into()))?;
        key.verify(&canonical, &signature).map_err(|_| {
            NodeError::Catalog("manifest signature does not match the trusted key".into())
        })?;
        Ok(())
    }

    pub fn check_url(&self, config: &Config) -> Result<Url> {
        let url = Url::parse(&self.artifact_url)
            .map_err(|_| NodeError::Catalog("artifact URL is invalid".into()))?;
        if url.scheme() != "https"
            || url.host_str().is_none()
            || url.path().is_empty()
            || url.username() != ""
            || url.password().is_some()
            || url.fragment().is_some()
            || url.port().is_some_and(|port| port != 443)
        {
            return Err(NodeError::Catalog(
                "artifact URL must be HTTPS without embedded credentials".into(),
            ));
        }
        let host = url.host_str().unwrap_or_default().to_ascii_lowercase();
        let mut allowed = config
            .allowed_model_hosts
            .iter()
            .map(|s| s.to_ascii_lowercase())
            .collect::<Vec<_>>();
        if let Some(server) = &config.server {
            if let Ok(server) = Url::parse(server) {
                if let Some(server_host) = server.host_str() {
                    allowed.push(server_host.to_ascii_lowercase());
                }
            }
        }
        if !allowed.iter().any(|entry| entry == &host) {
            return Err(NodeError::Catalog(
                "artifact host is not in the configured model host allowlist".into(),
            ));
        }
        Ok(url)
    }

    pub fn artifact_path(&self, model_dir: &std::path::Path) -> std::path::PathBuf {
        model_dir.join(format!("{}-{}.gguf", self.model.id, self.artifact_sha256))
    }
}

fn validate_manifest_fields(manifest: &CatalogManifest) -> Result<()> {
    let model = &manifest.model;
    if model.object != "model" || model.owned_by != "cordova" {
        return Err(NodeError::Catalog("model identity is invalid".into()));
    }
    if model.id.is_empty()
        || model.id.len() > 96
        || !model
            .id
            .bytes()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'-')
    {
        return Err(NodeError::Catalog("model id is invalid".into()));
    }
    if model.context_tokens == 0
        || model.max_output_tokens == 0
        || model.max_output_tokens > model.context_tokens
        || model.owner_share_bps > 10_000
    {
        return Err(NodeError::Catalog("model limits are invalid".into()));
    }
    if !is_money(&model.input_rate_piconero_per_million)
        || !is_money(&model.output_rate_piconero_per_million)
    {
        return Err(NodeError::Catalog(
            "model rates must be canonical decimal strings".into(),
        ));
    }
    for hash in [
        &manifest.artifact_sha256,
        &manifest.tokenizer_sha256,
        &manifest.template_sha256,
    ] {
        if hash.len() != 64
            || !hash
                .bytes()
                .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase())
        {
            return Err(NodeError::Catalog(
                "manifest hashes must be lowercase SHA-256 hex".into(),
            ));
        }
    }
    if manifest.artifact_size_bytes == 0
        || manifest.artifact_size_bytes > (1u64 << 50)
        || manifest.min_memory_bytes == 0
        || manifest.memory_per_slot_bytes == 0
    {
        return Err(NodeError::Catalog(
            "artifact or memory bounds are invalid".into(),
        ));
    }
    if manifest.license_spdx.is_empty()
        || manifest.license_spdx.len() > 128
        || manifest.llama_commit.len() != 40
        || !manifest
            .llama_commit
            .bytes()
            .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase())
    {
        return Err(NodeError::Catalog(
            "license or llama.cpp revision is invalid".into(),
        ));
    }
    if manifest.version.is_empty() {
        return Err(NodeError::Catalog("manifest version is missing".into()));
    }
    Ok(())
}

fn is_money(value: &str) -> bool {
    if value == "0" {
        return true;
    }
    !value.is_empty()
        && !value.starts_with('0')
        && value.bytes().all(|c| c.is_ascii_digit())
        && value.len() <= 19
}

pub fn sha256_hex(bytes: &[u8]) -> String {
    hex::encode(Sha256::digest(bytes))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn verifies_shared_go_catalog_signature_vector() {
        let mut config = Config::default();
        config.catalog_public_key = Some("p/bfr484uJuozmSbWU+R5NAf3Ff5yUk99DteUKmYc2c=".into());
        config
            .allowed_model_hosts
            .push("models.example.test".into());
        let snapshot: crate::protocol::CatalogSnapshot = serde_json::from_str(include_str!(
            "../../contracts/fixtures/ws/node-catalog-snapshot.json"
        ))
        .expect("shared catalog fixture parses");
        assert_eq!(snapshot.version, "00000000-0000-4000-8000-000000000002");
        assert_eq!(snapshot.models.len(), 1);
        snapshot.models[0]
            .verify(&config)
            .expect("Go signature verifies in Rust");
        snapshot.models[0]
            .check_url(&config)
            .expect("fixture host is allowlisted");
    }

    #[test]
    fn accepts_only_canonical_money_strings() {
        assert!(is_money("0"));
        assert!(is_money("9000000000000000000"));
        assert!(!is_money("01"));
        assert!(!is_money("-1"));
        assert!(!is_money("1e3"));
    }

    #[test]
    fn artifact_host_requires_allowlist_and_https() {
        let mut config = Config {
            server: Some("https://api.example.test".into()),
            ..Config::default()
        };
        let mut manifest = CatalogManifest {
            model: ModelDefinition {
                id: "model-v1".into(),
                object: "model".into(),
                created: 0,
                owned_by: "cordova".into(),
                context_tokens: 4096,
                max_output_tokens: 2048,
                input_rate_piconero_per_million: "0".into(),
                output_rate_piconero_per_million: "0".into(),
                owner_share_bps: 0,
                price_version: "00000000-0000-4000-8000-000000000001".into(),
            },
            artifact_url: "https://cdn.example.test/model.gguf".into(),
            artifact_sha256: "a".repeat(64),
            artifact_size_bytes: 10,
            tokenizer_sha256: "b".repeat(64),
            template_sha256: "c".repeat(64),
            llama_commit: "abcdef0123456789abcdef0123456789abcdef01".into(),
            add_bos: false,
            min_memory_bytes: 1,
            memory_per_slot_bytes: 1,
            license_spdx: "MIT".into(),
            version: "00000000-0000-4000-8000-000000000001".into(),
            signature: String::new(),
        };
        assert!(validate_manifest_fields(&manifest).is_ok());
        assert!(manifest.check_url(&config).is_err());
        config.allowed_model_hosts.push("cdn.example.test".into());
        assert!(manifest.check_url(&config).is_ok());
        manifest.artifact_url = "http://cdn.example.test/model.gguf".into();
        assert!(manifest.check_url(&config).is_err());
        manifest.llama_commit = "abcdef0123456789".into();
        assert!(validate_manifest_fields(&manifest).is_err());
    }
}
