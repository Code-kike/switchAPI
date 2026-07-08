//! desktop.json 配置（~/.switchapi/desktop.json）：目前只有 Hub 地址。

use serde::{Deserialize, Serialize};
use std::{fs, path::PathBuf};

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct DesktopConfig {
    #[serde(default)]
    pub hub_url: String,
}

/// Agent 与桌面壳共用的状态目录。
pub fn state_dir() -> PathBuf {
    dirs::home_dir()
        .unwrap_or_else(|| PathBuf::from("."))
        .join(".switchapi")
}

fn config_path() -> PathBuf {
    state_dir().join("desktop.json")
}

pub fn load() -> DesktopConfig {
    fs::read(config_path())
        .ok()
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default()
}

pub fn save(cfg: &DesktopConfig) -> Result<(), String> {
    let dir = state_dir();
    fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = fs::set_permissions(&dir, fs::Permissions::from_mode(0o700));
    }
    let data = serde_json::to_vec_pretty(cfg).map_err(|e| e.to_string())?;
    fs::write(config_path(), data).map_err(|e| e.to_string())
}
