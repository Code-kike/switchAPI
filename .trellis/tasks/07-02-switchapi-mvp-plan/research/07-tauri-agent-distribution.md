# Research: Tauri 2 桌面壳分发与托管 Agent 二进制（研究#7）

- **Query**: prd.md 研究项 #7 — Tauri 2 sidecar/自更新与 Agent 二进制分发方案（含 Windows 服务注册）；design.md §8 `[研究#7]`
- **Scope**: mixed（本机 cargo registry 源码 + 本机 cc-switch 仓库 + 官方文档/仓库）
- **Date**: 2026-07-03
- **主要本地证据**: `~/.cargo/registry/.../tauri-2.10.3`、`tauri-utils-2.8.3`、`wry-0.54.3`、`tauri-plugin-updater-2.10.0`、`/home/orion/Workspace/github/cc-switch-web`（真实 Tauri 2 生产应用）

---

## 结论

### (a) sidecar / externalBin 机制 —— 全部确认（置信度 High）

1. **命名规则**：`bundle.externalBin` 配置一组路径（相对 `src-tauri/`），构建时按 `名称-$TARGET_TRIPLE[.exe]` 查找，如 `binaries/agent` → `src-tauri/binaries/agent-x86_64-unknown-linux-gnu`。所有目标平台的三元组二进制都要预先放好（源码级确认：tauri-utils config.rs L1386-1397 + 官方 sidecar 文档）。**High**
2. **打包落点**（tauri-bundler 源码逐个确认，`-triple` 后缀在打包时被剥掉，装好后就叫 `agent`）：
   - macOS：`<App>.app/Contents/MacOS/`（与主二进制同目录）
   - deb/rpm：`/usr/bin/`
   - AppImage：镜像内 `usr/bin/`（复用 debian::generate_data）——**注意：AppImage 运行时挂载在随机 `/tmp/.mount_*` 只读路径**
   - NSIS/MSI：安装目录，与主 exe 同层。**High**
3. **运行时解析**：`app.shell().sidecar("agent")` = `当前 exe 所在目录/agent[.exe]`（shell 插件 `relative_command_path` 源码确认）。只传文件名，不传配置里的相对路径。**High**
4. **权限**：从 **JS** 调 spawn/execute 需 capability 里加 `shell:allow-execute` 或 `shell:allow-spawn` 且 `{"name": "binaries/agent", "sidecar": true}`；从 **Rust** 侧 `shell().sidecar()` 不经过 ACL，无需 capability。**High**
5. **关键生命周期事实**：shell 插件在 `RunEvent::Exit` 时**杀死所有它 spawn 的子进程**（plugins-workspace shell/src/lib.rs L132-142 源码确认）——sidecar 方式拉起的进程**不会在桌面壳退出后存活**。**High**

### (b) 核心问题：sidecar 拉起 vs 随包分发 + `agent install` 一次性注册 —— 明确结论：后者

两种方案诚实对比：

| 维度 | A. shell 插件 spawn sidecar 常驻 | B. externalBin 只做分发，首启复制到稳定路径 + `agent install` 注册系统服务 |
|---|---|---|
| 桌面壳退出后 Agent 存活 | ✘ 被 RunEvent::Exit 杀死（源码确认） | ✔ 由 systemd/launchd/SCM 管理 |
| 开机自启（不开桌面壳） | ✘ 依赖壳先启动 | ✔ 服务管理器负责 |
| 崩溃自动拉起 | ✘ 需壳自己实现监督 | ✔ KeepAlive/Restart=on-failure |
| AppImage 路径稳定性 | ✘ 挂载点每次随机，服务单元无法指向包内 | ✔ 复制出来的路径稳定 |
| Windows 更新时文件锁 | ✘ 服务若直接跑安装目录内的 exe，NSIS/MSI 覆盖会被锁 | ✔ 服务跑副本，安装目录可自由覆盖 |
| 分发便利性 | ✔ | ✔（同样用 externalBin 装进安装包，只是不用 shell spawn） |

**结论（High）**：`externalBin` 是正确的**分发**载体（自动处理 target-triple、保留可执行位、进各平台安装包），但**不是**正确的**运行**载体。正确流程：

```
桌面壳首启（或检测到 Agent 未装/版本落后）：
  1. 定位 <当前exe目录>/switchapi-agent（macOS 为 Contents/MacOS/，AppImage 为挂载点 usr/bin/）
  2. 复制到稳定目录（建议 ~/.switchapi/bin/ 或平台等价物）
  3. 以用户上下文执行一次: switchapi-agent install && start（kardianos/service）
  4. 之后壳只做 状态检测 / start / stop / 版本同步
```

design.md §8 “二进制随桌面壳分发或按需下载”中的“随壳分发”成立；“按需下载”对 MVP 非必需（externalBin 已覆盖），可留作 CLI 安装脚本路径。

### (c) 三平台从 GUI 安装服务（置信度见各条）

- **Linux**：kardianos `Option{UserService: true}` → 单元文件写 `~/.config/systemd/user/<name>.service`，用 `systemctl --user` 管理，**无需 root**（service_systemd_linux.go L77-92 源码确认）。**High**
  注意：user 单元只在该用户登录会话期间运行（SSH 登录也算）；如需注销后仍运行，`loginctl enable-linger <user>`（freedesktop 文档）。对开发机场景（CC/Codex 只在登录时使用）可接受。**High**
- **macOS**：`UserService: true` → plist 写 `~/Library/LaunchAgents/<name>.plist` + `launchctl load`，目录用户可写，**无需管理员**（service_darwin.go L110 源码确认）。kardianos 默认 `KeepAlive=true`、`RunAtLoad=false`——**必须显式设 RunAtLoad=true** 才随登录启动（service.go L70-77 源码确认）。**High**
- **Windows**：kardianos **没有**用户级服务支持（service_windows.go 无 UserService 引用，一律 `mgr.Connect()` 走 SCM）；微软官方文档明确 “Only processes with Administrator privileges are able to open handles to the SCM that can be used by the CreateService”→ **注册服务必须一次性提权**。**High**
  从 GUI 提权的可选路径（Tauri 本身无内置提权 API，属功能缺失型结论 **Medium**）：
  1. **运行时 UAC**：以 `runas` 动词启动 `switchapi-agent.exe install`（ShellExecuteW "runas" / PowerShell `Start-Process -Verb RunAs`，微软文档确认 runas 动词存在）——弹一次 UAC，最贴合“壳引导安装”交互。**High**
  2. **安装期完成**：NSIS `installerHooks`（`NSIS_HOOK_POSTINSTALL` 等四个钩子，tauri-utils config.rs 源码确认）+ `installMode: perMachine|both`（此时安装器已提权）在装桌面壳时顺手注册服务；但 Tauri NSIS 默认 `currentUser` 模式不提权，且本地证据显示 cc-switch 特意用 per-user WiX 模板避免管理员——若走 per-user 安装则钩子里也没有权限，仍需路径 1。**High**
  3. **降级方案（无管理员环境）**：Windows 上放弃 SCM，把 Agent 注册为 HKCU Run 自启动进程（autostart 同款机制）+ 壳守护，牺牲崩溃自动重启与登录前启动；kardianos 不支持此模式，需自写分支。**Medium（可行性判断）**
  服务账户细节：kardianos Windows 服务默认跑 **LocalSystem**，其 `%USERPROFILE%` 是 SYSTEM 的 → Agent 的状态目录不能默认解析 `~`，须在 install 时把 `--state-dir`/配置路径显式写进服务参数；“首次安装写 CC/Codex 配置”必须在**用户上下文**（CLI/壳）执行，不能放进服务进程。**Medium-High**

### (d) 托盘 / 自启动 / 更新器（置信度 High，均源码确认）

- **托盘**：Tauri 2 核心内置（cargo feature `tray-icon`），`tauri::tray::TrayIconBuilder` + `Menu` + `on_menu_event`；也可 `app.trayIcon` 配置声明。平台差异：Linux 上 `show_menu_on_left_click` 不支持（config.rs 源码注释）；托盘菜单是原生菜单，动态改菜单项（当前供应商名）用 `MenuItem::set_text` 等运行时 API。
- **自启动**：`tauri-plugin-autostart`（底层 auto-launch 0.5 crate）：macOS 默认 LaunchAgent plist（也支持 AppleScript / SMAppService macOS13+）；Windows 写 `HKCU\...\CurrentVersion\Run`（含 StartupApproved 状态处理，Dynamic 模式 HKLM 失败自动落 HKCU）；Linux 写 `~/.config/autostart/*.desktop`（或 systemd user）。支持带 `--minimized` 之类参数静默启动。
- **更新器**：`tauri-plugin-updater` 2.10.0，需 minisign 签名（`plugins.updater.pubkey` + 私钥签名产物）+ `bundle.createUpdaterArtifacts: true` + endpoints（cc-switch 本地实例用 GitHub Releases `latest.json`，可直接照抄该模式）。平台安装方式（updater.rs 源码确认）：
  - Windows：下载 msi/nsis → 退出应用 → 静默重跑安装器；
  - macOS：解 `.app.tar.gz` 原地替换 .app；
  - Linux：**AppImage 原地替换**；deb/rpm 也已支持（`pkexec` → zenity/kdialog sudo → 终端 sudo 三级回退）。
- **更新器能否连带更新 Agent？** 能且只能更新**安装包内**那份：更新本质是重装整个 bundle，externalBin 的 Agent 随之更新。但**已复制出去注册为服务的那份副本不归 updater 管**。必须由壳在每次启动/更新后做**版本对齐**：比较 `已装服务 agent --version` vs `包内 agent --version`，不一致则 stop → 覆盖副本 → start。Windows 上因服务跑的是副本而非安装目录原件，更新器覆盖安装目录不会撞文件锁；但 stop/start 服务在标准 DACL 下仍需管理员——可在首次 install 时用 `sc sdset` 给当前用户授 start/stop 权，或改为 Agent 自更新（服务进程收到指令后 rename 自身 exe → 落新文件 → 退出，SCM Recovery/KeepAlive 拉起新版；Windows 允许 rename 运行中的 exe）。**版本对齐机制必须进设计，具体提权/自更新路径留 M3 定**。**High（机制）/ Medium（Windows 权限细节最佳解）**

### (e) WebView 装载远程 Hub URL vs 内嵌 SPA（design.md 选了前者——可行，需按下列结论修补）

1. **远程 URL 支持**：`app.windows[].url` 接受 http/https 外部 URL（`WebviewUrl::External`，源码确认）。**High**
2. **远程页面调 Tauri IPC**：默认远程源**无权**调用任何命令；需 capability 带 `remote.urls`（URLPattern，如 `http://192.168.1.10:8080`）。Hub URL 在构建期未知 → 用**运行时 capability**：`Manager::add_capability` + `CapabilityBuilder::new("hub").remote(url).window("main").permission(...)`（tauri 2.10.3 lib.rs L819 + ipc/capability_builder.rs 源码确认）——首启向导拿到 Hub 地址后动态注入。官方警告：**Linux/Android 无法区分 iframe 与窗口本身**（远程 capability 会泄给页内 iframe）；我们的 SPA 不嵌第三方 iframe 即可控。**High**
3. **CSP 两个坑**：
   - `tauri.conf.json` 的 `security.csp` **只注入本地 HTML**（源码 doc 注释确认），对远程 Hub 页面无效；远程页面的 CSP 由 **Hub 的响应头**决定。若 Hub 下发严格 CSP，必须放行 IPC 通道：`connect-src` 加 `ipc: http://ipc.localhost`（cc-switch 本地配置同款写法交叉印证）。反过来：Hub 不发 CSP 头则无此问题。**High**
   - WebKit（macOS/Linux）**不会在 http 源上写入带 `Secure` 标志的 cookie**（tauri#2604，仍 open）。本项目 ADR-0005 内网明文 http → **Hub 的会话 cookie 绝不能设 Secure**，否则桌面壳（及浏览器）登录态直接失效。**High**
4. **会话 cookie 跨重启持久化**：
   - 存储位置默认值（源码确认）：Windows/Linux 上 Tauri **强制**把 webview 数据目录设为 `{LocalData}/{identifier}`（manager/webview.rs L505-521）；Linux 下 wry 据此调用 `cookie_manager.set_persistent_storage(<dir>/cookies, Text)`（wry webkitgtk/web_context.rs），Windows 即 WebView2 user data folder；macOS 用 `WKWebsiteDataStore::defaultDataStore`（持久）。→ **持久 cookie 三平台默认都能跨重启保留**。**High**
   - 但**会话级 cookie（无 Max-Age/Expires）按浏览器语义在进程退出即失效**，与存储目录无关 → **Hub 必须给会话 cookie 设 Max-Age（含续期），否则桌面用户每次开壳都要重登**。这是对 design.md §3 “Session Cookie 鉴权”的直接约束。**High（语义）**
   - macOS WKWebView 有 cookie 懒刷盘行为（退出前最后时刻写入的 cookie 可能丢失）——**Medium**，M3 实测。
5. **托盘快切的鉴权**：托盘菜单动作发生在 Rust 侧，需要以登录态调 Hub REST。Tauri 2.10 提供 `Webview::cookies_for_url(hub_url)`（源码 L2107 确认）可取出 webview 里的 Hub 会话 cookie 供 Rust 侧 HTTP 客户端复用；备选：壳持有独立凭据。**High（API 存在）**
6. **Hub 不可达应急页**：wry 的 `PageLoadEvent` 只有 `Started/Finished`，**没有加载失败事件**（源码确认）→ 不能靠 webview 事件判断 Hub 挂了。推荐模式：主窗口先装载**本地引导页**（`WebviewUrl::App`，打包进壳的极小页面），Rust 侧对 Hub `/healthz` 做带超时探测，成功则 `webview.navigate(hub_url)`（顶级导航无 CORS 问题；若在 JS 里 fetch 探测则有 CORS，应避免或 Hub 放开），失败则留在本地应急页（直连 `127.0.0.1:9527` Agent 读缓存状态/临时降级——注意应急页 fetch 本机 Agent 同样是跨源，Agent 本地状态接口需允许来自 `tauri://localhost` / `http://tauri.localhost` 的 CORS，或应急数据走 Tauri command 由 Rust 代取【更简】）。**High（事件缺失）/ 设计建议部分为推荐而非事实**
7. **内嵌 SPA 对比**（为什么远程 URL 仍是对的）：内嵌（`frontendDist`）可离线、无远程 capability 麻烦，但**壳版本与 Hub API 版本会漂移**（升级 Hub 不升级壳则 UI 落后），且双端“同一套 SPA 实时一致”需要双份部署。远程 URL 模式壳永远显示 Hub 当前版本，只有应急页是本地的——与 design.md 一致。**结论：维持远程 URL + 本地引导/应急页**。

### 附带确认

- 建议引入 `tauri-plugin-single-instance`（本机 registry 已有 2.4.0）：托盘常驻应用防止二开实例、并把二次启动聚焦到已有窗口。**High（插件存在与用途）**
- SPA 侧用 `isTauri()`（@tauri-apps/api core.ts 源码确认）在同一套 React 代码里区分桌面/浏览器环境。**High**

---

## 证据与来源

本地源码（最强证据，均已逐行核对）：

| 结论 | 位置 |
|---|---|
| externalBin 命名/含义 | `~/.cargo/.../tauri-utils-2.8.3/src/config.rs` L1386-1397 |
| 打包落点 macOS `Contents/MacOS` | tauri GitHub `crates/tauri-bundler/src/bundle/macos/app.rs`（`bin_dir = bundle_directory.join("MacOS")` + `copy_binaries`） |
| 打包落点 deb/AppImage `usr/bin` | 同仓库 `linux/debian.rs` L118/L130；`linux/appimage/linuxdeploy.rs` L80 复用 `debian::generate_data` |
| NSIS/MSI 与主 exe 同目录、剥 triple 后缀 | 同仓库 `windows/nsis/mod.rs` L855-880；`settings.rs::copy_binaries` L1168-1184 |
| sidecar 运行时=exe 同目录 | plugins-workspace `plugins/shell/src/process/mod.rs::relative_command_path` L120-142 |
| 壳退出杀 sidecar 子进程 | plugins-workspace `plugins/shell/src/lib.rs` L132-142（RunEvent::Exit → child.kill） |
| kardianos 三平台路径/选项 | kardianos/service `service_systemd_linux.go` L77-92、`service_darwin.go` L110、`service.go` L70-77/L187、`service_windows.go`（无 UserService，`mgr.Connect`） |
| SCM 建服务需管理员 | [Microsoft Learn, Service Security and Access Rights, https://learn.microsoft.com/en-us/windows/win32/services/service-security-and-access-rights]（“Only processes with Administrator privileges … CreateService”） |
| runas 提权动词 | [Microsoft Learn, ShellExecuteA, https://learn.microsoft.com/en-us/windows/win32/api/shellapi/nf-shellapi-shellexecutea] |
| systemd user 单元/linger | [freedesktop.org, loginctl(1), https://www.freedesktop.org/software/systemd/man/latest/loginctl.html] |
| NSIS installerHooks 四钩子/installMode | `tauri-utils-2.8.3/src/config.rs` L890-935 + `NSISInstallerMode` 枚举 |
| TrayIconConfig/Linux 左键限制/tray-icon feature | `tauri-utils-2.8.3/src/config.rs` L2808-2838；`tauri-2.10.3/Cargo.toml` L126 |
| updater 平台安装逻辑（msi/nsis/.app.tar.gz/AppImage/deb/rpm+pkexec） | `~/.cargo/.../tauri-plugin-updater-2.10.0/src/updater.rs` L733-1210 |
| WebviewUrl::External(http/https) | `tauri-utils-2.8.3/src/config.rs` L77-110 |
| capability remote(URLPattern)/运行时 add_capability | `tauri-utils-2.8.3/src/acl/capability.rs` L128-249；`tauri-2.10.3/src/lib.rs` L819-826；`src/ipc/capability_builder.rs` L31-101 |
| csp 只注入本地 HTML | `tauri-utils-2.8.3/src/config.rs` L2583-2588 doc 注释 |
| Win/Linux 强制默认 data_directory={LocalData}/{identifier} | `tauri-2.10.3/src/manager/webview.rs` L505-521 |
| Linux cookie 持久化依赖 data_directory | `wry-0.54.3/src/webkitgtk/web_context.rs` L32-49 |
| macOS defaultDataStore（持久）/data_store_identifier(macOS14+) | `wry-0.54.3/src/wkwebview/mod.rs` L215-247 |
| PageLoadEvent 无失败变体 | `wry-0.54.3/src/lib.rs` L2520-2527 |
| Webview::cookies_for_url | `tauri-2.10.3/src/webview/mod.rs` L2107 |
| navigate API | `tauri-2.10.3/src/webview/mod.rs` L1671 |

官方文档 / 仓库（第二来源，交叉验证）：

- [Tauri, 2026, Embedding External Binaries, https://tauri.app/develop/sidecar/]（tauri-docs v2 分支 mdx 原文核对：命名、capabilities 示例 `"sidecar": true`）
- [Tauri, 2026, Capabilities — Remote API Access, https://tauri.app/security/capabilities/]（“On Linux and Android, Tauri is unable to distinguish between requests from an embedded `<iframe>` and the window itself.” 原文摘录）
- [tauri-apps/plugins-workspace, 2026, plugins/{shell,autostart,updater,single-instance}, https://github.com/tauri-apps/plugins-workspace]
- [zzzgydi/auto-launch, 2026, README, https://github.com/zzzgydi/auto-launch]（Windows Run 注册表三键、macOS 三模式、Linux XDG/systemd）
- [kardianos/service, 2026, README + 源码, https://github.com/kardianos/service]
- [tauri-apps/tauri, issue #2604, https://github.com/tauri-apps/tauri/issues/2604]（WebKit 在 http/localhost 不写 Secure cookie，open）
- 本地生产实例：`/home/orion/Workspace/github/cc-switch-web/src-tauri/tauri.conf.json`（updater endpoints=GitHub latest.json、pubkey、`createUpdaterArtifacts: true`、per-user WiX 模板、CSP `connect-src 'self' ipc: http://ipc.localhost ...` 实证）

---

## 对 design.md 的影响

| design.md 假设（[研究#7] 相关） | 判定 | 说明 |
|---|---|---|
| §8 “Agent 托管（检测/安装/启停本机 Agent，二进制随桌面壳分发或按需下载）” | **confirmed-with-changes** | 随壳分发成立，载体用 `externalBin`；但必须写明：**不用 shell 插件 spawn 常驻**（退出即被杀），而是首启复制到稳定路径后 `agent install` 注册系统服务；“按需下载”降为 CLI 场景可选项 |
| §4 “kardianos/service 注册（systemd/launchd/Windows SCM）” | **confirmed-with-changes** | Linux/macOS 用 **UserService=true**（免 root/免管理员；macOS 需显式 RunAtLoad=true；Linux 注销即停，需要时 enable-linger）；Windows 无用户级服务，**必须一次 UAC**（runas 动词或安装器钩子），且服务默认 LocalSystem → 状态目录须显式传参、写 CC/Codex 配置必须在用户上下文执行 |
| §8 “WebView 装载 Hub URL（首启配置向导）” | **confirmed-with-changes** | 可行；需补三点：① 运行时 `add_capability(remote=Hub 源)` 授权 IPC；② **Hub 会话 cookie 必须带 Max-Age 且不设 Secure**（否则桌面登录态每次丢/http 下直接写不进）；③ Hub 若下发 CSP 头需放行 `connect-src ipc: http://ipc.localhost` |
| §8 “托盘菜单（当前供应商 + 快切 + Agent 状态）” | **confirmed** | 核心 TrayIconBuilder 全覆盖；Linux 无左键弹菜单；Rust 侧调 Hub 可用 `cookies_for_url` 复用登录态 |
| §8 “开机自启” | **confirmed** | autostart 插件即可（HKCU Run / LaunchAgent / XDG autostart），支持 `--minimized` |
| §8 “Hub 不可达时的应急页（直连 127.0.0.1 Agent）” | **confirmed-with-changes** | 无加载失败事件 → 结构改为“本地引导页默认 + Rust 侧健康探测 → navigate(Hub)”；应急页取 Agent 数据建议走 Tauri command（Rust 代取）避免 CORS |
| §9 “桌面安装包：msi/dmg/AppImage 或 deb” | **confirmed-with-changes** | 若要壳自更新：Linux 优先 **AppImage**（deb/rpm 更新走 pkexec/sudo 弹窗，体验差）；Windows 建议 NSIS（cc-switch 实证走 per-user 安装 + GitHub latest.json）；需 minisign 签名与 `createUpdaterArtifacts` |
| （新增条目建议）壳↔Agent 版本对齐 | **needs change（补充设计）** | Tauri updater 只更新安装包内副本；服务运行的副本需壳在启动/更新后做版本比对 + stop/覆盖/start（Windows 的服务操作权限方案 M3 定：sc sdset 授权 vs Agent 自更新） |

无与 design.md **矛盾**（contradicted）的发现；整体架构选择（独立守护进程 + 壳只做引导安装）被证据强化。

---

## 遗留不确定性

1. **macOS WKWebView cookie 懒刷盘**：defaultDataStore 持久化已源码确认，但退出瞬间写入的 cookie 是否可靠落盘未实测（未找到可引用的权威 issue）——M3 在真机验证“登录→立即退出→重开”路径。（Medium）
2. **Windows 服务 start/stop 的非管理员授权**：`sc sdset` 给普通用户授 start/stop 与“Agent 自更新（rename 自身）”两条路都可行但均未实测；MVP 可先接受“版本对齐时弹一次 UAC”。（Medium）
3. **NSIS per-user 安装 + runas 提权注册服务**在带 UAC 策略收紧（如企业机）环境下的表现未验证；家庭/个人机默认策略下无疑虑。（Medium）
4. **运行时 add_capability 的持久性**：每次进程启动都要重新注入（内存态），壳启动流程需固化“读配置→add_capability→navigate”次序；未发现官方对 remote + 动态 capability 组合的反例，但集成测试应覆盖。（Medium）
5. **Linux 托盘生态**：TrayIcon 在 GNOME 需 AppIndicator 扩展（libayatana-appindicator 依赖链）；未在本任务展开各发行版矩阵。（Low 影响，Medium 不确定）
6. AppImage 自更新替换后**文件路径不变**（原地覆盖）已由源码确认，但若用户手动改名/移动 AppImage，壳内“定位包内 agent”逻辑要以 `current_exe` 为锚而非硬编码路径——实现注意项。（High 确定性，列此仅作实现提醒）
