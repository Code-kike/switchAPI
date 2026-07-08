//! switchAPI 桌面壳（Tauri 2）——装载远程 Hub 控制台的薄壳（design.md §4）：
//! 本地引导页 → healthz 探测 → navigate(Hub)；托盘快切；Agent 托管与版本对齐；
//! 应急页数据由 Rust 命令代取（不开 CORS）。远程页面不调用 Tauri IPC，
//! 故无需注入 remote capability（research/07 (e)——如未来需要，见 connect 内注释）。

#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod agent;
mod config;
mod hub;
mod tray;

use config::DesktopConfig;
use tauri::{AppHandle, Manager, Url, WindowEvent};

#[tauri::command]
fn get_config() -> DesktopConfig {
    config::load()
}

#[tauri::command]
fn set_config(cfg: DesktopConfig) -> Result<(), String> {
    config::save(&cfg)
}

/// 探测 → 落盘配置 → 把主窗口导航到 Hub。失败返回 Err，前端进应急视图。
#[tauri::command]
async fn connect(app: AppHandle, hub_url: String) -> Result<(), String> {
    let hub_url = hub_url.trim_end_matches('/').to_string();
    hub::probe(&hub_url).await?;
    config::save(&DesktopConfig {
        hub_url: hub_url.clone(),
    })?;

    // 若未来 Hub 托管的 SPA 需要调用 Tauri IPC（当前不需要），在此注入运行时
    // remote capability（research/07 (e)2）：
    //   app.add_capability(
    //       tauri::ipc::CapabilityBuilder::new("hub")
    //           .remote(&hub_url).window("main").permission("core:default"),
    //   )?;

    let window = app
        .get_webview_window("main")
        .ok_or("main window missing")?;
    let url: Url = hub_url.parse().map_err(|e| format!("Hub 地址无效: {e}"))?;
    window.navigate(url).map_err(|e| e.to_string())?;

    // 登录态就绪后托盘即可拉到数据；立即刷新一次。
    tauri::async_runtime::spawn(async move { tray::refresh(&app).await });
    Ok(())
}

#[tauri::command]
fn agent_status() -> String {
    agent::status_text()
}

#[tauri::command]
fn agent_ctl(action: String) -> Result<String, String> {
    agent::ctl(&action)
}

fn main() {
    let start_minimized = std::env::args().any(|a| a == "--minimized");

    tauri::Builder::default()
        // single-instance 必须最先注册：二开实例把焦点交还既有窗口。
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| {
            if let Some(w) = app.get_webview_window("main") {
                let _ = w.show();
                let _ = w.set_focus();
            }
        }))
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            Some(vec!["--minimized"]),
        ))
        .invoke_handler(tauri::generate_handler![
            get_config,
            set_config,
            connect,
            agent_status,
            agent_ctl
        ])
        .setup(move |app| {
            tray::init(app.handle())?;

            if start_minimized {
                if let Some(w) = app.get_webview_window("main") {
                    let _ = w.hide();
                }
            }

            // 版本对齐（尽力而为）：包内 Agent 比已装副本新 → 停→覆盖→启。
            tauri::async_runtime::spawn_blocking(|| {
                if let Some(msg) = agent::sync_version() {
                    println!("{msg}");
                }
            });

            // 已配置过 Hub 的常规路径由引导页 JS 触发 connect；这里无需处理。
            Ok(())
        })
        .on_window_event(|window, event| {
            // 托盘常驻应用范式：点关闭 = 隐藏到托盘，退出走托盘菜单。
            if let WindowEvent::CloseRequested { api, .. } = event {
                api.prevent_close();
                let _ = window.hide();
            }
        })
        .run(tauri::generate_context!())
        .expect("error while running switchAPI desktop");
}
