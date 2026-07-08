外部二进制占位目录：`tauri build` 打包前，把 Go 构建产物按 target-triple 命名放入，例如
`switchapi-agent-x86_64-unknown-linux-gnu`（见 tauri.conf.json bundle.externalBin 与 research/07 (a)）。
纯 `cargo build` 不需要本目录有内容。
