//! Hub REST 访问（托盘/连接探测用）。登录态复用 webview 里的会话 cookie
//! （research/07 (e)5：`cookies_for_url`），Rust 侧不持有独立凭据。

use serde::Deserialize;
use std::collections::HashMap;
use std::time::Duration;
use tauri::{AppHandle, Manager, Url};

fn client() -> reqwest::Client {
    reqwest::Client::builder()
        .timeout(Duration::from_secs(5))
        .build()
        .expect("reqwest client")
}

/// GET {hub}/healthz，3 秒超时；200 即可达。
pub async fn probe(hub_url: &str) -> Result<(), String> {
    let url = format!("{}/healthz", hub_url.trim_end_matches('/'));
    let resp = reqwest::Client::builder()
        .timeout(Duration::from_secs(3))
        .build()
        .map_err(|e| e.to_string())?
        .get(&url)
        .send()
        .await
        .map_err(|e| format!("无法连接 Hub: {e}"))?;
    if resp.status().is_success() {
        Ok(())
    } else {
        Err(format!("Hub 返回 {}", resp.status()))
    }
}

/// 从主窗口 webview 取 Hub 会话 cookie（未登录/取不到 → None）。
pub fn session_cookie(app: &AppHandle, hub_url: &str) -> Option<String> {
    let window = app.get_webview_window("main")?;
    let url: Url = hub_url.parse().ok()?;
    let cookies = window.cookies_for_url(url).ok()?;
    let pairs: Vec<String> = cookies
        .iter()
        .map(|c| format!("{}={}", c.name(), c.value()))
        .collect();
    (!pairs.is_empty()).then(|| pairs.join("; "))
}

#[derive(Deserialize)]
pub struct AppStateLite {
    #[serde(default)]
    pub active_provider_id: String,
}

#[derive(Deserialize)]
pub struct ProviderLite {
    pub id: String,
    pub name: String,
    pub protocol: String,
}

pub struct HubSnapshot {
    pub state: HashMap<String, Option<AppStateLite>>,
    pub providers: Vec<ProviderLite>,
}

/// 拉取切换所需的最小快照；任何失败（含 401）返回 Err，托盘据此降级。
pub async fn snapshot(hub_url: &str, cookie: &str) -> Result<HubSnapshot, String> {
    let base = hub_url.trim_end_matches('/');
    let cli = client();
    let state = cli
        .get(format!("{base}/api/v1/state"))
        .header("Cookie", cookie)
        .send()
        .await
        .map_err(|e| e.to_string())?
        .error_for_status()
        .map_err(|e| e.to_string())?
        .json::<HashMap<String, Option<AppStateLite>>>()
        .await
        .map_err(|e| e.to_string())?;
    let providers = cli
        .get(format!("{base}/api/v1/providers"))
        .header("Cookie", cookie)
        .send()
        .await
        .map_err(|e| e.to_string())?
        .error_for_status()
        .map_err(|e| e.to_string())?
        .json::<Vec<ProviderLite>>()
        .await
        .map_err(|e| e.to_string())?;
    Ok(HubSnapshot { state, providers })
}

/// POST /switch（托盘快切）。
pub async fn do_switch(
    hub_url: &str,
    cookie: &str,
    app: &str,
    provider_id: &str,
) -> Result<(), String> {
    let base = hub_url.trim_end_matches('/');
    client()
        .post(format!("{base}/api/v1/switch"))
        .header("Cookie", cookie)
        .json(&serde_json::json!({ "app": app, "provider_id": provider_id }))
        .send()
        .await
        .map_err(|e| e.to_string())?
        .error_for_status()
        .map_err(|e| format!("切换失败: {e}"))?;
    Ok(())
}
