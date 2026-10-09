use std::{env, fs, path::PathBuf, process::Command};

use serde::{Deserialize, Serialize};

use crate::{
    NodeError, Result,
    config::{config_dir, status_path},
};

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NodeStatus {
    pub server: Option<String>,
    pub node_id: Option<String>,
    pub connected: bool,
    pub state: String,
    pub active_slots: u32,
    pub models: Vec<ModelStatus>,
    pub last_error_code: Option<String>,
    pub updated_at: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ModelStatus {
    pub model_id: String,
    pub state: String,
    pub active_slots: u32,
    pub max_slots: u32,
}

pub fn write_status(status: &NodeStatus) -> Result<()> {
    let path = status_path();
    let parent = path
        .parent()
        .ok_or_else(|| NodeError::Local("status path has no parent".into()))?;
    fs::create_dir_all(parent)
        .map_err(|e| NodeError::Local(format!("could not create status directory: {e}")))?;
    let tmp = path.with_extension("json.tmp");
    let bytes = serde_json::to_vec_pretty(status).map_err(|e| NodeError::Local(e.to_string()))?;
    fs::write(&tmp, bytes).map_err(|e| NodeError::Local(format!("could not write status: {e}")))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&tmp, fs::Permissions::from_mode(0o600))
            .map_err(|e| NodeError::Local(format!("could not restrict status permissions: {e}")))?;
    }
    fs::rename(&tmp, path).map_err(|e| NodeError::Local(format!("could not replace status: {e}")))
}

pub fn read_status() -> Result<NodeStatus> {
    let path = status_path();
    if !path.exists() {
        return Ok(NodeStatus {
            state: "not_running".into(),
            ..NodeStatus::default()
        });
    }
    let content = fs::read(&path)
        .map_err(|e| NodeError::Local(format!("could not read node status: {e}")))?;
    let status: NodeStatus = serde_json::from_slice(&content).map_err(|_| {
        NodeError::Local("status file is invalid; remove it with status --reset".into())
    })?;
    Ok(status)
}

pub fn install_user_service() -> Result<()> {
    #[cfg(target_os = "linux")]
    return install_systemd_user();
    #[cfg(target_os = "macos")]
    return install_launch_agent();
    #[cfg(target_os = "windows")]
    {
        println!(
            "Автозапуск для Windows пока не настроен; запустите cordova-node run --foreground."
        );
        Ok(())
    }
    #[allow(unreachable_code)]
    Ok(())
}

pub fn stop_user_service() -> Result<()> {
    #[cfg(target_os = "linux")]
    {
        if command_available("systemctl") {
            let _ = Command::new("systemctl")
                .args(["--user", "disable", "--now", "cordova-node.service"])
                .status();
        }
    }
    #[cfg(target_os = "macos")]
    {
        let plist = launch_agent_path();
        if plist.exists() {
            let _ = Command::new("launchctl")
                .args(["unload", "-w"])
                .arg(&plist)
                .status();
            let _ = fs::remove_file(plist);
        }
    }
    Ok(())
}

#[cfg(target_os = "linux")]
fn install_systemd_user() -> Result<()> {
    if !command_available("systemctl")
        || !Command::new("systemctl")
            .args(["--user", "show-environment"])
            .status()
            .is_ok_and(|s| s.success())
    {
        println!(
            "Пользовательский systemd недоступен; для запуска используйте cordova-node run --foreground."
        );
        return Ok(());
    }
    let executable = env::current_exe()
        .map_err(|e| NodeError::Local(format!("could not locate node executable: {e}")))?;
    let unit_dir = env::var_os("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            env::var_os("HOME")
                .map(PathBuf::from)
                .unwrap_or_else(env::temp_dir)
                .join(".config")
        })
        .join("systemd/user");
    fs::create_dir_all(&unit_dir)
        .map_err(|e| NodeError::Local(format!("could not create systemd user directory: {e}")))?;
    let unit = format!(
        "[Unit]\nDescription=Cordova community inference node\nAfter=network-online.target\n\n[Service]\nExecStart={} run --foreground\nRestart=on-failure\nRestartSec=3\nNoNewPrivileges=true\n\n[Install]\nWantedBy=default.target\n",
        systemd_quote(executable.to_string_lossy().as_ref())
    );
    let path = unit_dir.join("cordova-node.service");
    fs::write(&path, unit)
        .map_err(|e| NodeError::Local(format!("could not write systemd user service: {e}")))?;
    let daemon = Command::new("systemctl")
        .args(["--user", "daemon-reload"])
        .status();
    let enabled = Command::new("systemctl")
        .args(["--user", "enable", "--now", "cordova-node.service"])
        .status();
    if !daemon.is_ok_and(|s| s.success()) || !enabled.is_ok_and(|s| s.success()) {
        return Err(NodeError::Local(
            "systemd service was written but could not be enabled".into(),
        ));
    }
    Ok(())
}

#[cfg(target_os = "macos")]
fn install_launch_agent() -> Result<()> {
    let executable = env::current_exe()
        .map_err(|e| NodeError::Local(format!("could not locate node executable: {e}")))?;
    let path = launch_agent_path();
    fs::create_dir_all(
        path.parent()
            .ok_or_else(|| NodeError::Local("LaunchAgent path has no parent".into()))?,
    )
    .map_err(|e| NodeError::Local(format!("could not create LaunchAgents directory: {e}")))?;
    let plist = format!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict><key>Label</key><string>com.cordova.node</string><key>ProgramArguments</key><array><string>{}</string><string>run</string><string>--foreground</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/></dict></plist>\n",
        xml_escape(executable.to_string_lossy().as_ref())
    );
    fs::write(&path, plist)
        .map_err(|e| NodeError::Local(format!("could not write LaunchAgent: {e}")))?;
    let _ = Command::new("launchctl")
        .args(["unload", "-w"])
        .arg(&path)
        .status();
    let loaded = Command::new("launchctl")
        .args(["load", "-w"])
        .arg(&path)
        .status();
    if !loaded.is_ok_and(|s| s.success()) {
        return Err(NodeError::Local(
            "LaunchAgent file was written but launchctl could not load it".into(),
        ));
    }
    Ok(())
}

#[cfg(target_os = "macos")]
fn launch_agent_path() -> PathBuf {
    env::var_os("HOME")
        .map(PathBuf::from)
        .unwrap_or_else(env::temp_dir)
        .join("Library/LaunchAgents/com.cordova.node.plist")
}

#[cfg(target_os = "linux")]
fn command_available(command: &str) -> bool {
    Command::new(command).arg("--version").output().is_ok()
}

#[cfg(target_os = "linux")]
fn systemd_quote(path: &str) -> String {
    format!(
        "\"{}\"",
        path.replace('\\', "\\\\")
            .replace('"', "\\\"")
            .replace('%', "%%")
    )
}

#[cfg(target_os = "macos")]
fn xml_escape(value: &str) -> String {
    value
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&apos;")
}

pub fn remove_status_file() {
    let _ = fs::remove_file(status_path());
}

pub fn config_directory() -> PathBuf {
    config_dir()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn service_arguments_are_escaped() {
        #[cfg(target_os = "linux")]
        assert_eq!(systemd_quote("/tmp/a b"), "\"/tmp/a b\"");
        #[cfg(target_os = "macos")]
        assert!(xml_escape("/tmp/a&b").contains("&amp;"));
    }
}
