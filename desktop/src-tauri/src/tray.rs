//! 托盘：当前供应商展示 + 快切 + 打开主窗 + 退出。
//! 数据来源：webview 会话 cookie 调 Hub REST；未登录/Hub 不可达自动降级为最小菜单。
//! 平台差异（research/07 (d)）：Linux 无左键弹菜单，菜单为唯一入口。

use crate::{config, hub};
use std::time::Duration;
use tauri::menu::{Menu, MenuItem, PredefinedMenuItem, Submenu};
use tauri::tray::TrayIconBuilder;
use tauri::{AppHandle, Manager, Wry};

const TRAY_ID: &str = "switchapi-tray";
const APPS: [(&str, &str, &str); 2] = [
    ("claude-code", "Claude Code", "anthropic"),
    ("codex", "Codex", "openai"),
];

/// 建托盘并启动 30 秒周期的菜单刷新循环。
pub fn init(app: &AppHandle) -> tauri::Result<()> {
    let menu = minimal_menu(app)?;
    TrayIconBuilder::with_id(TRAY_ID)
        .icon(app.default_window_icon().expect("window icon").clone())
        .tooltip("switchAPI")
        .menu(&menu)
        .on_menu_event(|app, event| {
            let id = event.id().0.clone();
            handle_menu(app.clone(), id);
        })
        .build(app)?;

    let handle = app.clone();
    tauri::async_runtime::spawn(async move {
        loop {
            refresh(&handle).await;
            tokio_sleep(Duration::from_secs(30)).await;
        }
    });
    Ok(())
}

async fn tokio_sleep(d: Duration) {
    // tauri::async_runtime 基于 tokio；避免直接依赖 tokio crate。
    let (tx, rx) = std::sync::mpsc::channel::<()>();
    std::thread::spawn(move || {
        std::thread::sleep(d);
        let _ = tx.send(());
    });
    let _ = tauri::async_runtime::spawn_blocking(move || rx.recv()).await;
}

fn handle_menu(app: AppHandle, id: String) {
    match id.as_str() {
        "open" => {
            if let Some(w) = app.get_webview_window("main") {
                let _ = w.show();
                let _ = w.set_focus();
            }
        }
        "quit" => app.exit(0),
        _ => {
            // "sw|<app>|<provider_id>"
            let parts: Vec<&str> = id.splitn(3, '|').collect();
            if parts.len() == 3 && parts[0] == "sw" {
                let (target_app, pid) = (parts[1].to_string(), parts[2].to_string());
                tauri::async_runtime::spawn(async move {
                    let hub_url = config::load().hub_url;
                    if hub_url.is_empty() {
                        return;
                    }
                    if let Some(cookie) = hub::session_cookie(&app, &hub_url) {
                        if let Err(e) = hub::do_switch(&hub_url, &cookie, &target_app, &pid).await {
                            eprintln!("tray switch: {e}");
                        }
                    }
                    refresh(&app).await;
                });
            }
        }
    }
}

/// 未登录/不可达时的最小菜单。
fn minimal_menu(app: &AppHandle) -> tauri::Result<Menu<Wry>> {
    let open = MenuItem::with_id(app, "open", "打开主窗口", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "退出", true, None::<&str>)?;
    Menu::with_items(app, &[&open, &PredefinedMenuItem::separator(app)?, &quit])
}

/// 拉取 Hub 快照重建菜单；任何失败都回落最小菜单（绝不让托盘挂掉）。
pub async fn refresh(app: &AppHandle) {
    let menu = build_full_menu(app)
        .await
        .or_else(|| minimal_menu(app).ok());
    if let (Some(menu), Some(tray)) = (menu, app.tray_by_id(TRAY_ID)) {
        let _ = tray.set_menu(Some(menu));
    }
}

async fn build_full_menu(app: &AppHandle) -> Option<Menu<Wry>> {
    let hub_url = config::load().hub_url;
    if hub_url.is_empty() {
        return None;
    }
    let cookie = hub::session_cookie(app, &hub_url)?;
    let snap = hub::snapshot(&hub_url, &cookie).await.ok()?;

    let menu = Menu::new(app).ok()?;
    for (app_key, label, protocol) in APPS {
        let active_id = snap
            .state
            .get(app_key)
            .and_then(|s| s.as_ref())
            .map(|s| s.active_provider_id.clone())
            .unwrap_or_default();
        let candidates: Vec<_> = snap
            .providers
            .iter()
            .filter(|p| p.protocol == protocol)
            .collect();
        if candidates.is_empty() {
            continue;
        }
        let active_name = candidates
            .iter()
            .find(|p| p.id == active_id)
            .map(|p| p.name.as_str())
            .unwrap_or("未设置");
        let sub = Submenu::with_id(
            app,
            format!("app|{app_key}"),
            format!("{label}: {active_name}"),
            true,
        )
        .ok()?;
        for p in candidates {
            let mark = if p.id == active_id { "● " } else { "　" };
            let item = MenuItem::with_id(
                app,
                format!("sw|{app_key}|{}", p.id),
                format!("{mark}{}", p.name),
                p.id != active_id,
                None::<&str>,
            )
            .ok()?;
            sub.append(&item).ok()?;
        }
        menu.append(&sub).ok()?;
    }
    let open = MenuItem::with_id(app, "open", "打开主窗口", true, None::<&str>).ok()?;
    let quit = MenuItem::with_id(app, "quit", "退出", true, None::<&str>).ok()?;
    menu.append(&PredefinedMenuItem::separator(app).ok()?)
        .ok()?;
    menu.append(&open).ok()?;
    menu.append(&quit).ok()?;
    Some(menu)
}
