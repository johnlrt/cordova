#![forbid(unsafe_code)]

pub mod catalog;
pub mod config;
pub mod credentials;
pub mod enrollment;
pub mod hardware;
pub mod llama;
pub mod model_manager;
pub mod protocol;
pub mod service;
pub mod transport;

use thiserror::Error;

#[derive(Debug, Error)]
pub enum NodeError {
    #[error("configuration is invalid: {0}")]
    Config(String),
    #[error("credential storage is unavailable: {0}")]
    Credential(String),
    #[error("server request failed: {0}")]
    Http(String),
    #[error("WebSocket connection failed: {0}")]
    WebSocket(String),
    #[error("protocol message is invalid: {0}")]
    Protocol(String),
    #[error("catalog verification failed: {0}")]
    Catalog(String),
    #[error("model operation failed: {0}")]
    Model(String),
    #[error("local operation failed: {0}")]
    Local(String),
}

pub type Result<T> = std::result::Result<T, NodeError>;
