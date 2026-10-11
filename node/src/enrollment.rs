use std::time::Duration;

use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Utc};
use hmac::{Hmac, KeyInit, Mac};
use reqwest::{Response, StatusCode};
use serde::{Deserialize, Serialize};

use crate::{
    NodeError, Result,
    config::{Config, http_client, validate_server_url},
    credentials, hardware,
};

type HmacSha256 = Hmac<sha2::Sha256>;

#[derive(Clone, Debug, Serialize, Deserialize)]
struct PendingEnrollment {
    server: String,
    device_code: String,
    user_code: String,
    verification_uri: String,
    expires_at: String,
    poll_interval_seconds: u64,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct EnrollmentCreated {
    enrollment_id: String,
    user_code: String,
    device_code: String,
    verification_uri: String,
    expires_at: String,
    poll_interval_seconds: u64,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct EnrollmentToken {
    node_id: String,
    node_secret: String,
    server_origin: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AuthChallenge {
    nonce: String,
    expires_at: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AccessToken {
    pub access_token: String,
    expires_at: String,
}

pub async fn login(config: &mut Config, server_override: Option<String>) -> Result<()> {
    if let Some(server) = server_override {
        validate_server_url(&server)?;
        config.server = Some(trim_origin(&server)?);
    }
    let server = config
        .server
        .as_deref()
        .ok_or_else(|| NodeError::Config("set --server for first enrollment".into()))?
        .to_owned();
    let origin = trim_origin(&server)?;
    config.server = Some(origin.clone());
    config.save()?;
    let client = http_client(config)?;
    let pending = match credentials::get("pending-enrollment")? {
        Some(encoded) => serde_json::from_str::<PendingEnrollment>(&encoded)
            .ok()
            .filter(|p| p.server == origin && !expired(&p.expires_at)),
        None => None,
    };
    let pending = match pending {
        Some(pending) => pending,
        None => start_enrollment(config, &client, &origin).await?,
    };
    println!("Откройте: {}", pending.verification_uri);
    println!("Введите код устройства: {}", pending.user_code);
    println!("Ожидаю подтверждения владельцем в Cordova…");
    poll_enrollment(config, &client, pending).await
}

async fn start_enrollment(
    config: &Config,
    client: &reqwest::Client,
    origin: &str,
) -> Result<PendingEnrollment> {
    let host = std::env::var("HOSTNAME")
        .or_else(|_| std::env::var("COMPUTERNAME"))
        .unwrap_or_else(|_| "node".to_owned());
    let device_name = host
        .chars()
        .filter(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_'))
        .take(64)
        .collect::<String>();
    let body = serde_json::json!({
        "hwid_hash": hardware::hwid_hash(config)?,
        "node_version": env!("CARGO_PKG_VERSION"),
        "device_name": if device_name.is_empty() { "node" } else { &device_name }
    });
    let response = client
        .post(format!("{origin}/api/node/enrollments"))
        .json(&body)
        .timeout(Duration::from_secs(30))
        .send()
        .await
        .map_err(|_| {
            NodeError::Http("enrollment request failed; check the server and proxy settings".into())
        })?;
    if response.status() != StatusCode::CREATED {
        return Err(NodeError::Http(format!(
            "enrollment failed with HTTP {}; no response body was retained",
            response.status().as_u16()
        )));
    }
    let created: EnrollmentCreated = response
        .json()
        .await
        .map_err(|_| NodeError::Http("enrollment response did not match the contract".into()))?;
    crate::protocol::validate_uuid(&created.enrollment_id)?;
    let verification_uri = validate_url_in_server_context(origin, &created.verification_uri)?;
    let expires_at = DateTime::parse_from_rfc3339(&created.expires_at)
        .map_err(|_| NodeError::Protocol("enrollment expiry is invalid".into()))?;
    if created.user_code.is_empty()
        || created.user_code.len() > 32
        || !created
            .user_code
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || c == b'-')
        || !has_256_bits(&created.device_code)
        || created.poll_interval_seconds != 5
        || expires_at <= Utc::now()
        || expires_at > Utc::now() + chrono::TimeDelta::minutes(10)
    {
        return Err(NodeError::Protocol(
            "enrollment response contained invalid bounds".into(),
        ));
    }
    let pending = PendingEnrollment {
        server: origin.to_owned(),
        device_code: created.device_code,
        user_code: created.user_code,
        verification_uri,
        expires_at: created.expires_at,
        poll_interval_seconds: created.poll_interval_seconds,
    };
    let encoded = serde_json::to_string(&pending).map_err(|e| NodeError::Local(e.to_string()))?;
    credentials::set("pending-enrollment", &encoded)?;
    Ok(pending)
}

async fn poll_enrollment(
    config: &mut Config,
    client: &reqwest::Client,
    pending: PendingEnrollment,
) -> Result<()> {
    let server = pending.server.clone();
    let url = format!("{server}/api/node/enrollments/token");
    let expires_at = DateTime::parse_from_rfc3339(&pending.expires_at)
        .map_err(|_| NodeError::Protocol("enrollment expiry is invalid".into()))?;
    loop {
        if Utc::now() >= expires_at {
            credentials::delete("pending-enrollment")?;
            return Err(NodeError::Http(
                "enrollment code expired; run login again".into(),
            ));
        }
        let response = tokio::select! {
            _ = tokio::signal::ctrl_c() => return Err(NodeError::Local("enrollment is still pending; run login again to resume".into())),
            response = client.post(&url).bearer_auth(&pending.device_code).json(&serde_json::json!({"device_code":pending.device_code})).timeout(Duration::from_secs(30)).send() => response,
        }.map_err(|_| NodeError::Http("enrollment polling failed; check the server and proxy settings".into()))?;
        if response.status() == StatusCode::PRECONDITION_REQUIRED {
            tokio::time::sleep(Duration::from_secs(pending.poll_interval_seconds)).await;
            continue;
        }
        if response.status() != StatusCode::OK {
            return Err(NodeError::Http(format!(
                "enrollment token request failed with HTTP {}",
                response.status().as_u16()
            )));
        }
        let token: EnrollmentToken = response.json().await.map_err(|_| {
            NodeError::Http("enrollment token response did not match the contract".into())
        })?;
        validate_secret_response(&token.node_id, &token.node_secret)?;
        let token_origin = trim_origin(&token.server_origin)?;
        if token_origin != pending.server {
            return Err(NodeError::Protocol(
                "enrollment response changed the trusted server origin".into(),
            ));
        }
        credentials::set(
            &credentials::node_secret_name(&token.node_id),
            &token.node_secret,
        )?;
        config.node_id = Some(token.node_id);
        config.server = Some(token_origin);
        config.save()?;
        credentials::delete("pending-enrollment")?;
        println!("Узел привязан. Ключ сохранён в системном хранилище учётных данных.");
        crate::service::install_user_service()?;
        return Ok(());
    }
}

pub async fn issue_access_token(
    config: &Config,
    node_id: &str,
    node_secret: &str,
) -> Result<AccessToken> {
    let origin = config
        .server
        .as_deref()
        .ok_or_else(|| NodeError::Config("server is not configured".into()))?;
    let client = http_client(config)?;
    let challenge_response = client
        .post(format!("{origin}/api/node/auth/challenge"))
        .json(&serde_json::json!({"node_id":node_id}))
        .timeout(Duration::from_secs(30))
        .send()
        .await
        .map_err(|_| NodeError::Http("node authentication challenge failed".into()))?;
    let challenge: AuthChallenge = auth_response(challenge_response)?
        .json()
        .await
        .map_err(|_| NodeError::Http("node authentication challenge response is invalid".into()))?;
    let expires = DateTime::parse_from_rfc3339(&challenge.expires_at)
        .map_err(|_| NodeError::Protocol("challenge expiry is invalid".into()))?;
    if !has_256_bits(&challenge.nonce)
        || expires <= Utc::now()
        || expires > Utc::now() + chrono::TimeDelta::seconds(90)
    {
        return Err(NodeError::Protocol(
            "authentication challenge expiry is outside the allowed window".into(),
        ));
    }
    let proof = auth_proof(node_secret, node_id, &challenge.nonce)?;
    let token_response = client
        .post(format!("{origin}/api/node/auth/token"))
        .json(&serde_json::json!({"node_id":node_id,"nonce":challenge.nonce,"proof":proof}))
        .timeout(Duration::from_secs(30))
        .send()
        .await
        .map_err(|_| NodeError::Http("node authentication failed".into()))?;
    let token: AccessToken = auth_response(token_response)?
        .json()
        .await
        .map_err(|_| NodeError::Http("node authentication response is invalid".into()))?;
    let token_expiry = DateTime::parse_from_rfc3339(&token.expires_at)
        .map_err(|_| NodeError::Protocol("access token expiry is invalid".into()))?;
    if token_expiry <= Utc::now()
        || token_expiry
            > Utc::now() + chrono::TimeDelta::minutes(5) + chrono::TimeDelta::seconds(30)
    {
        return Err(NodeError::Protocol(
            "access token lifetime is outside the five-minute contract".into(),
        ));
    }
    Ok(token)
}

fn auth_response(response: Response) -> Result<Response> {
    let status = response.status();
    check_auth_status(status)?;
    Ok(response)
}

fn check_auth_status(status: StatusCode) -> Result<()> {
    if status.is_success() {
        return Ok(());
    }
    if status == StatusCode::TOO_MANY_REQUESTS
        || status == StatusCode::REQUEST_TIMEOUT
        || status.is_server_error()
    {
        return Err(NodeError::Http(format!(
            "node authentication is temporarily unavailable (HTTP {})",
            status.as_u16()
        )));
    }
    Err(NodeError::AuthenticationRejected)
}

pub fn auth_proof(node_secret: &str, node_id: &str, nonce: &str) -> Result<String> {
    let secret = hex::decode(node_secret)
        .map_err(|_| NodeError::Credential("stored node secret is malformed".into()))?;
    if secret.len() != 32 {
        return Err(NodeError::Credential(
            "stored node secret is not 256 bits".into(),
        ));
    }
    let mut mac = HmacSha256::new_from_slice(&secret)
        .map_err(|_| NodeError::Credential("stored node secret is invalid".into()))?;
    mac.update(format!("cordova-node-auth-v1\n{node_id}\n{nonce}").as_bytes());
    Ok(hex::encode(mac.finalize().into_bytes()))
}

fn validate_secret_response(node_id: &str, node_secret: &str) -> Result<()> {
    crate::protocol::validate_uuid(node_id)?;
    let secret = hex::decode(node_secret)
        .map_err(|_| NodeError::Protocol("issued node secret is malformed".into()))?;
    if secret.len() != 32 {
        return Err(NodeError::Protocol(
            "issued node secret must be 256 bits".into(),
        ));
    }
    Ok(())
}

fn has_256_bits(value: &str) -> bool {
    hex::decode(value).is_ok_and(|bytes| bytes.len() >= 32)
        || URL_SAFE_NO_PAD
            .decode(value)
            .is_ok_and(|bytes| bytes.len() >= 32)
}

fn trim_origin(value: &str) -> Result<String> {
    let mut url = validate_server_url(value)?;
    url.set_path("");
    url.set_query(None);
    url.set_fragment(None);
    Ok(url.as_str().trim_end_matches('/').to_owned())
}

fn validate_url_in_server_context(server: &str, candidate: &str) -> Result<String> {
    let uri = url::Url::parse(candidate)
        .map_err(|_| NodeError::Protocol("verification URI is invalid".into()))?;
    let base =
        url::Url::parse(server).map_err(|_| NodeError::Config("server URL is invalid".into()))?;
    if !matches!(uri.scheme(), "https" | "http")
        || uri.host_str().is_none()
        || !uri.username().is_empty()
        || uri.password().is_some()
        || (uri.scheme() == "http" && base.scheme() == "https")
    {
        return Err(NodeError::Protocol("verification URI is not secure".into()));
    }
    Ok(uri.to_string())
}

fn expired(value: &str) -> bool {
    DateTime::parse_from_rfc3339(value)
        .map(|time| time <= Utc::now())
        .unwrap_or(true)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn authentication_proof_is_stable_and_bound_to_node_nonce() {
        let secret = "11".repeat(32);
        let first = auth_proof(&secret, "00000000-0000-4000-8000-000000000001", "nonce").unwrap();
        let same = auth_proof(&secret, "00000000-0000-4000-8000-000000000001", "nonce").unwrap();
        let other = auth_proof(&secret, "00000000-0000-4000-8000-000000000002", "nonce").unwrap();
        assert_eq!(first, same);
        assert_ne!(first, other);
        assert_eq!(first.len(), 64);
    }

    #[test]
    fn enrollment_codes_and_challenges_require_256_bits() {
        assert!(has_256_bits(&"11".repeat(32)));
        assert!(has_256_bits(&URL_SAFE_NO_PAD.encode([7u8; 32])));
        assert!(!has_256_bits(&"11".repeat(16)));
        assert!(!has_256_bits("not-a-random-secret"));
    }

    #[test]
    fn authentication_statuses_separate_temporary_failures_from_rejections() {
        assert!(check_auth_status(StatusCode::OK).is_ok());

        for status in [
            StatusCode::REQUEST_TIMEOUT,
            StatusCode::TOO_MANY_REQUESTS,
            StatusCode::INTERNAL_SERVER_ERROR,
            StatusCode::BAD_GATEWAY,
        ] {
            assert!(matches!(check_auth_status(status), Err(NodeError::Http(_))));
        }

        for status in [
            StatusCode::BAD_REQUEST,
            StatusCode::UNAUTHORIZED,
            StatusCode::FORBIDDEN,
        ] {
            assert!(matches!(
                check_auth_status(status),
                Err(NodeError::AuthenticationRejected)
            ));
        }
    }
}
