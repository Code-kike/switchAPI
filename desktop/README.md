# desktop — 桌面壳（Tauri 2）

装载**远程 Hub URL** 的薄壳（一套 SPA 双端复用，ADR-0004 / design.md §8）：

- `ui/`：本地引导页（无构建器的静态页）——首启向导（填 Hub 地址）、连接中、应急视图
  （Hub 不可达时展示本机 Agent 缓存状态，数据经 Tauri command 由 Rust 代取）。
- `src-tauri/`：壳本体——healthz 探测 + `navigate(hub)`、托盘快切（复用 webview 会话
  cookie 调 Hub REST）、single-instance、autostart（`--minimized`）、Agent 托管
  （externalBin 仅分发；首启复制到 `~/.switchapi/bin` 后 `agent install` 注册系统服务，
  启动时做版本对齐）。

## 开发

```bash
export PATH="$HOME/.cargo/bin:$PATH"
cd src-tauri && cargo build            # 编译校验（无显示器环境勿跑 tauri dev）
cargo clippy -- -D warnings
```

## 打包

`binaries/` 放入按 target-triple 命名的 Agent（如 `switchapi-agent-x86_64-unknown-linux-gnu`），
然后 `cargo tauri build`。目标：Linux AppImage / macOS dmg / Windows NSIS（per-user）。
updater 尚未启用（发布期配 minisign 签名后开 `createUpdaterArtifacts`）。

已知平台事项（research/07）：Linux 托盘无左键弹菜单；Windows 服务注册需一次 UAC
（TODO(M4)：runas 提权路径）；macOS 首启后建议登录一次以便托盘拿到会话。
