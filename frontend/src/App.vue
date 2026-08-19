<script setup>
import { ref, reactive, computed, onMounted, onUnmounted } from 'vue'

const wails = () => window.go?.main?.API

// ---------- 状态 ----------
const tab = ref('dashboard')
const dash = ref(null)
const accounts = ref([])
const settings = reactive({ port: 8317, defaultProvider: 'auto', checkinEnabled: true, checkinTime: '09:05', startMinimized: false })
const logs = ref([])
const toast = ref('')
let toastTimer = null

// 登录弹窗
const loginModal = reactive({
  visible: false, provider: '', url: '', status: 'pending', err: '', result: null
})
let loginPollTimer = null

// ---------- 工具 ----------
function showToast(msg) {
  toast.value = msg
  clearTimeout(toastTimer)
  toastTimer = setTimeout(() => (toast.value = ''), 2600)
}

async function wrap(fn, okMsg) {
  try {
    const r = await fn()
    if (okMsg) showToast(okMsg)
    return r
  } catch (e) {
    showToast('操作失败: ' + (e?.message || e))
    throw e
  }
}

const uptimeText = computed(() => {
  const ms = dash.value?.uptimeMs || 0
  const s = Math.floor(ms / 1000)
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60)
  if (h > 0) return `${h} 小时 ${m} 分`
  return `${m} 分 ${s % 60} 秒`
})

// ---------- 数据刷新 ----------
async function refreshDash() {
  try { dash.value = await wails().GetDashboard() } catch {}
}
async function refreshAccounts() {
  try { accounts.value = await wails().ListAccounts() } catch {}
}
async function refreshLogs() {
  try { logs.value = (await wails().GetLogs(200)).slice().reverse() } catch {}
}

let pollTimer = null
onMounted(async () => {
  await Promise.all([refreshDash(), refreshAccounts(), refreshLogs()])
  // 加载设置
  try {
    const s = await wails().GetSettings()
    Object.assign(settings, s)
  } catch {}
  pollTimer = setInterval(async () => {
    await Promise.all([refreshDash(), refreshAccounts()])
  }, 5000)
  // 日志事件推送
  window.runtime?.EventsOn('log', (e) => {
    logs.value.unshift(e)
    if (logs.value.length > 300) logs.value.pop()
  })
})
onUnmounted(() => {
  clearInterval(pollTimer)
  clearInterval(loginPollTimer)
})

// ---------- 账号操作 ----------
async function startLogin(provider) {
  try {
    const url = await wails().StartLogin(provider)
    loginModal.visible = true
    loginModal.provider = provider
    loginModal.url = url
    loginModal.status = 'pending'
    loginModal.err = ''
    loginModal.result = null
    clearInterval(loginPollTimer)
    loginPollTimer = setInterval(async () => {
      try {
        const st = await wails().PollLoginStatus()
        if (st.active && st.provider === provider) {
          if (st.status === 'done') {
            loginModal.status = 'done'
            loginModal.result = st.result
            clearInterval(loginPollTimer)
            refreshAccounts()
          } else if (st.status === 'error') {
            loginModal.status = 'error'
            loginModal.err = st.err
            clearInterval(loginPollTimer)
          }
        }
      } catch {}
    }, 2500)
  } catch (e) {
    showToast('发起登录失败: ' + (e?.message || e))
  }
}

function closeLoginModal() {
  if (loginModal.status === 'pending') {
    wails()?.CancelLogin()
  }
  clearInterval(loginPollTimer)
  loginModal.visible = false
  refreshAccounts()
}

async function delAccount(a) {
  if (!confirm(`确定删除账号 ${a.nickname || a.uid}？`)) return
  await wrap(() => wails().DeleteAccount(a.provider, a.uid), '已删除')
  refreshAccounts()
}

async function checkin(a) {
  await wrap(() => wails().CheckinNow(a.provider, a.uid), '签到完成')
  refreshAccounts()
}

async function refreshCredits(a) {
  await wrap(() => wails().RefreshCreditsNow(a.provider, a.uid))
  refreshAccounts()
}

async function reenable(a) {
  await wrap(() => wails().ReenableAccount(a.provider, a.uid), '已恢复')
  refreshAccounts()
}

function openLoginUrl() {
  wails()?.OpenURL(loginModal.url)
}

// ---------- 设置 ----------
async function saveSettings() {
  await wrap(() => wails().SaveSettings({ ...settings }), '设置已保存')
  refreshDash()
}

async function regenKey() {
  if (!confirm('重新生成后，现有调用方需更新 API Key，继续？')) return
  await wrap(async () => {
    const key = await wails().RegenerateAPIKey()
    dash.value.apiKey = key
  }, '已重新生成')
  refreshDash()
}

async function copyKey() {
  const key = dash.value?.apiKey || ''
  try {
    await navigator.clipboard.writeText(key)
    showToast('已复制到剪贴板')
  } catch {
    showToast('复制失败')
  }
}

function copyCurl() {
  const d = dash.value
  if (!d) return
  const text = `curl ${d.baseUrl}/v1/chat/completions -H "Authorization: Bearer ${d.apiKey}" -H "Content-Type: application/json" -d '{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}'`
  navigator.clipboard.writeText(text).then(() => showToast('已复制调用示例'))
}

function openDataDir() { wails()?.OpenDataDir() }
function hideToTray() { wails()?.HideToTray() }
</script>

<template>
  <div class="layout">
    <!-- 侧栏 -->
    <aside class="sidebar">
      <div class="brand">Work<span class="dot">2</span>API</div>
      <div class="nav-item" :class="{ active: tab === 'dashboard' }" @click="tab = 'dashboard'">仪表盘</div>
      <div class="nav-item" :class="{ active: tab === 'accounts' }" @click="tab = 'accounts'">账号管理</div>
      <div class="nav-item" :class="{ active: tab === 'settings' }" @click="tab = 'settings'">设置</div>
      <div class="spacer"></div>
      <div class="nav-item" @click="hideToTray">隐藏到托盘</div>
      <div class="ver">v{{ dash?.version || '…' }}</div>
    </aside>

    <!-- 仪表盘 -->
    <main v-if="tab === 'dashboard'" class="main">
      <h2>仪表盘</h2>
      <div class="grid">
        <div class="stat">
          <div class="label">服务状态</div>
          <div class="value" :style="{ color: dash?.healthyCount > 0 ? 'var(--green)' : 'var(--red)' }">
            {{ dash?.healthyCount > 0 ? '运行中' : '无可用账号' }}
          </div>
        </div>
        <div class="stat">
          <div class="label">可用账号 / 总数</div>
          <div class="value">{{ dash?.healthyCount ?? '–' }} / {{ dash?.accountCount ?? '–' }}</div>
        </div>
        <div class="stat">
          <div class="label">总积分</div>
          <div class="value">{{ dash?.totalCredits ?? '–' }}</div>
        </div>
        <div class="stat">
          <div class="label">运行时长</div>
          <div class="value">{{ uptimeText }}</div>
        </div>
      </div>

      <div class="card" v-if="dash">
        <h2 style="font-size:15px; margin-bottom:12px">DeepSeek Harness 接入</h2>
        <div class="form-item">
          <label>Base URL</label>
          <div class="key-box"><span class="txt">{{ dash.baseUrl }}/v1</span>
            <button class="btn sm ghost" @click="navigator.clipboard.writeText(dash.baseUrl + '/v1').then(() => showToast('已复制'))">复制</button>
          </div>
        </div>
        <div class="form-item">
          <label>API Key</label>
          <div class="key-box">
            <span class="txt">{{ dash.apiKey }}</span>
            <button class="btn sm ghost" @click="copyKey">复制</button>
            <button class="btn sm ghost" @click="regenKey">重新生成</button>
          </div>
        </div>
        <div class="row">
          <button class="btn ghost" @click="copyCurl">复制 curl 示例</button>
          <span style="color:var(--text-dim);font-size:12px">模型可用 trae/xxx 或 wb/xxx 前缀强制指定上游</span>
        </div>
      </div>

      <div class="card">
        <h2 style="font-size:15px; margin-bottom:12px">日志（脱敏）</h2>
        <div class="logs">
          <div v-if="logs.length === 0" class="empty">暂无日志</div>
          <div v-for="(l, i) in logs" :key="i">
            <span class="t">{{ new Date(l.ms).toLocaleTimeString() }}</span>
            <span :class="'lv-' + l.level">{{ l.msg }}</span>
          </div>
        </div>
      </div>
    </main>

    <!-- 账号管理 -->
    <main v-else-if="tab === 'accounts'" class="main">
      <div class="row" style="margin-bottom:16px">
        <h2 class="grow">账号管理</h2>
        <button class="btn" @click="startLogin('trae')">+ 登录 TRAE</button>
        <button class="btn" @click="startLogin('workbuddy')">+ 登录 WorkBuddy</button>
        <button class="btn ghost" @click="wails()?.RefreshAllCredits(); showToast('积分刷新中…')">刷新积分</button>
      </div>
      <div class="card" style="padding:0">
        <table v-if="accounts.length > 0">
          <thead>
            <tr>
              <th>上游</th><th>昵称 / UID</th><th>积分</th><th>状态</th><th>Token 到期</th><th style="width:220px">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="a in accounts" :key="a.provider + a.uid">
              <td><span class="badge" :class="a.provider">{{ a.provider === 'trae' ? 'TRAE' : 'WorkBuddy' }}</span></td>
              <td>
                <div>{{ a.nickname || '–' }}</div>
                <div class="mono" style="color:var(--text-dim);font-size:11px">{{ a.uid }}</div>
              </td>
              <td>{{ a.credits }}</td>
              <td>
                <span v-if="a.disabled" class="badge dead">已禁用</span>
                <span v-else-if="a.cooling" class="badge cool">冷却至 {{ a.until }}</span>
                <span v-else class="badge ok">可用</span>
                <div v-if="a.reason" style="color:var(--text-dim);font-size:11px">{{ a.reason }}</div>
              </td>
              <td style="font-size:12px">{{ a.expiresTime }}</td>
              <td>
                <div class="row" style="gap:6px">
                  <button class="btn sm ghost" @click="checkin(a)">签到</button>
                  <button class="btn sm ghost" @click="refreshCredits(a)">积分</button>
                  <button v-if="a.disabled" class="btn sm ghost" @click="reenable(a)">恢复</button>
                  <button class="btn sm danger" @click="delAccount(a)">删除</button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        <div v-else class="empty">
          暂无账号，点击右上角按钮登录添加<br /><br />
          <span style="font-size:12px">TRAE 登录会自动捕获浏览器回调；WorkBuddy 登录需在浏览器完成授权</span>
        </div>
      </div>
    </main>

    <!-- 设置 -->
    <main v-else class="main">
      <h2>设置</h2>
      <div class="card" style="max-width:520px">
        <div class="form-item">
          <label>API 监听端口（仅 127.0.0.1）</label>
          <input type="number" v-model.number="settings.port" min="1024" max="65535" />
        </div>
        <div class="form-item">
          <label>默认上游（无前缀模型走哪个）</label>
          <select v-model="settings.defaultProvider">
            <option value="auto">auto（积分最多者优先）</option>
            <option value="trae">TRAE SOLO</option>
            <option value="workbuddy">WorkBuddy</option>
          </select>
        </div>
        <div class="form-item">
          <label>每日自动签到</label>
          <label class="switch">
            <input type="checkbox" v-model="settings.checkinEnabled" />
            <span class="slider"></span>
          </label>
        </div>
        <div class="form-item" v-if="settings.checkinEnabled">
          <label>签到时间</label>
          <input type="time" v-model="settings.checkinTime" />
        </div>
        <div class="form-item">
          <label>启动时最小化到托盘</label>
          <label class="switch">
            <input type="checkbox" v-model="settings.startMinimized" />
            <span class="slider"></span>
          </label>
        </div>
        <div class="row">
          <button class="btn" @click="saveSettings">保存设置</button>
          <button class="btn ghost" @click="openDataDir">打开数据目录</button>
        </div>
      </div>
    </main>

    <!-- 登录弹窗 -->
    <div v-if="loginModal.visible" class="modal-mask" @click.self="closeLoginModal">
      <div class="modal">
        <h3>登录 {{ loginModal.provider === 'trae' ? 'TRAE SOLO' : 'WorkBuddy' }}</h3>
        <div class="desc" v-if="loginModal.status === 'pending'">
          点击下方按钮在浏览器完成授权。<br />
          <template v-if="loginModal.provider === 'trae'">登录成功后浏览器会自动跳转回本应用，无需手动操作。</template>
          <template v-else>完成登录后此窗口会自动检测到。</template>
        </div>
        <div class="url-box">{{ loginModal.url }}</div>
        <div class="status-line" v-if="loginModal.status === 'pending'">
          <span class="spinner"></span> 等待浏览器授权…（5 分钟超时）
        </div>
        <div class="status-line" v-else-if="loginModal.status === 'done'" style="color:var(--green)">
          登录成功：{{ loginModal.result?.nickname || loginModal.result?.uid }}
        </div>
        <div class="status-line" v-else style="color:var(--red)">
          登录失败：{{ loginModal.err }}
        </div>
        <div class="row">
          <button v-if="loginModal.status === 'pending'" class="btn" @click="openLoginUrl">在浏览器打开</button>
          <button class="btn ghost" @click="closeLoginModal">{{ loginModal.status === 'done' ? '完成' : '关闭' }}</button>
        </div>
      </div>
    </div>

    <div v-if="toast" class="toast">{{ toast }}</div>
  </div>
</template>
