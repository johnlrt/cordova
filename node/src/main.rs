#![forbid(unsafe_code)]

use std::path::PathBuf;

use base64::Engine;
use clap::{Args, Parser, Subcommand};
use serde::Serialize;

use cordova_node::{
    NodeError, Result,
    config::{Config, validate_server_url},
    credentials, enrollment, service, transport,
};

#[derive(Parser)]
#[command(
    name = "cordova-node",
    version,
    about = "Cordova community inference node"
)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Link this device to an owner account.
    Login(LoginArgs),
    /// Connect to Cordova and serve signed catalog models.
    Run(RunArgs),
    /// Show node runtime state without credentials.
    Status {
        #[arg(long)]
        reset: bool,
    },
    /// Remove local node credentials and stop the user service.
    Logout,
    /// Inspect or update non-secret node configuration.
    Config {
        #[command(subcommand)]
        command: ConfigCommand,
    },
}

#[derive(Args)]
struct LoginArgs {
    /// Cordova server origin; required for first enrollment only.
    #[arg(long)]
    server: Option<String>,
    /// SOCKS5h proxy; destination DNS is resolved by the proxy.
    #[arg(long)]
    proxy: Option<String>,
    #[arg(long)]
    model_dir: Option<PathBuf>,
    #[arg(long)]
    max_disk_gib: Option<u64>,
    #[arg(long)]
    max_concurrency: Option<u8>,
    #[arg(long)]
    log_level: Option<String>,
}

#[derive(Args)]
struct RunArgs {
    /// Keep the node in the foreground. This is the default for this command.
    #[arg(long)]
    foreground: bool,
    /// SOCKS5h proxy; destination DNS is resolved by the proxy.
    #[arg(long)]
    proxy: Option<String>,
    #[arg(long)]
    model_dir: Option<PathBuf>,
    #[arg(long)]
    max_disk_gib: Option<u64>,
    #[arg(long)]
    max_concurrency: Option<u8>,
    #[arg(long)]
    log_level: Option<String>,
}

#[derive(Subcommand)]
enum ConfigCommand {
    Show,
    Set { key: String, value: String },
    Unset { key: String },
    Path,
}

#[derive(Serialize)]
struct SafeConfig<'a> {
    server: &'a Option<String>,
    node_id: &'a Option<String>,
    proxy: &'a Option<String>,
    model_dir: &'a PathBuf,
    max_disk_gib: u64,
    max_concurrency: u8,
    log_level: &'a str,
    llama_server: &'a Option<PathBuf>,
    catalog_public_key_configured: bool,
    allowed_model_hosts: &'a [String],
}

#[tokio::main]
async fn main() {
    let result = execute(Cli::parse()).await;
    if let Err(error) = result {
        eprintln!("Ошибка: {error}");
        std::process::exit(1);
    }
}

async fn execute(cli: Cli) -> Result<()> {
    match cli.command {
        Command::Login(args) => {
            let mut config = Config::load()?;
            apply_login_overrides(&mut config, args)?;
            enrollment::login(&mut config, None).await
        }
        Command::Run(args) => {
            let mut config = Config::load()?;
            apply_run_overrides(&mut config, args)?;
            transport::run_forever(config).await
        }
        Command::Status { reset } => {
            if reset {
                service::remove_status_file();
                println!("Статус удалён.");
            } else {
                println!(
                    "{}",
                    serde_json::to_string_pretty(&service::read_status()?)
                        .map_err(|e| NodeError::Local(e.to_string()))?
                );
            }
            Ok(())
        }
        Command::Logout => logout().await,
        Command::Config { command } => config_command(command),
    }
}

fn apply_login_overrides(config: &mut Config, args: LoginArgs) -> Result<()> {
    if let Some(server) = args.server {
        let origin = validate_server_url(&server)?;
        let origin = origin.origin().ascii_serialization();
        let configured_origin = config
            .server
            .as_deref()
            .map(validate_server_url)
            .transpose()?
            .map(|server| server.origin().ascii_serialization());
        if config.node_id.is_some() && configured_origin.as_deref() != Some(origin.as_str()) {
            return Err(NodeError::Config(
                "the --server option is only available for first enrollment".into(),
            ));
        }
        config.server = Some(origin);
    }
    if let Some(proxy) = args.proxy {
        config.proxy = Some(proxy);
    }
    if let Some(path) = args.model_dir {
        config.model_dir = path;
    }
    if let Some(value) = args.max_disk_gib {
        config.max_disk_gib = value;
    }
    if let Some(value) = args.max_concurrency {
        config.max_concurrency = value;
    }
    if let Some(value) = args.log_level {
        config.log_level = value;
    }
    config.validate()?;
    config.save()
}

fn apply_run_overrides(config: &mut Config, args: RunArgs) -> Result<()> {
    let _foreground = args.foreground;
    if let Some(proxy) = args.proxy {
        config.proxy = Some(proxy);
    }
    if let Some(path) = args.model_dir {
        config.model_dir = path;
    }
    if let Some(value) = args.max_disk_gib {
        config.max_disk_gib = value;
    }
    if let Some(value) = args.max_concurrency {
        config.max_concurrency = value;
    }
    if let Some(value) = args.log_level {
        config.log_level = value;
    }
    config.validate()
}

fn config_command(command: ConfigCommand) -> Result<()> {
    let mut config = Config::load()?;
    match command {
        ConfigCommand::Show => {
            let safe = SafeConfig {
                server: &config.server,
                node_id: &config.node_id,
                proxy: &config.proxy,
                model_dir: &config.model_dir,
                max_disk_gib: config.max_disk_gib,
                max_concurrency: config.max_concurrency,
                log_level: &config.log_level,
                llama_server: &config.llama_server,
                catalog_public_key_configured: config.catalog_public_key.is_some(),
                allowed_model_hosts: &config.allowed_model_hosts,
            };
            println!(
                "{}",
                serde_json::to_string_pretty(&safe).map_err(|e| NodeError::Local(e.to_string()))?
            );
        }
        ConfigCommand::Path => println!("{}", Config::path().display()),
        ConfigCommand::Set { key, value } => {
            match key.as_str() {
                "server" => {
                    let parsed = validate_server_url(&value)?;
                    let origin = parsed.origin().ascii_serialization();
                    let configured_origin = config
                        .server
                        .as_deref()
                        .map(validate_server_url)
                        .transpose()?
                        .map(|server| server.origin().ascii_serialization());
                    if config.node_id.is_some() && configured_origin.as_deref() != Some(&origin) {
                        return Err(NodeError::Config(
                            "server cannot be changed after node enrollment".into(),
                        ));
                    }
                    config.server = Some(origin);
                }
                "proxy" => config.proxy = Some(value),
                "model-dir" => config.model_dir = PathBuf::from(value),
                "max-disk-gib" => config.max_disk_gib = value.parse().map_err(|_| NodeError::Config("max-disk-gib must be an integer".into()))?,
                "max-concurrency" => config.max_concurrency = value.parse().map_err(|_| NodeError::Config("max-concurrency must be an integer".into()))?,
                "log-level" => config.log_level = value,
                "llama-server" => config.llama_server = Some(PathBuf::from(value)),
                "catalog-public-key" => {
                    let decoded = base64::engine::general_purpose::STANDARD.decode(&value).map_err(|_| NodeError::Config("catalog public key must be base64".into()))?;
                    if decoded.len() != 32 { return Err(NodeError::Config("catalog public key must be 32 bytes".into())); }
                    config.catalog_public_key = Some(value);
                }
                "allowed-model-hosts" => config.allowed_model_hosts = value.split(',').map(|v| v.trim().to_ascii_lowercase()).filter(|v| !v.is_empty()).collect(),
                "proxy-username" => { credentials::set("proxy-username", &value)?; println!("Учётные данные прокси сохранены в системном хранилище."); return Ok(()); }
                "proxy-password" => { credentials::set("proxy-password", &value)?; println!("Учётные данные прокси сохранены в системном хранилище."); return Ok(()); }
                _ => return Err(NodeError::Config("supported keys: server, proxy, model-dir, max-disk-gib, max-concurrency, log-level, llama-server, catalog-public-key, allowed-model-hosts, proxy-username, proxy-password".into())),
            }
            config.validate()?;
            config.save()?;
            println!("Настройка сохранена.");
        }
        ConfigCommand::Unset { key } => {
            match key.as_str() {
                "server" => {
                    if config.node_id.is_some() {
                        return Err(NodeError::Config(
                            "server cannot be removed after node enrollment".into(),
                        ));
                    }
                    config.server = None;
                }
                "proxy" => config.proxy = None,
                "llama-server" => config.llama_server = None,
                "catalog-public-key" => config.catalog_public_key = None,
                "allowed-model-hosts" => config.allowed_model_hosts.clear(),
                "proxy-credentials" => {
                    credentials::delete("proxy-username")?;
                    credentials::delete("proxy-password")?;
                    println!("Учётные данные прокси удалены из системного хранилища.");
                    return Ok(());
                }
                _ => return Err(NodeError::Config("only server, proxy, llama-server, catalog-public-key, allowed-model-hosts, or proxy-credentials can be unset".into())),
            }
            config.save()?;
            println!("Настройка удалена.");
        }
    }
    Ok(())
}

async fn logout() -> Result<()> {
    let mut config = Config::load()?;
    service::stop_user_service()?;
    if let Some(node_id) = config.node_id.take() {
        credentials::delete(&credentials::node_secret_name(&node_id))?;
    }
    credentials::delete("pending-enrollment")?;
    config.save()?;
    service::remove_status_file();
    println!(
        "Локальные учётные данные удалены. Для отзыва узла попросите владельца выполнить revoke в панели Cordova."
    );
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn config_display_does_not_include_hwid_salt_or_secret_values() {
        let config = Config::default();
        let safe = SafeConfig {
            server: &config.server,
            node_id: &config.node_id,
            proxy: &config.proxy,
            model_dir: &config.model_dir,
            max_disk_gib: config.max_disk_gib,
            max_concurrency: config.max_concurrency,
            log_level: &config.log_level,
            llama_server: &config.llama_server,
            catalog_public_key_configured: config.catalog_public_key.is_some(),
            allowed_model_hosts: &config.allowed_model_hosts,
        };
        let text = serde_json::to_string(&safe).unwrap();
        assert!(!text.contains(&config.installation_salt));
        assert!(!text.contains("proxy-password"));
    }
}
