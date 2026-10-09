use keyring::Entry;

use crate::{NodeError, Result};

const SERVICE: &str = "cordova-node";

fn entry(name: &str) -> Result<Entry> {
    Entry::new(SERVICE, name)
        .map_err(|e| NodeError::Credential(format!("OS credential store: {e}")))
}

pub fn get(name: &str) -> Result<Option<String>> {
    match entry(name)?.get_password() {
        Ok(value) => Ok(Some(value)),
        Err(keyring::Error::NoEntry) => Ok(None),
        Err(error) => Err(NodeError::Credential(format!(
            "OS credential store: {error}"
        ))),
    }
}

pub fn set(name: &str, value: &str) -> Result<()> {
    entry(name)?
        .set_password(value)
        .map_err(|e| NodeError::Credential(format!("OS credential store: {e}")))
}

pub fn delete(name: &str) -> Result<()> {
    match entry(name)?.delete_credential() {
        Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
        Err(error) => Err(NodeError::Credential(format!(
            "OS credential store: {error}"
        ))),
    }
}

pub fn node_secret_name(node_id: &str) -> String {
    format!("node-secret:{node_id}")
}
