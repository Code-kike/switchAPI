//! 本机 Agent 托管（research/07 (b)/(c)）：externalBin 只是分发载体——
//! 首启把包内二进制复制到稳定路径 ~/.switchapi/bin/，再以用户上下文执行
//! `switchapi-agent install|start|stop`（kardianos 注册系统服务）。
//! 壳每次启动做版本对齐：包内版本 ≠ 已装版本 → 停→覆盖→启。

use crate::config::state_dir;
use serde::Deserialize;
use std::collections::BTreeMap;
use std::path::PathBuf;
use std::process::Command;
use std::{fs, io};

const BIN_NAME: &str = if cfg!(windows) {
    "switchapi-agent.exe"
} else {
    "switchapi-agent"
};

/// 已安装（稳定路径）的 Agent 二进制。
pub fn installed_path() -> PathBuf {
    state_dir().join("bin").join(BIN_NAME)
}

/// 随包分发的 Agent 二进制：永远以 current_exe 所在目录为锚
/// （macOS Contents/MacOS、AppImage 挂载点 usr/bin、NSIS 安装目录——
/// tauri-bundler 都把 externalBin 放在主程序旁边）。
pub fn bundled_path() -> Option<PathBuf> {
    let exe = std::env::current_exe().ok()?;
    let p = exe.parent()?.join(BIN_NAME);
    p.is_file().then_some(p)
}

fn run(bin: &PathBuf, args: &[&str]) -> Result<String, String> {
    let out = Command::new(bin)
        .args(args)
        .output()
        .map_err(|e| format!("执行 {} 失败: {e}", bin.display()))?;
    let text = format!(
        "{}{}",
        String::from_utf8_lossy(&out.stdout),
        String::from_utf8_lossy(&out.stderr)
    );
    if out.status.success() {
        Ok(text)
    } else {
        Err(if text.trim().is_empty() {
            format!(
                "{} {:?} 退出码 {:?}",
                bin.display(),
                args,
                out.status.code()
            )
        } else {
            text
        })
    }
}

fn version_of(bin: &PathBuf) -> Option<String> {
    run(bin, &["-version"]).ok().map(|s| s.trim().to_string())
}

/// 把包内二进制复制到稳定路径（覆盖旧副本），返回目标路径。
fn copy_to_stable() -> Result<PathBuf, String> {
    let src = bundled_path()
        .ok_or("安装包内未找到 switchapi-agent（开发模式下请手动放置或用 CLI 安装）")?;
    let dst = installed_path();
    fs::create_dir_all(dst.parent().unwrap()).map_err(|e| e.to_string())?;
    fs::copy(&src, &dst).map_err(|e: io::Error| format!("复制 Agent 失败: {e}"))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = fs::set_permissions(&dst, fs::Permissions::from_mode(0o755));
    }
    Ok(dst)
}

/// install / start / stop。install = 复制 + 注册服务 + 启动。
/// Windows 注意（research/07 (c)）：SCM 注册需要一次 UAC 提权——
/// TODO(M4)：以 runas 动词重启 `switchapi-agent install` 或在安装器钩子内完成；
/// 当前直接执行，无提权时由 Agent 返回明确错误。
pub fn ctl(action: &str) -> Result<String, String> {
    match action {
        "install" => {
            let bin = copy_to_stable()?;
            let mut log = run(&bin, &["install"])?;
            log.push_str(&run(&bin, &["start"]).unwrap_or_default());
            Ok(log)
        }
        "start" | "stop" => {
            let bin = installed_path();
            if !bin.is_file() {
                return Err("Agent 尚未安装，请先执行安装".into());
            }
            run(&bin, &[action])
        }
        _ => Err(format!("未知操作: {action}")),
    }
}

/// 版本对齐（壳启动/自更新后调用）：包内 ≠ 已装 → 停 → 覆盖 → 启。
/// 任一步不可用（无包内二进制/未安装过）都静默跳过——这只是尽力而为的对齐。
pub fn sync_version() -> Option<String> {
    let bundled = bundled_path()?;
    let installed = installed_path();
    if !installed.is_file() {
        return None;
    }
    let (bv, iv) = (version_of(&bundled)?, version_of(&installed)?);
    if bv == iv {
        return None;
    }
    let _ = run(&installed, &["stop"]);
    copy_to_stable().ok()?;
    let _ = run(&installed, &["start"]);
    Some(format!("Agent 已从 {iv} 对齐到 {bv}"))
}

// ---- 状态汇总（应急页展示；字段白名单，绝不触碰 token/api_key） ----

#[derive(Deserialize)]
struct StateWhitelist {
    #[serde(default)]
    hub_url: String,
    #[serde(default)]
    device_id: String,
    #[serde(default)]
    saved_at: i64,
    #[serde(default)]
    last_push: Option<PushLite>,
}

#[derive(Deserialize)]
struct PushLite {
    #[serde(default)]
    rev: i64,
    #[serde(default)]
    apps: BTreeMap<String, RouteLite>,
}

#[derive(Deserialize)]
struct RouteLite {
    #[serde(default)]
    name: String,
    #[serde(default)]
    protocol: String,
}

/// 人读状态文本：agent-state.json 摘要 + `switchapi-agent status` 输出。
pub fn status_text() -> String {
    let mut lines = Vec::new();

    match fs::read(state_dir().join("agent-state.json")) {
        Ok(raw) => match serde_json::from_slice::<StateWhitelist>(&raw) {
            Ok(st) => {
                lines.push(format!("设备 ID: {}", st.device_id));
                lines.push(format!("Hub: {}", st.hub_url));
                if let Some(push) = st.last_push {
                    lines.push(format!("缓存路由 rev {}:", push.rev));
                    for (app, r) in push.apps {
                        lines.push(format!("  {app} → {} ({})", r.name, r.protocol));
                    }
                }
                if st.saved_at > 0 {
                    lines.push(format!("快照时间戳: {}", st.saved_at));
                }
            }
            Err(e) => lines.push(format!("agent-state.json 解析失败: {e}")),
        },
        Err(_) => lines.push("本机尚未配对（无 agent-state.json）".into()),
    }

    let bin = installed_path();
    if bin.is_file() {
        lines.push(String::new());
        match run(&bin, &["status"]) {
            Ok(out) => lines.push(format!("服务状态: {}", out.trim())),
            Err(e) => lines.push(format!("服务状态查询失败: {}", e.trim())),
        }
    } else {
        lines.push("Agent 未安装到 ~/.switchapi/bin".into());
    }
    lines.join("\n")
}
