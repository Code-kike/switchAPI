// 引导页逻辑：get_config → 有 hub_url 则 connect（Rust 探测 healthz 后 navigate），
// 失败落应急视图（Agent 状态经 Tauri command 由 Rust 代取，避免 CORS）。
const { invoke } = window.__TAURI__.core

const views = ['wizard', 'connecting', 'emergency']
function show(name) {
  for (const v of views) {
    document.getElementById(`view-${v}`).hidden = v !== name
  }
}

let hubUrl = ''

async function refreshAgentStatus() {
  const box = document.getElementById('agent-status')
  try {
    box.textContent = await invoke('agent_status')
  } catch (e) {
    box.textContent = `无法读取 Agent 状态：${e}`
  }
}

async function tryConnect(url) {
  show('connecting')
  document.getElementById('connecting-target').textContent = url
  try {
    await invoke('connect', { hubUrl: url })
    // 成功时 Rust 已把 webview 导航到 Hub，本页生命周期到此结束。
  } catch (e) {
    hubUrl = url
    document.getElementById('emergency-hub').textContent = url
    show('emergency')
    refreshAgentStatus()
  }
}

async function agentCtl(action) {
  const err = document.getElementById('agent-error')
  err.hidden = true
  try {
    await invoke('agent_ctl', { action })
  } catch (e) {
    err.textContent = String(e)
    err.hidden = false
  }
  refreshAgentStatus()
}

document.getElementById('wizard-form').addEventListener('submit', (ev) => {
  ev.preventDefault()
  const url = document.getElementById('hub-url').value.trim().replace(/\/+$/, '')
  if (url) tryConnect(url)
})
document.getElementById('btn-retry').addEventListener('click', () => tryConnect(hubUrl))
document.getElementById('btn-change').addEventListener('click', () => show('wizard'))
document.getElementById('btn-agent-install').addEventListener('click', () => agentCtl('install'))
document.getElementById('btn-agent-start').addEventListener('click', () => agentCtl('start'))
document.getElementById('btn-agent-stop').addEventListener('click', () => agentCtl('stop'))

// 启动：读配置决定进向导还是直连。
invoke('get_config')
  .then((cfg) => {
    if (cfg && cfg.hub_url) {
      hubUrl = cfg.hub_url
      document.getElementById('hub-url').value = cfg.hub_url
      tryConnect(cfg.hub_url)
    } else {
      show('wizard')
    }
  })
  .catch(() => show('wizard'))
