use crate::config::Config;
use sha2::{Digest, Sha256};
use std::process::Command;

use crate::{NodeError, Result, protocol::Hardware};

pub fn detect() -> Result<Hardware> {
    let (platform, arch) = platform_arch()?;
    let ram_bytes = detect_ram_bytes().unwrap_or(0);
    let gpu = nvidia_gpu();
    let (backend, gpu_name, total, free) = match gpu {
        Some((name, total, free)) => ("cuda".to_owned(), Some(name), total, free),
        None => match metal_gpu() {
            Some(name) => ("metal".to_owned(), Some(name), 0, 0),
            None => ("cpu".to_owned(), None, 0, 0),
        },
    };
    Ok(Hardware {
        platform,
        arch,
        backend,
        gpu_name,
        reported_total_vram_bytes: total,
        reported_free_vram_bytes: free,
        ram_bytes,
    })
}

fn platform_arch() -> Result<(String, String)> {
    let platform = if cfg!(target_os = "windows") {
        "windows"
    } else if cfg!(target_os = "macos") {
        "macos"
    } else if cfg!(target_os = "linux") {
        "linux"
    } else {
        return Err(NodeError::Config(
            "this operating system is not supported".into(),
        ));
    }
    .to_owned();
    let arch = match std::env::consts::ARCH {
        "x86_64" => "x86_64",
        "aarch64" => "aarch64",
        _ => {
            return Err(NodeError::Config(
                "this CPU architecture is not supported".into(),
            ));
        }
    }
    .to_owned();
    Ok((platform, arch))
}

#[cfg(target_os = "macos")]
fn metal_gpu() -> Option<String> {
    let output = Command::new("system_profiler")
        .args(["SPDisplaysDataType"])
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    let text = String::from_utf8(output.stdout).ok()?;
    text.lines()
        .find_map(|line| {
            line.trim()
                .strip_prefix("Chipset Model:")
                .or_else(|| line.trim().strip_prefix("Chip:"))
        })
        .map(|name| {
            name.chars()
                .filter(|c| !c.is_control())
                .take(128)
                .collect::<String>()
        })
        .filter(|name| !name.is_empty())
}

#[cfg(not(target_os = "macos"))]
fn metal_gpu() -> Option<String> {
    None
}

fn detect_ram_bytes() -> Option<u64> {
    if let Ok(value) = std::env::var("CORDOVA_NODE_RAM_BYTES") {
        return value.parse().ok();
    }
    #[cfg(target_os = "macos")]
    {
        let output = Command::new("sysctl")
            .args(["-n", "hw.memsize"])
            .output()
            .ok()?;
        return String::from_utf8(output.stdout).ok()?.trim().parse().ok();
    }
    #[cfg(target_os = "linux")]
    {
        let text = std::fs::read_to_string("/proc/meminfo").ok()?;
        let value = text
            .lines()
            .find_map(|line| line.strip_prefix("MemTotal:"))?
            .split_whitespace()
            .next()?
            .parse::<u64>()
            .ok()?;
        return value.checked_mul(1024);
    }
    #[cfg(target_os = "windows")]
    {
        let output = Command::new("powershell.exe")
            .args([
                "-NoProfile",
                "-NonInteractive",
                "-Command",
                "(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory",
            ])
            .output()
            .ok()?;
        return String::from_utf8(output.stdout).ok()?.trim().parse().ok();
    }
    #[allow(unreachable_code)]
    None
}

fn nvidia_gpu() -> Option<(String, u64, u64)> {
    let output = Command::new("nvidia-smi")
        .args([
            "--query-gpu=name,memory.total,memory.free",
            "--format=csv,noheader,nounits",
        ])
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    let line = String::from_utf8(output.stdout)
        .ok()?
        .lines()
        .next()?
        .to_owned();
    let mut fields = line.split(',').map(str::trim);
    let name = fields
        .next()?
        .chars()
        .filter(|c| !c.is_control())
        .take(128)
        .collect::<String>();
    let total_mib = fields.next()?.parse::<u64>().ok()?;
    let free_mib = fields.next()?.parse::<u64>().ok()?;
    Some((
        name,
        total_mib.saturating_mul(1024 * 1024),
        free_mib.saturating_mul(1024 * 1024),
    ))
}

pub fn local_capacity_bytes() -> Result<u64> {
    let hardware = detect()?;
    Ok(available_ram_bytes()
        .unwrap_or(0)
        .saturating_add(hardware.reported_free_vram_bytes))
}

pub fn hwid_hash(config: &Config) -> Result<String> {
    let hardware = detect()?;
    hwid_hash_for(config, &hardware)
}

pub fn hwid_hash_for(config: &Config, hardware: &Hardware) -> Result<String> {
    let host = std::env::var("HOSTNAME")
        .or_else(|_| std::env::var("COMPUTERNAME"))
        .unwrap_or_else(|_| "node".to_owned());
    let fingerprint = format!(
        "{}|{}|{}|{}|{}",
        host.trim().to_lowercase(),
        hardware.platform,
        hardware.arch,
        hardware.backend,
        hardware.ram_bytes
    );
    let mut hasher = Sha256::new();
    hasher.update(
        hex::decode(&config.installation_salt)
            .map_err(|_| NodeError::Config("installation salt is invalid".into()))?,
    );
    hasher.update(fingerprint.as_bytes());
    Ok(hex::encode(hasher.finalize()))
}

fn available_ram_bytes() -> Option<u64> {
    #[cfg(target_os = "linux")]
    {
        let text = std::fs::read_to_string("/proc/meminfo").ok()?;
        return text
            .lines()
            .find_map(|line| line.strip_prefix("MemAvailable:"))?
            .split_whitespace()
            .next()?
            .parse::<u64>()
            .ok()?
            .checked_mul(1024);
    }
    #[cfg(target_os = "macos")]
    {
        let output = Command::new("vm_stat").output().ok()?;
        let text = String::from_utf8(output.stdout).ok()?;
        let page_size = text
            .split("page size of ")
            .nth(1)?
            .split_ascii_whitespace()
            .next()?
            .parse::<u64>()
            .ok()?;
        let mut pages = 0u64;
        for label in [
            "Pages free:",
            "Pages inactive:",
            "Pages speculative:",
            "Pages purgeable:",
        ] {
            if let Some(value) = text
                .lines()
                .find_map(|line| line.trim().strip_prefix(label))
            {
                pages =
                    pages.saturating_add(value.trim().trim_end_matches('.').parse::<u64>().ok()?);
            }
        }
        return pages.checked_mul(page_size);
    }
    #[cfg(target_os = "windows")]
    {
        let output = Command::new("powershell.exe")
            .args([
                "-NoProfile",
                "-NonInteractive",
                "-Command",
                "(Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory",
            ])
            .output()
            .ok()?;
        return String::from_utf8(output.stdout)
            .ok()?
            .trim()
            .parse::<u64>()
            .ok()?
            .checked_mul(1024);
    }
    #[allow(unreachable_code)]
    None
}

pub fn verify_manifest_headroom(
    min_memory_bytes: u64,
    memory_per_slot_bytes: u64,
    slots: u8,
) -> Result<u64> {
    let required = min_memory_bytes
        .checked_add(memory_per_slot_bytes.saturating_mul(u64::from(slots)))
        .ok_or_else(|| NodeError::Model("model memory requirement overflowed".into()))?;
    if required == 0 {
        return Err(NodeError::Model(
            "model memory requirement is invalid".into(),
        ));
    }
    let available = local_capacity_bytes()?;
    if available < required {
        return Err(NodeError::Model("insufficient_memory".into()));
    }
    Ok(available)
}
