<script setup>
import { ref, reactive, computed, onMounted, onUnmounted } from 'vue'

const wails = () => window.go?.main?.API

// ---------- 状态 ----------
const tab = ref('dashboard')
const dash = ref(null)
const accounts = ref([])
const settings = reactive({ port: 8317, defaultProvider: 'auto', checkinEnabled: true, checkinTime: '09:05', startMinimized: false, autoStart: false, traeEnabled: true, wbEnabled: true, creditFloor: 0, traeCheckinMethod: 'browser', traeWebAccountCount: 3 })
const logs = ref([])
const toast = ref('')
let toastTimer = null

// 模型列表
const models = reactive({ trae: [], workbuddy: [] })
const modelsLoaded = ref(false)
const modelsLoading = ref(false)

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
  await Promise.all([refreshDash(), refreshAccounts(), refreshLogs(), refreshTraeWeb()])
  // 加载设置
  try {
    const s = await wails().GetSettings()
    Object.assign(settings, s)
  } catch {}
  pollTimer = setInterval(async () => {
    await Promise.all([refreshDash(), refreshAccounts(), refreshTraeWeb()])
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

// ---------- 模型 ----------
const modelProviders = [
  { key: 'trae', label: 'TRAE SOLO', enabledKey: 'traeEnabled', prefix: 'trae/' },
  { key: 'workbuddy', label: 'WorkBuddy', enabledKey: 'wbEnabled', prefix: 'wb/' }
]

// 上游启用开关切换：立即保存（关闭后不再消耗该上游积分）
// v-model 已把新值写入 settings，这里直接读当前值；保存失败回滚 UI 状态
async function toggleProvider(p) {
  const prev = !settings[p.enabledKey]
  const next = settings[p.enabledKey]
  try {
    await wails().SaveSettings({ ...settings })
    showToast(next ? `${p.label} 已启用` : `${p.label} 已关闭（不再消耗其积分）`)
    refreshDash()
  } catch (e) {
    settings[p.enabledKey] = prev
    showToast('操作失败: ' + (e?.message || e))
  }
}

async function loadModels(force = false) {
  if (modelsLoading.value) return
  modelsLoading.value = true
  try {
    const m = force ? await wails().RefreshModels() : await wails().ListModels()
    models.trae = m.trae || []
    models.workbuddy = m.workbuddy || []
    modelsLoaded.value = true
  } catch (e) {
    showToast('模型列表加载失败: ' + (e?.message || e))
  } finally {
    modelsLoading.value = false
  }
}

function switchTab(t) {
  tab.value = t
  if (t === 'models' && !modelsLoaded.value) loadModels()
}

function fmtTokens(n) {
  if (!n) return '–'
  return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n)
}

function copyModelId(p, m) {
  const id = p.prefix + m.id
  navigator.clipboard.writeText(id).then(() => showToast(`已复制 ${id}`))
}

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
  // 后端返回真实结果：签到成功 / 今日已签到 / 签到未生效（enable=false）
  const msg = await wrap(() => wails().CheckinNow(a.provider, a.uid))
  showToast(`${a.provider === 'trae' ? 'TRAE' : 'WorkBuddy'}：${msg || '签到完成'}`)
  refreshAccounts()
}

let checkinAllRunning = false
async function checkinAll() {
  if (checkinAllRunning) return
  checkinAllRunning = true
  showToast('正在为所有账号签到…')
  try {
    const msg = await wails().CheckinAllNow()
    showToast(msg)
    refreshAccounts()
  } catch (e) {
    showToast('一键全签失败: ' + (e?.message || e))
  } finally {
    checkinAllRunning = false
  }
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

// ---------- Trae 网页签到（浏览器引擎，独立于 OAuth 账号） ----------
const traeWeb = ref([])
async function refreshTraeWeb() {
  try { traeWeb.value = await wails().TraeWebStatus() } catch {}
}
const todayStr = () => {
  const t = new Date()
  return `${t.getFullYear()}-${String(t.getMonth() + 1).padStart(2, '0')}-${String(t.getDate()).padStart(2, '0')}`
}
// 网页登录：启动有头 Edge 让用户输入手机+验证码（最长 5 分钟）
async function startTraeWebLogin(i) {
  showToast(`请在弹出的 Edge 窗口用手机+验证码登录账号 ${i}（最多 5 分钟）`)
  try {
    await wails().StartTraeWebLogin(i)
    showToast(`账号 ${i} 登录成功，会话已保存`)
  } catch (e) {
    showToast('网页登录未成功: ' + (e?.message || e))
  }
  refreshTraeWeb()
}
// 手动签到单个网页账号
async function checkinTraeWeb(i) {
  try {
    const msg = await wails().CheckinTraeWebNow(i)
    showToast(`账号 ${i}: ${msg || '完成'}`)
  } catch (e) {
    showToast('网页签到失败: ' + (e?.message || e))
  }
  refreshTraeWeb()
}

// ---------- 设置 ----------
async function saveSettings() {
  // 积分阈值：空输入/非法值归零（0 = 不限制）
  settings.creditFloor = Math.max(0, Math.floor(Number(settings.creditFloor) || 0))
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
  const text = `curl ${d.baseUrl}/v1/chat/completions -H "Authorization: Bearer ${d.apiKey}" -H "Content-Type: application/json" -d '{"messages":[{"role":"user","content":"hi"}]}'`
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
      <div class="nav-item" :class="{ active: tab === 'dashboard' }" @click="switchTab('dashboard')">仪表盘</div>
      <div class="nav-item" :class="{ active: tab === 'accounts' }" @click="switchTab('accounts')">账号管理</div>
      <div class="nav-item" :class="{ active: tab === 'models' }" @click="switchTab('models')">模型</div>
      <div class="nav-item" :class="{ active: tab === 'traeweb' }" @click="switchTab('traeweb')">网页签到</div>
      <div class="nav-item" :class="{ active: tab === 'settings' }" @click="switchTab('settings')">设置</div>
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
          <span style="color:var(--text-dim);font-size:12px">不传 model 时走各上游默认模型（DeepSeek v4 flash）；trae/xxx、wb/xxx 前缀强制指定上游</span>
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
        <button class="btn ghost" :disabled="checkinAllRunning" @click="checkinAll">一键全签</button>
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
                <span v-else-if="settings.creditFloor > 0 && a.credits <= settings.creditFloor" class="badge cool">低于阈值（保留 {{ settings.creditFloor }}）</span>
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

    <!-- 模型 -->
    <main v-else-if="tab === 'models'" class="main">
      <div class="row" style="margin-bottom:16px">
        <h2 class="grow">模型</h2>
        <button class="btn ghost" :disabled="modelsLoading" @click="loadModels(true)">
          {{ modelsLoading ? '加载中…' : '刷新列表' }}
        </button>
      </div>

      <div class="card" v-for="p in modelProviders" :key="p.key" style="margin-bottom:16px">
        <div class="row" style="margin-bottom:12px">
          <h2 style="font-size:15px">{{ p.label }}</h2>
          <label class="switch" style="margin-left:8px" :title="settings[p.enabledKey] ? '点击关闭：不再消耗该上游积分' : '点击开启：正常使用该上游'">
            <input type="checkbox" v-model="settings[p.enabledKey]" @change="toggleProvider(p)" />
            <span class="slider"></span>
          </label>
          <span style="color:var(--text-dim);font-size:12px" v-if="!settings[p.enabledKey]">已关闭（不消耗积分）</span>
          <span class="badge" :class="p.key" style="margin-left:4px">{{ models[p.key].length }} 个模型</span>
        </div>
        <div style="max-height:420px;overflow:auto">
          <table v-if="models[p.key].length > 0">
            <thead>
              <tr><th>模型 ID</th><th>名称</th><th>上下文</th><th>最大输出</th><th style="width:90px">操作</th></tr>
            </thead>
            <tbody>
              <tr v-for="m in models[p.key]" :key="m.id">
                <td class="mono">{{ m.id }}</td>
                <td>{{ m.name || '–' }}</td>
                <td>{{ fmtTokens(m.contextLength) }}</td>
                <td>{{ fmtTokens(m.maxTokens) }}</td>
                <td><button class="btn sm ghost" @click="copyModelId(p, m)">复制</button></td>
              </tr>
            </tbody>
          </table>
          <div v-else class="empty" style="padding:12px">
            {{ modelsLoading ? '加载中…' : '暂无模型（登录账号后可动态获取，当前显示为静态回退表）' }}
          </div>
        </div>
      </div>

      <p style="color:var(--text-dim);font-size:12px">
        复制得到的名称可直接填入 Agent 的模型栏（带 trae/ 或 wb/ 前缀，强制路由到对应上游）；客户端未指定模型时自动回退 DeepSeek v4 flash 正式版。
      </p>
    </main>

    <!-- 网页签到 -->
    <main v-else-if="tab === 'traeweb'" class="main">
      <div class="row" style="margin-bottom:16px">
        <h2 class="grow">Trae 网页签到</h2>
        <button class="btn ghost" @click="refreshTraeWeb">刷新状态</button>
      </div>
      <div class="card" style="margin-bottom:16px; background:var(--bg-soft)">
        本功能通过系统 <b>Edge 浏览器</b> 驱动网页端签到，绕过桌面客户端 TTNet 签名限制（纯 HTTP 会被 9074 拒绝）。
        每个账号只需<b>首次</b>用「手机 + 验证码」登录一次，会话独立保存在应用数据目录，之后每日自动签到无需再输入。
      </div>
      <div class="card" style="padding:0">
        <table v-if="traeWeb.length > 0">
          <thead>
            <tr><th>账号</th><th>登录状态</th><th>最近签到</th><th style="width:240px">操作</th></tr>
          </thead>
          <tbody>
            <tr v-for="w in traeWeb" :key="w.index">
              <td>账号 {{ w.index }}</td>
              <td>
                <span v-if="w.loggedIn" class="badge ok">已登录</span>
                <span v-else class="badge dead">未登录（需首次登录）</span>
              </td>
              <td>
                <span v-if="w.lastCheckin === todayStr()" class="badge ok">今日已签</span>
                <span v-else-if="w.lastCheckin" style="font-size:12px">{{ w.lastCheckin }}</span>
                <span v-else style="color:var(--text-dim);font-size:12px">—</span>
              </td>
              <td>
                <div class="row" style="gap:6px">
                  <button class="btn sm ghost" @click="startTraeWebLogin(w.index)">{{ w.loggedIn ? '重新登录' : '网页登录' }}</button>
                  <button class="btn sm ghost" :disabled="!w.loggedIn" @click="checkinTraeWeb(w.index)">签到</button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        <div v-else class="empty">暂无网页账号（默认 3 个，可在设置中调整）</div>
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
          <label>积分保留阈值（账号余额 ≤ 该值时暂停使用，积分恢复后自动启用；0 = 不限制）</label>
          <input type="number" v-model.number="settings.creditFloor" min="0" step="10" placeholder="0" />
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
          <label>Trae 签到方式</label>
          <select v-model="settings.traeCheckinMethod">
            <option value="browser">浏览器（推荐，驱动系统 Edge 绕开 TTNet）</option>
            <option value="http">纯 HTTP（旧版，会返回 9074 假失败）</option>
          </select>
        </div>
        <div class="form-item" v-if="settings.traeCheckinMethod === 'browser'">
          <label>网页签到账号数（隔离 profile 个数，对应你有几个 Trae 账号）</label>
          <input type="number" v-model.number="settings.traeWebAccountCount" min="1" max="10" />
        </div>
        <div class="form-item">
          <label>启动时最小化到托盘</label>
          <label class="switch">
            <input type="checkbox" v-model="settings.startMinimized" />
            <span class="slider"></span>
          </label>
        </div>
        <div class="form-item">
          <label>开机自动启动（写入当前用户注册表）</label>
          <label class="switch">
            <input type="checkbox" v-model="settings.autoStart" />
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
