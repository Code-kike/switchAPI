# 安全姿态：内网优先，不内置 TLS

MVP 假定 Hub 运行于内网 / Tailscale 之内，以 HTTP 明文监听；公网暴露交给用户自备的反向代理（Caddy 等）或 VPN，不内置 ACME/HTTPS/防爆破——不重造 TLS 轮子。系统内部安全边界仍然完整：Web 控制台管理员密码 + Session；设备以一次性配对码换取可吊销的长期 token；Agent 仅监听 127.0.0.1 且校验本地 token；全部上游 API Key 以主密钥 AES-GCM 加密落库（主密钥首次启动生成，存于 Hub 配置文件）；配置导出含密钥时强制口令加密。公网直挂全套（内置 ACME、TOTP、登录限速）列为二期。
