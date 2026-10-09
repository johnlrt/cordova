use std::{env, fs, fs::OpenOptions, io::Write, path::PathBuf};

use base64::Engine;
use rand::RngCore;
use serde::{Deserialize, Serialize};

use crate::{NodeError, Result};

pub const DEFAULT_MAX_DISK_GIB: u64 = 20;
pub const DEFAULT_MAX_CONCURRENCY: u8 = 1;

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Config {
    pub server: Option<String>,
    pub node_id: Option<String>,
    pub proxy: Option<String>,
    pub model_dir: PathBuf,
    pub max_disk_gib: u64,
    pub max_concurrency: u8,
    pub log_level: String,
    pub llama_server: Option<PathBuf>,
    pub catalog_public_key: Option<String>,
    pub allowed_model_hosts: Vec<String>,
    pub installation_salt: String,
}

impl Default for Config {
    fn default() -> Self {
        let mut salt = [0u8; 32];
        rand::rng().fill_bytes(&mut salt);
        Self {
            server: None,
            node_id: None,
            proxy: None,
            model_dir: data_dir().join("models"),
            max_disk_gib: DEFAULT_MAX_DISK_GIB,
            max_concurrency: DEFAULT_MAX_CONCURRENCY,
            log_level: "info".to_owned(),
            llama_server: None,
            catalog_public_key: None,
            allowed_model_hosts: Vec::new(),
            installation_salt: hex::encode(salt),
        }
    }
}

pub fn config_dir() -> PathBuf {
    if let Some(path) = env::var_os("CORDOVA_NODE_CONFIG_DIR") {
        return PathBuf::from(path);
    }
    #[cfg(target_os = "windows")]
    let base = env::var_os("APPDATA")
        .map(PathBuf::from)
        .unwrap_or_else(env::temp_dir);
    #[cfg(target_os = "macos")]
    let base = env::var_os("HOME")
        .map(PathBuf::from)
        .unwrap_or_else(env::temp_dir)
        .join("Library/Application Support");
    #[cfg(all(unix, not(target_os = "macos")))]
    let base = env::var_os("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            env::var_os("HOME")
                .map(PathBuf::from)
                .unwrap_or_else(env::temp_dir)
                .join(".config")
        });
    base.join("cordova-node")
}

pub fn data_dir() -> PathBuf {
    if let Some(path) = env::var_os("CORDOVA_NODE_DATA_DIR") {
        return PathBuf::from(path);
    }
    #[cfg(target_os = "windows")]
    let base = env::var_os("LOCALAPPDATA")
        .map(PathBuf::from)
        .unwrap_or_else(env::temp_dir);
    #[cfg(target_os = "macos")]
    let base = env::var_os("HOME")
        .map(PathBuf::from)
        .unwrap_or_else(env::temp_dir)
        .join("Library/Application Support");
    #[cfg(all(unix, not(target_os = "macos")))]
    let base = env::var_os("XDG_DATA_HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            env::var_os("HOME")
                .map(PathBuf::from)
                .unwrap_or_else(env::temp_dir)
                .join(".local/share")
        });
    base.join("cordova-node")
}

pub fn status_path() -> PathBuf {
    config_dir().join("status.json")
}

impl Config {
    pub fn path() -> PathBuf {
        config_dir().join("config.json")
    }

    pub fn load() -> Result<Self> {
        let path = Self::path();
        let mut config = if path.exists() {
            let bytes = fs::read(&path)
                .map_err(|e| NodeError::Local(format!("could not read {}: {e}", path.display())))?;
            serde_json::from_slice(&bytes)
                .map_err(|e| NodeError::Config(format!("{}: {e}", path.display())))?
        } else {
            Self::default()
        };
        if let Ok(value) = env::var("CORDOVA_NODE_SERVER") {
            let origin = validate_server_url(&value)?.origin().ascii_serialization();
            let configured_origin = config
                .server
                .as_deref()
                .map(validate_server_url)
                .transpose()?
                .map(|server| server.origin().ascii_serialization());
            if config.node_id.is_some() && configured_origin.as_deref() != Some(&origin) {
                return Err(NodeError::Config(
                    "CORDOVA_NODE_SERVER cannot change the server after enrollment".into(),
                ));
            }
            config.server = Some(origin);
        }
        if let Ok(value) = env::var("CORDOVA_NODE_PROXY") {
            config.proxy = Some(value);
        }
        if let Ok(value) = env::var("CORDOVA_NODE_MODEL_DIR") {
            config.model_dir = PathBuf::from(value);
        }
        if let Ok(value) = env::var("CORDOVA_NODE_MAX_DISK_GIB") {
            config.max_disk_gib = parse_env(&value, "CORDOVA_NODE_MAX_DISK_GIB")?;
        }
        if let Ok(value) = env::var("CORDOVA_NODE_MAX_CONCURRENCY") {
            config.max_concurrency = parse_env(&value, "CORDOVA_NODE_MAX_CONCURRENCY")?;
        }
        if let Ok(value) = env::var("CORDOVA_NODE_LOG_LEVEL") {
            config.log_level = value;
        }
        if let Ok(value) = env::var("CORDOVA_NODE_LLAMA_SERVER") {
            config.llama_server = Some(PathBuf::from(value));
        }
        if let Ok(value) = env::var("CORDOVA_NODE_CATALOG_PUBLIC_KEY") {
            config.catalog_public_key = Some(value);
        }
        if let Ok(value) = env::var("CORDOVA_NODE_ALLOWED_MODEL_HOSTS") {
            config.allowed_model_hosts = value
                .split(',')
                .map(|host| host.trim().to_ascii_lowercase())
                .filter(|host| !host.is_empty())
                .collect();
        }
        config.validate()?;
        Ok(config)
    }

    pub fn save(&self) -> Result<()> {
        self.validate()?;
        let dir = config_dir();
        fs::create_dir_all(&dir)
            .map_err(|e| NodeError::Local(format!("could not create config directory: {e}")))?;
        #[cfg(unix)]
        if env::var_os("CORDOVA_NODE_CONFIG_DIR").is_none() {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&dir, fs::Permissions::from_mode(0o700)).map_err(|_| {
                NodeError::Local("could not restrict config directory permissions".into())
            })?;
        }
        let path = Self::path();
        let data = serde_json::to_vec_pretty(self).map_err(|e| NodeError::Local(e.to_string()))?;
        let mut random = [0u8; 12];
        rand::rng().fill_bytes(&mut random);
        let tmp = dir.join(format!(".config-{}.tmp", hex::encode(random)));
        let mut options = OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut file = options.open(&tmp).map_err(|_| {
            NodeError::Local("could not create a private temporary config file".into())
        })?;
        if file.write_all(&data).and_then(|_| file.sync_all()).is_err() {
            let _ = fs::remove_file(&tmp);
            return Err(NodeError::Local("could not write config".into()));
        }
        drop(file);
        if let Err(error) = fs::rename(&tmp, &path) {
            let _ = fs::remove_file(&tmp);
            return Err(NodeError::Local(format!(
                "could not replace config: {error}"
            )));
        }
        Ok(())
    }

    pub fn validate(&self) -> Result<()> {
        if self.max_disk_gib == 0 || self.max_disk_gib > 4096 {
            return Err(NodeError::Config(
                "max_disk_gib must be between 1 and 4096".into(),
            ));
        }
        if self.max_concurrency == 0 || self.max_concurrency > 64 {
            return Err(NodeError::Config(
                "max_concurrency must be between 1 and 64".into(),
            ));
        }
        if !matches!(self.log_level.as_str(), "error" | "warn" | "info" | "debug") {
            return Err(NodeError::Config(
                "log_level must be error, warn, info, or debug".into(),
            ));
        }
        if let Some(proxy) = &self.proxy {
            let parsed = url::Url::parse(proxy)
                .map_err(|_| NodeError::Config("proxy must be a valid socks5h:// URL".into()))?;
            if parsed.scheme() != "socks5h"
                || parsed.host_str().is_none()
                || parsed.port().is_none()
                || !parsed.username().is_empty()
                || parsed.password().is_some()
            {
                return Err(NodeError::Config(
                    "proxy must use socks5h://host:port".into(),
                ));
            }
        }
        if let Some(server) = &self.server {
            validate_server_url(server)?;
        }
        if let Some(node_id) = &self.node_id {
            crate::protocol::validate_uuid(node_id)?;
            if self.server.is_none() {
                return Err(NodeError::Config(
                    "an enrolled node must retain its server origin".into(),
                ));
            }
        }
        let salt = hex::decode(&self.installation_salt).map_err(|_| {
            NodeError::Config("installation salt must be lowercase hexadecimal".into())
        })?;
        if self.installation_salt.len() != 64
            || self
                .installation_salt
                .bytes()
                .any(|c| c.is_ascii_uppercase())
            || salt.len() != 32
        {
            return Err(NodeError::Config(
                "installation salt must be 256 bits of lowercase hex".into(),
            ));
        }
        if let Some(encoded_key) = &self.catalog_public_key {
            let key = base64::engine::general_purpose::STANDARD
                .decode(encoded_key)
                .map_err(|_| NodeError::Config("catalog public key must be base64".into()))?;
            if key.len() != 32 {
                return Err(NodeError::Config(
                    "catalog public key must be 32 bytes".into(),
                ));
            }
        }
        Ok(())
    }
}

pub fn validate_server_url(value: &str) -> Result<url::Url> {
    let url = url::Url::parse(value)
        .map_err(|_| NodeError::Config("server must be an http(s) URL".into()))?;
    if !matches!(url.scheme(), "https" | "http")
        || url.host_str().is_none()
        || url.path() != "/"
        || url.query().is_some()
        || url.fragment().is_some()
        || !url.username().is_empty()
        || url.password().is_some()
    {
        return Err(NodeError::Config(
            "server must be an http(s) origin without query or fragment".into(),
        ));
    }
    if url.scheme() == "http"
        && !url.host_str().is_some_and(|h| {
            h == "localhost"
                || h.parse::<std::net::IpAddr>()
                    .is_ok_and(|ip| ip.is_loopback())
        })
    {
        return Err(NodeError::Config(
            "unencrypted HTTP is allowed only for loopback development".into(),
        ));
    }
    Ok(url)
}

pub fn http_client(config: &Config) -> Result<reqwest::Client> {
    let mut builder = reqwest::Client::builder()
        .no_proxy()
        .redirect(reqwest::redirect::Policy::none())
        .https_only(
            config
                .server
                .as_ref()
                .is_some_and(|s| s.starts_with("https://")),
        );
    if let Some(proxy) = &config.proxy {
        let mut parsed = url::Url::parse(proxy)
            .map_err(|_| NodeError::Config("invalid SOCKS5h proxy URL".into()))?;
        let username = crate::credentials::get("proxy-username")?;
        let password = crate::credentials::get("proxy-password")?;
        match (username, password) {
            (Some(username), Some(password)) => {
                parsed
                    .set_username(&username)
                    .map_err(|_| NodeError::Config("proxy username is invalid".into()))?;
                parsed
                    .set_password(Some(&password))
                    .map_err(|_| NodeError::Config("proxy password is invalid".into()))?;
            }
            (None, None) => {}
            _ => {
                return Err(NodeError::Credential(
                    "proxy credentials are incomplete in the OS credential store".into(),
                ));
            }
        }
        let proxy = reqwest::Proxy::all(parsed.as_str())
            .map_err(|_| NodeError::Config("invalid SOCKS5h proxy URL".into()))?;
        builder = builder.proxy(proxy);
    }
    builder
        .build()
        .map_err(|_| NodeError::Config("could not initialize HTTP client".into()))
}

fn parse_env<T: std::str::FromStr>(value: &str, name: &str) -> Result<T> {
    value
        .parse()
        .map_err(|_| NodeError::Config(format!("{name} is invalid")))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_remote_plain_http_and_non_socks_proxy() {
        assert!(validate_server_url("http://example.com").is_err());
        assert!(validate_server_url("https://user:pass@example.com").is_err());
        assert!(validate_server_url("https://example.com/base").is_err());
        let config = Config {
            proxy: Some("socks5://localhost:9050".into()),
            ..Config::default()
        };
        assert!(config.validate().is_err());
        assert!(validate_server_url("http://127.0.0.1:8080").is_ok());
    }
}
