/* 外卖商户申诉与证据归档 —— Vue 3 单文件应用（无构建步骤，使用全局构建版 Vue） */
const { createApp, reactive, ref, computed, onMounted, onUnmounted, watch } = Vue;

const api = {
  token: localStorage.getItem('token') || '',
  storeId() { return this.user?.store_id; },
  user: JSON.parse(localStorage.getItem('user') || 'null'),
  async req(method, path, body, isForm) {
    const opt = { method, headers: {} };
    if (body !== undefined) {
      if (isForm) { opt.body = body; }
      else { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
    }
    if (this.token) opt.headers['Authorization'] = 'Bearer ' + this.token;
    const res = await fetch(path, opt);
    let data = null;
    try { data = await res.json(); } catch (e) { /* 非 json */ }
    if (!res.ok) {
      const msg = (data && data.message) || ('请求失败（' + res.status + '）');
      const err = new Error(msg);
      err.status = res.status; err.code = data && data.error;
      if (res.status === 401) { this.token = ''; localStorage.removeItem('token'); location.hash = '#/login'; }
      throw err;
    }
    return data;
  },
  setSession(token, user) {
    this.token = token; this.user = user;
    localStorage.setItem('token', token);
    localStorage.setItem('user', JSON.stringify(user));
    // 附件下载用的 <img>/<a> 无法带 Authorization 头，同步种一个 HttpOnly=false 的会话 cookie
    document.cookie = 'session=' + token + ';path=/;SameSite=Lax';
  },
  clear() {
    this.token = ''; this.user = null;
    localStorage.removeItem('token'); localStorage.removeItem('user');
    document.cookie = 'session=;path=/;expires=Thu, 01 Jan 1970 00:00:00 GMT';
  },
};

const STATUS_TABS = [
  { key: '', label: '全部' },
  { key: 'pending', label: '待申诉' },
  { key: 'submitted', label: '审核中' },
  { key: 'withdrawn', label: '已撤回' },
  { key: 'expired', label: '已超期' },
  { key: 'closed', label: '已结案' },
];

const SLOT_OPTIONS = [
  { value: 'meal_ready', label: '出餐凭证' },
  { value: 'packed', label: '打包凭证' },
  { value: 'rider_pickup', label: '骑手取餐凭证' },
  { value: 'other', label: '其他材料' },
];

function fmtDT(t) {
  if (!t) return '';
  const d = new Date(t);
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}
function fmtMoney(c) { return '¥' + ((c || 0) / 100).toFixed(2); }
function fmtCountdown(sec) {
  if (sec <= 0) return '已超期';
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600),
    m = Math.floor(sec % 3600 / 60), s = sec % 60;
  if (d > 0) return `剩 ${d} 天 ${h} 小时`;
  if (h > 0) return `剩 ${h} 小时 ${m} 分`;
  if (m > 0) return `剩 ${m} 分 ${s} 秒`;
  return `剩 ${s} 秒`;
}

createApp({
  setup() {
    const state = reactive({
      view: api.token ? 'main' : 'login',
      loginForm: { username: '', password: '' },
      loginErr: '',
      logging: false,
      list: [],
      loadingList: false,
      tab: '',
      detail: null,
      loadingDetail: false,
      selectedId: null,
      stats: { counts: {}, expiring_soon: 0, total: 0 },
      notifications: [], unread: 0, showNotif: false,
      // 上传表单
      up: { slot: 'meal_ready', note: '', file: null, fileName: '' },
      uploading: false,
      // 弹窗
      confirm: null, // {title, body, note, busy, action}
      lightbox: null,
      // 模拟平台
      mock: { receipt_key: '', kind: 'result', decision: 'revoked', adjusted_cents: 500, message: '', busy: false },
    });
    const toasts = reactive([]);
    let toastSeq = 0;
    function toast(message, type = 'info', ms = 3800) {
      const id = ++toastSeq;
      toasts.push({ id, message, type });
      setTimeout(() => { const i = toasts.findIndex(t => t.id === id); if (i >= 0) toasts.splice(i, 1); }, ms);
    }
    const failMsg = e => toast(e.message || '操作失败', 'err', 5200);

    /* ---------- 登录 ---------- */
    async function doLogin() {
      if (!state.loginForm.username || !state.loginForm.password) {
        state.loginErr = '请输入用户名和密码'; return;
      }
      state.logging = true; state.loginErr = '';
      try {
        const r = await api.req('POST', '/api/auth/login', state.loginForm);
        api.setSession(r.token, r.user);
        state.view = 'main';
        await bootstrap();
      } catch (e) { state.loginErr = e.message; }
      finally { state.logging = false; }
    }
    function quickFill(name) { state.loginForm.username = name; state.loginForm.password = '123456'; }
    async function logout() {
      try { await api.req('POST', '/api/auth/logout'); } catch (e) {}
      api.clear();
      state.view = 'login'; state.list = []; state.detail = null;
    }

    /* ---------- 数据加载 ---------- */
    const base = () => `/api/stores/${api.user.store_id}`;

    async function loadList() {
      state.loadingList = true;
      try {
        const q = state.tab ? `?status=${state.tab}` : '';
        const r = await api.req('GET', base() + '/penalties' + q);
        state.list = r.items;
      } catch (e) { failMsg(e); }
      finally { state.loadingList = false; }
    }
    async function loadStats() {
      try { state.stats = await api.req('GET', base() + '/dashboard'); } catch (e) {}
    }
    async function loadDetail(id) {
      state.loadingDetail = true;
      try {
        state.detail = await api.req('GET', `${base()}/penalties/${id}`);
        state.selectedId = id;
        // 默认上传槽位：优先选第一个缺失槽位
        if (state.detail.missing_slots && state.detail.missing_slots.length) {
          state.up.slot = state.detail.missing_slots[0].slot;
        } else if (state.detail.required_slots && state.detail.required_slots.length) {
          state.up.slot = state.detail.required_slots[0];
        }
      } catch (e) {
        state.detail = null;
        failMsg(e);
      } finally { state.loadingDetail = false; }
    }
    async function loadNotif() {
      try {
        const r = await api.req('GET', base() + '/notifications');
        state.notifications = r.items; state.unread = r.unread;
      } catch (e) {}
    }
    async function markRead() {
      try {
        await api.req('POST', base() + '/notifications/read', {});
        state.notifications.forEach(n => n.read = true); state.unread = 0;
      } catch (e) {}
    }
    async function bootstrap() {
      await Promise.all([loadList(), loadStats(), loadNotif()]);
      if (!state.selectedId && state.list.length) selectPenalty(state.list[0].id);
      else if (state.selectedId) loadDetail(state.selectedId);
    }
    function selectPenalty(id) { loadDetail(id); }
    watch(() => state.tab, loadList);

    /* ---------- 倒计时 ---------- */
    let ticker = null, notifTimer = null;
    // 秒级脉冲：dSec 内读取它，使模板中的倒计时每秒重新渲染
    const nowPulse = ref(0);
    onMounted(() => {
      ticker = setInterval(() => { nowPulse.value++; }, 1000);
      if (state.view === 'main') {
        bootstrap();
        notifTimer = setInterval(loadNotif, 15000);
      }
    });
    onUnmounted(() => { clearInterval(ticker); clearInterval(notifTimer); });
    watch(() => state.view, v => {
      if (v === 'main' && !notifTimer) notifTimer = setInterval(loadNotif, 15000);
    });

    function dSec(d) {
      void nowPulse.value; // 建立渲染依赖
      return Math.floor(new Date(d.deadline_at).getTime() / 1000) - Math.floor(Date.now() / 1000);
    }

    /* ---------- 权限/状态派生 ---------- */
    const isManager = computed(() => api.user && api.user.role === 'manager');
    const canDraft = computed(() => {
      const d = state.detail;
      return d && (d.status === 'pending' || d.status === 'withdrawn') && dSec(d) > 0;
    });
    const canSupplement = computed(() => isManager.value && state.detail && state.detail.status === 'submitted');
    const supplementVisible = computed(() => state.detail && state.detail.status === 'submitted');
    const canSubmit = computed(() => {
      const d = state.detail;
      return d && isManager.value && (d.status === 'pending' || d.status === 'withdrawn') && dSec(d) > 0;
    });
    const canWithdraw = computed(() => state.detail && isManager.value && state.detail.status === 'submitted');
    const missingBlocking = computed(() => (state.detail && state.detail.missing_slots) || []);

    /* ---------- 证据：上传 / 删除 / 预览 ---------- */
    function onFile(e) {
      const f = e.target.files[0];
      state.up.file = f || null;
      state.up.fileName = f ? f.name : '';
    }
    async function uploadEvidence(asSupplement) {
      const d = state.detail;
      const fd = new FormData();
      fd.append('slot', state.up.slot);
      fd.append('note', state.up.note || '');
      if (state.up.file) fd.append('file', state.up.file);
      if (!state.up.file && !(state.up.note || '').trim()) { toast('请选择文件或填写文字说明', 'err'); return; }
      state.uploading = true;
      try {
        const path = asSupplement
          ? `${base()}/penalties/${d.id}/supplement`
          : `${base()}/penalties/${d.id}/draft`;
        await api.req('POST', path, fd, true);
        toast(asSupplement ? '补件已归档（原版本保留）' : '证据草稿已保存', 'ok');
        state.up.note = ''; state.up.file = null; state.up.fileName = '';
        await loadDetail(d.id);
      } catch (e) { failMsg(e); }
      finally { state.uploading = false; }
    }
    async function deleteEvidence(v) {
      state.confirm = {
        title: '删除草稿？', body: '仅未提交的草稿可以删除；该操作不影响其他版本。', busy: false,
        run: async () => {
          state.confirm.busy = true;
          try {
            await api.req('DELETE', `${base()}/penalties/${state.detail.id}/evidence/${v.id}`);
            toast('草稿已删除', 'ok'); state.confirm = null;
            await loadDetail(state.detail.id);
          } catch (e) { state.confirm.busy = false; failMsg(e); }
        },
      };
    }
    function groupedEvidence() {
      const d = state.detail; if (!d) return [];
      const map = {};
      d.evidence.forEach(v => {
        (map[v.slot] = map[v.slot] || []).push(v);
      });
      return SLOT_OPTIONS
        .filter(s => (map[s.value] || []).length > 0 || (d.required_slots || []).includes(s.value) || s.value === 'other')
        .map(s => ({ slot: s.value, label: s.label, versions: map[s.value] || [], required: (d.required_slots || []).includes(s.value) }))
        .filter(g => g.versions.length > 0 || g.required);
    }

    /* ---------- 提交 / 撤回 ---------- */
    function askSubmit() {
      if (missingBlocking.value.length > 0) {
        toast('尚缺必需证据：' + missingBlocking.value.map(s => s.label).join('、'), 'err', 5200);
        return;
      }
      state.confirm = {
        title: '确认正式提交申诉？',
        body: '提交后本单现有证据将归档，不能直接替换；审核中仍可补件（追加版本）。',
        note: '', withNote: true, busy: false,
        run: async () => {
          state.confirm.busy = true;
          try {
            await api.req('POST', `${base()}/penalties/${state.detail.id}/submit`, { note: state.confirm.note });
            toast('申诉已提交，等待平台处理', 'ok');
            state.confirm = null;
            await Promise.all([loadDetail(state.detail.id), loadList(), loadStats()]);
          } catch (e) { state.confirm.busy = false; failMsg(e); }
        },
      };
    }
    function askWithdraw() {
      state.confirm = {
        title: '撤回本次申诉？',
        body: '撤回后可在期限内重新整理并再次提交；已归档的证据版本会原样保留。',
        note: '', withNote: true, busy: false,
        run: async () => {
          state.confirm.busy = true;
          try {
            await api.req('POST', `${base()}/penalties/${state.detail.id}/withdraw`, { note: state.confirm.note });
            toast('已撤回，可在期限内重新提交', 'info');
            state.confirm = null;
            await Promise.all([loadDetail(state.detail.id), loadList(), loadStats()]);
          } catch (e) { state.confirm.busy = false; failMsg(e); }
        },
      };
    }

    /* ---------- 模拟平台回执 ---------- */
    function newReceiptKey() {
      state.mock.receipt_key = 'RCP-' + Date.now().toString(36).toUpperCase() + '-' + Math.floor(Math.random() * 900 + 100);
    }
    function setupMockFor(kind) {
      state.mock.kind = kind;
      if (!state.mock.receipt_key) newReceiptKey();
    }
    async function pushReceipt() {
      const d = state.detail;
      if (!state.mock.receipt_key.trim()) { toast('请填写回执编号（平台幂等键）', 'err'); return; }
      state.mock.busy = true;
      try {
        const body = {
          receipt_key: state.mock.receipt_key.trim(),
          kind: state.mock.kind,
          decision: state.mock.decision,
          adjusted_cents: state.mock.decision === 'adjusted' ? Number(state.mock.adjusted_cents || 0) * 100 : 0,
          message: state.mock.message || (state.mock.kind === 'correction' ? '平台复核后更正结论' : '模拟平台处理结果'),
        };
        const r = await api.req('POST', `${base()}/penalties/${d.id}/receipts`, body);
        if (r.duplicated) {
          toast('回执重复送达：已幂等忽略，结论未变化（送达次数 +1）', 'info', 5200);
        } else {
          toast('平台回执已处理：' + ({ upheld: '维持处罚', revoked: '撤销处罚', adjusted: '部分调整' }[r.decision]), 'ok');
        }
        state.mock.message = '';
        await Promise.all([loadDetail(d.id), loadList(), loadStats(), loadNotif()]);
      } catch (e) { failMsg(e); }
      finally { state.mock.busy = false; }
    }

    function resUrl(v) { return v.download_url; }
    function isImage(v) { return v.kind === 'image' || (v.mimeType || '').startsWith('image/'); }

    function statusTagClass(s) { return 'tag tag-' + s; }
    function decisionClass(d) { return 'decision-' + d; }

    return {
      state, toasts, toast, STATUS_TABS, SLOT_OPTIONS,
      currentUser: computed(() => api.user),
      fmtRT: fmtDT,
      doLogin, quickFill, logout,
      loadList, selectPenalty, loadDetail, markRead,
      isManager, canDraft, canSupplement, supplementVisible, canSubmit, canWithdraw, missingBlocking,
      onFile, uploadEvidence, deleteEvidence, groupedEvidence,
      askSubmit, askWithdraw,
      newReceiptKey, setupMockFor, pushReceipt,
      resUrl, isImage, statusTagClass, decisionClass,
      fmtDT, fmtMoney, fmtCountdown, dSec,
      lightbox: computed(() => state.lightbox),
      openImg(u) { state.lightbox = u; },
      closeImg() { state.lightbox = null; },
    };
  },
  template: `
  <div>
    <!-- ============ 登录 ============ -->
    <div v-if="state.view==='login'" class="login-wrap">
      <div class="login-card">
        <h1>🛵 申诉与证据归档</h1>
        <div class="sub">外卖商户异常订单申诉平台 · 本地模拟环境</div>
        <div class="field">
          <label>用户名</label>
          <input v-model="state.loginForm.username" @keyup.enter="doLogin" placeholder="如 wang / zhang">
        </div>
        <div class="field">
          <label>密码</label>
          <input type="password" v-model="state.loginForm.password" @keyup.enter="doLogin" placeholder="演示密码 123456">
        </div>
        <div v-if="state.loginErr" class="alert alert-danger" style="margin:4px 0 10px">{{ state.loginErr }}</div>
        <button class="btn btn-primary btn-block" @click="doLogin" :disabled="state.logging">
          <span v-if="state.logging" class="spinner"></span> 登 录
        </button>
        <div class="demo-accounts">
          <div class="t">演示账号（点击自动填充）：</div>
          <span class="chip" @click="quickFill('wang')">A店 · 王店长</span>
          <span class="chip" @click="quickFill('zhang')">A店 · 张员工</span>
          <span class="chip" @click="quickFill('li')">B店 · 李店长</span>
          <span class="chip" @click="quickFill('chen')">B店 · 陈员工</span>
        </div>
      </div>
    </div>

    <!-- ============ 主界面 ============ -->
    <template v-else>
      <div class="topbar">
        <div class="brand"><span class="dot"></span>申诉与证据归档</div>
        <span class="store-tag">{{ currentUser?.store_name }}</span>
        <span class="role-tag">{{ isManager ? '店长' : '普通员工' }} · {{ currentUser?.display_name }}</span>
        <div class="spacer"></div>
        <div style="position:relative">
          <button class="icon-btn" @click="state.showNotif=!state.showNotif; state.showNotif && markRead()">🔔
            <span v-if="state.unread" class="badge-dot">{{ state.unread }}</span>
          </button>
          <div v-if="state.showNotif" class="notif-pop">
            <div class="hd"><span>门店通知</span><button class="btn btn-ghost" @click="state.showNotif=false">关闭</button></div>
            <div style="max-height:380px;overflow-y:auto">
              <div v-if="!state.notifications.length" style="padding:24px;text-align:center;color:#8b94a3;font-size:13px">暂无通知</div>
              <div v-for="n in state.notifications" :key="n.id" class="notif-item" :class="{unread:!n.read}">
                <div class="nt">{{ n.title }}</div>
                <div class="nb">{{ n.body }}</div>
                <div class="nt-time">{{ fmtDT(n.created_at) }}</div>
              </div>
            </div>
          </div>
        </div>
        <button class="btn" @click="logout">退出登录</button>
      </div>

      <div class="layout">
        <!-- 左：列表 -->
        <div class="list-col">
          <div class="stats">
            <div class="stat warn"><div class="n">{{ state.stats.counts.pending || 0 }}</div><div class="l">待申诉</div></div>
            <div class="stat"><div class="n" style="color:#2f6bff">{{ state.stats.counts.submitted || 0 }}</div><div class="l">审核中</div></div>
            <div class="stat danger"><div class="n">{{ state.stats.counts.expired || 0 }}</div><div class="l">已超期</div></div>
            <div class="stat ok"><div class="n">{{ state.stats.counts.closed || 0 }}</div><div class="l">已结案</div></div>
          </div>
          <div class="panel">
            <div class="list-hd">
              <span class="title">处罚记录</span>
              <span v-if="state.stats.expiring_soon" class="tag tag-pending">⏰ {{ state.stats.expiring_soon }} 条 24h 内到期</span>
              <div class="tabs" style="margin-left:auto">
                <span v-for="t in STATUS_TABS" :key="t.key" class="tab"
                      :class="{active:state.tab===t.key}" @click="state.tab=t.key">{{ t.label }}</span>
              </div>
            </div>
            <div v-if="state.loadingList" class="empty">加载中…</div>
            <div v-else-if="!state.list.length" class="empty">当前分类下没有处罚记录</div>
            <div v-else>
              <div v-for="p in state.list" :key="p.id" class="p-card"
                   :class="{active:state.selectedId===p.id}" @click="selectPenalty(p.id)">
                <div class="p-row1">
                  <span class="p-type">{{ p.type_label }}</span>
                  <span :class="statusTagClass(p.status)">{{ p.status_label }}</span>
                  <span class="p-amount">{{ fmtMoney(p.amount_cents) }}</span>
                </div>
                <div class="p-row2">
                  <span class="p-order">{{ p.order_no }}</span>
                  <span v-if="p.current_decision" :class="decisionClass(p.current_decision)">
                    · {{ p.decision_label }}
                  </span>
                  <span style="margin-left:auto" class="countdown"
                        :class="{urgent: p.status==='pending' && dSec(p)>=0 && dSec(p)<86400,
                                  overdue: p.status==='expired' || (['pending','withdrawn'].includes(p.status)&&dSec(p)<=0)}">
                    {{ fmtCountdown(dSec(p)) }}
                  </span>
                </div>
              </div>
            </div>
          </div>
        </div>

        <!-- 右：详情 -->
        <div class="detail-col panel">
          <div v-if="state.loadingDetail" class="empty">加载中…</div>
          <div v-else-if="!state.detail" class="empty">请选择左侧处罚记录查看</div>
          <template v-else>
            <!-- 头部 -->
            <div class="detail-hd">
              <div class="d-title">
                {{ state.detail.type_label }}
                <span :class="statusTagClass(state.detail.status)">{{ state.detail.status_label }}</span>
                <span v-if="state.detail.current_decision" class="tag tag-closed">{{ state.detail.decision_label }}</span>
              </div>
              <div class="d-meta">
                <span>订单 <b style="font-family:ui-monospace,Menlo,monospace">{{ state.detail.order.platform_order_no }}</b></span>
                <span>处罚单号 {{ state.detail.platform_ref }}</span>
                <span>金额 {{ fmtMoney(state.detail.amount_cents) }}
                  <template v-if="state.detail.status==='closed' && state.detail.current_decision==='adjusted'">
                    → 调整为 <b>{{ fmtMoney(state.detail.adjusted_cents) }}</b>
                  </template>
                </span>
              </div>
              <div class="d-meta">
                <span>处罚下达 {{ fmtDT(state.detail.issued_at) }}</span>
                <span><b>申诉截止 {{ fmtDT(state.detail.deadline_at) }}</b></span>
                <span v-if="['pending','withdrawn'].includes(state.detail.status)">
                  <span class="countdown" :class="{urgent:dSec(state.detail)<86400,overdue:dSec(state.detail)<=0}">
                    {{ fmtCountdown(dSec(state.detail)) }}
                  </span>
                </span>
                <span v-if="state.detail.submitted_at">提交于 {{ fmtDT(state.detail.submitted_at) }}</span>
                <span v-if="state.detail.finalized_at">结案于 {{ fmtDT(state.detail.finalized_at) }}</span>
              </div>
              <div class="d-meta" v-if="state.detail.reason">原因：{{ state.detail.reason }}</div>
            </div>

            <div class="detail-body">
              <!-- 关键提示条 -->
              <div v-if="state.detail.status==='expired'" class="alert alert-danger">
                ⛔ <div><b>申诉期限已过，无法提交或补充材料。</b><br>
                  截止时间为 {{ fmtDT(state.detail.deadline_at) }}，该处罚已按超期处理。</div>
              </div>
              <div v-else-if="['pending','withdrawn'].includes(state.detail.status) && dSec(state.detail)<=0"
                   class="alert alert-danger">
                ⛔ <div><b>刚刚超过申诉期限！</b> 刷新后该记录将转为「已超期」，不能再操作。</div>
              </div>
              <div v-else-if="['pending','withdrawn'].includes(state.detail.status) && dSec(state.detail) < 86400"
                   class="alert alert-warn">
                ⏰ <div>申诉将在 <b>{{ fmtCountdown(dSec(state.detail)) }}</b> 内截止，请尽快由店长完成正式提交。</div>
              </div>
              <div v-if="state.detail.status==='submitted'" class="alert alert-info">
                📨 申诉已提交，平台处理中。期间可由店长继续<b>补件</b>（只追加、不替换），也可撤回后重申。
              </div>
              <div v-if="state.detail.status==='closed'" class="alert alert-ok">
                ✅ <div>平台已结案：<b>{{ state.detail.decision_label }}</b>
                  <span v-if="state.detail.receipts.length>1">；期间平台更正过 {{ state.detail.receipts.length-1 }} 次，历史结论均可在下方回执记录中追溯。</span>
                </div>
              </div>
              <div v-if="!isManager && canSubmit" class="alert alert-warn">
                🔒 你可以整理、上传证据，但<b>正式提交必须由店长完成</b>。材料齐备后请通知店长。
              </div>

              <!-- 缺失证据 -->
              <div class="sec" v-if="state.detail.required_slots && state.detail.required_slots.length">
                <h3>🧾 必需证据检查 <span class="hint">按处罚类型要求出餐 / 打包 / 骑手取餐凭证</span></h3>
                <div class="slots">
                  <template v-for="g in groupedEvidence()" :key="g.slot">
                    <div class="slot" :class="{missing: g.required && !g.versions.length}">
                      <div class="s-name">
                        <span>{{ g.required ? '✔' : '·' }}</span>{{ g.label }}
                      </div>
                      <div v-if="g.versions.length" class="s-count">已归档/草稿 {{ g.versions.length }} 个版本</div>
                      <div v-else class="s-miss">⚠ 尚缺此凭证</div>
                    </div>
                  </template>
                </div>
                <div v-if="missingBlocking.length" class="alert alert-danger" style="margin-top:10px">
                  尚缺：<b>{{ missingBlocking.map(s=>s.label).join('、') }}</b>，提交前请补齐。
                </div>
              </div>

              <!-- 订单时间线 -->
              <div class="sec">
                <h3>🕑 原订单时间线</h3>
                <div class="timeline">
                  <div v-for="(e,i) in state.detail.timeline" :key="i" class="tl-item done">
                    <div class="tl-dot">{{ e.icon }}</div>
                    <div class="tl-label">{{ e.label }}<span class="tl-time">{{ fmtDT(e.occurred_at) }}</span></div>
                    <div v-if="e.note" style="font-size:12px;color:#5a6472">{{ e.note }}</div>
                  </div>
                </div>
                <div v-if="state.detail.sibling_penalties.length" style="margin-top:6px">
                  <span style="font-size:12px;color:#8b94a3">同一订单的其他处罚（期限独立计算）：</span>
                  <span v-for="s in state.detail.sibling_penalties" :key="s.id" class="sib-link"
                        @click="selectPenalty(s.id)">
                    {{ s.type_label }} <span :class="statusTagClass(s.status)">{{ s.status_label }}</span>
                    · {{ fmtCountdown(dSec(s)) }}
                  </span>
                </div>
              </div>

              <!-- 证据版本 -->
              <div class="sec">
                <h3>📎 证据版本 <span class="hint">提交后归档版本不可删除或替换，补件仅追加新版本</span></h3>
                <div v-if="!state.detail.evidence.length" class="empty" style="padding:18px">暂无证据材料</div>
                <template v-for="g in groupedEvidence()" :key="'g'+g.slot">
                  <div style="font-size:12px;color:#5a6472;margin:8px 0 4px">
                    {{ g.label }}（{{ g.versions.length }}）
                  </div>
                  <div v-for="v in g.versions" :key="v.id" class="ver">
                    <div class="ver-hd">
                      <span :class="v.archived ? 'arch' : 'draft'">{{ v.archived ? '已归档' : '草稿' }}</span>
                      <span class="vname">{{ v.original_name || (v.kind==='note' ? '文字说明' : '材料') }}</span>
                      <span style="color:#8b94a3">{{ v.uploader_name }} 上传于 {{ fmtDT(v.created_at) }}</span>
                      <span v-if="v.supersedes_id" class="chain">↳ 替代版本 #{{ v.supersedes_id }}（旧版保留）</span>
                      <span style="margin-left:auto">
                        <a v-if="v.kind!=='note'" :href="resUrl(v)" target="_blank"
                           :download="v.original_name">下载</a>
                        <button v-if="!v.archived" class="btn btn-danger" style="padding:2px 10px;margin-left:8px"
                                @click="deleteEvidence(v)">删除草稿</button>
                      </span>
                    </div>
                    <div class="ver-bd">
                      <img v-if="isImage(v)" :src="resUrl(v)" @click="openImg(resUrl(v))" alt="凭证">
                      <div class="vinfo" style="flex:1">
                        <div v-if="v.note">📝 {{ v.note }}</div>
                        <div v-if="v.original_name" style="color:#8b94a3">
                          {{ v.original_name }} · {{ (v.size_bytes/1024).toFixed(1) }} KB
                        </div>
                      </div>
                    </div>
                  </div>
                </template>

                <!-- 上传区 -->
                <div v-if="canDraft || canSupplement" class="upload-box">                  <div style="font-weight:600;font-size:13px;margin-bottom:8px">
                    {{ canSupplement ? '➕ 补件（审核中，提交后立即归档为新动作）' : '➕ 整理证据草稿' }}
                  </div>
                  <div class="row">
                    <select v-model="state.up.slot">
                      <option v-for="o in SLOT_OPTIONS" :key="o.value" :value="o.value">{{ o.label }}</option>
                    </select>
                    <input type="file" accept="image/*,video/*,.pdf,.txt" @change="onFile">
                    <span style="font-size:11px;color:#8b94a3">{{ state.up.fileName || '可选，留空则只保存文字说明（≤20MB）' }}</span>
                  </div>
                  <textarea class="note-input" v-model="state.up.note"
                            placeholder="补充说明，如：出餐时间 12:03，监控显示骑手 12:08 到店取餐"></textarea>
                  <div style="margin-top:8px;display:flex;gap:8px;align-items:center">
                    <button class="btn btn-primary" @click="uploadEvidence(canSupplement)" :disabled="state.uploading">
                      <span v-if="state.uploading" class="spinner"></span>
                      {{ canSupplement ? '提交补件并归档' : '保存草稿' }}
                    </button>
                    <span v-if="canSupplement && !isManager" class="role-hint" style="margin-top:0">补件属于正式动作，仅店长可操作</span>
                  </div>
                  <div v-if="canDraft && !isManager" class="role-hint">员工可保存草稿；正式提交请联系店长。</div>
                </div>
                <div v-else-if="state.detail.status==='closed'" class="role-hint">
                  已结案：证据不可再修改。如平台后续更正，新回执与新版本仍会追加保留。
                </div>
                <div v-else-if="supplementVisible && !isManager" class="role-hint" style="padding:10px 12px;border:1px solid #f2ddb0;background:#fdf4e3;color:#92630a;border-radius:10px">
                  🔒 申诉审核中：补件属于正式动作，<b>仅店长可操作</b>。已归档的历史版本所有人均可查看。
                </div>
              </div>

              <!-- 动作按钮 -->
              <div class="sec" style="display:flex;gap:10px;flex-wrap:wrap">
                <button v-if="canSubmit" class="btn btn-primary" @click="askSubmit">
                  ✅ 由店长正式提交申诉
                </button>
                <button v-else-if="!isManager && ['pending','withdrawn'].includes(state.detail.status) && dSec(state.detail)>0"
                        class="btn" disabled title="仅店长可提交">🔒 正式提交（店长权限）</button>
                <button v-if="canWithdraw" class="btn btn-danger" @click="askWithdraw">↩ 撤回申诉</button>
              </div>

              <!-- 平台回执记录 -->
              <div class="sec" v-if="state.detail.receipts.length">
                <h3>📨 平台回执 <span class="hint">回执编号为幂等键，重复送达不会产生冲突结果</span></h3>
                <div v-for="r in state.detail.receipts" :key="r.id" class="rcpt"
                     :style="r.kind==='correction' ? 'border-color:#bfe7d1;background:#f6fdf9' : ''">
                  <div class="r1">
                    <span class="tag" :class="r.kind==='correction' ? 'tag-closed' : 'tag-submitted'">
                      {{ r.kind==='correction' ? '平台更正' : '首次结论' }}
                    </span>
                    <span :class="decisionClass(r.decision)">{{ r.decision_label }}</span>
                    <span v-if="r.decision==='adjusted'">→ {{ fmtMoney(r.adjusted_cents) }}</span>
                    <span style="font-weight:400;color:#8b94a3;font-size:11px;margin-left:auto">
                      {{ r.receipt_key }}
                    </span>
                  </div>
                  <div class="r2">{{ r.message }}</div>
                  <div class="r3">
                    收到于 {{ fmtRT(r.received_at) }}
                    <span v-if="r.delivery_count>1" class="dup-hint">
                      · 该回执重复送达 {{ r.delivery_count }} 次，已幂等去重
                    </span>
                    <span v-if="r.supersedes_receipt_id"> · 更正了回执 #{{ r.supersedes_receipt_id }}（原记录保留）</span>
                  </div>
                </div>
              </div>

              <!-- 动作链 -->
              <div class="sec">
                <h3>🧭 申诉处理记录</h3>
                <div v-if="!state.detail.actions.length" class="empty" style="padding:14px">还没有申诉动作</div>
                <div v-for="a in state.detail.actions" :key="a.id" class="act">
                  <div class="aicon" :class="'a-'+a.kind">
                    {{ {submit:'📤',supplement:'📎',withdraw:'↩',result:'📨',correction:'🔁'}[a.kind] }}
                  </div>
                  <div class="abody">
                    <div class="at1">{{ a.kind_label }}<span class="atime">{{ fmtDT(a.created_at) }} · {{ a.actor_name }}</span></div>
                    <div v-if="a.note" class="anote">{{ a.note }}</div>
                    <div v-if="a.snapshots && a.snapshots.length">
                      <span v-for="s in a.snapshots" :key="s.id" class="snap-mini">
                        {{ s.slot_label }} · {{ s.original_name || '文字说明' }}
                      </span>
                    </div>
                  </div>
                </div>
              </div>

              <!-- 模拟平台面板（店长可见） -->
              <div class="sec" v-if="isManager">
                <h3>🧪 模拟平台 / 消息接口 <span class="hint">本地模拟：推送处理结论或更正；相同编号重复推送验证幂等</span></h3>
                <div class="mock-box">
                  <div class="row">
                    <label>回执编号</label>
                    <input v-model="state.mock.receipt_key" placeholder="receipt_key（幂等键）" style="width:210px">
                    <button class="btn" @click="newReceiptKey">生成</button>
                  </div>
                  <div class="row">
                    <label>类型</label>
                    <select v-model="state.mock.kind">
                      <option value="result">首次处理结果</option>
                      <option value="correction">平台更正</option>
                    </select>
                    <label>结论</label>
                    <select v-model="state.mock.decision">
                      <option value="upheld">维持处罚</option>
                      <option value="revoked">撤销处罚</option>
                      <option value="adjusted">部分调整</option>
                    </select>
                    <template v-if="state.mock.decision==='adjusted'">
                      <label>调整后金额(元)</label>
                      <input type="number" min="0" step="0.01" v-model="state.mock.adjusted_cents" style="width:110px">
                    </template>
                  </div>
                  <div class="row">
                    <input v-model="state.mock.message" placeholder="平台说明（可选）" style="flex:1;min-width:220px">
                    <button class="btn btn-primary" @click="pushReceipt" :disabled="state.mock.busy">
                      <span v-if="state.mock.busy" class="spinner"></span> 推送回执
                    </button>
                    <button class="btn" @click="setupMockFor('result')">设为首次结果</button>
                    <button class="btn" @click="setupMockFor('correction')">设为更正</button>
                  </div>
                  <div style="font-size:11px;color:#8b94a3;line-height:1.7">
                    验证建议：先用「首次处理结果」推送一次并结案；不改编号再点一次「推送回执」，
                    系统会返回<b>重复送达提示</b>，结论保持不变、送达次数累加；
                    点「生成」换编号 + 类型选「平台更正」，可模拟撤销→改判，原回执与原结论仍完整保留。
                  </div>
                </div>
              </div>

            </div>
          </template>
        </div>
      </div>
    </template>

    <!-- toast -->
    <div class="toast-wrap">
      <div v-for="t in toasts" :key="t.id" class="toast" :class="t.type">{{ t.message }}</div>
    </div>

    <!-- 确认弹窗 -->
    <div v-if="state.confirm" class="modal-mask" @click.self="state.confirm=null">
      <div class="modal">
        <h3>{{ state.confirm.title }}</h3>
        <div style="color:#5a6472;font-size:13px">{{ state.confirm.body }}</div>
        <textarea v-if="state.confirm.withNote" class="note-input" style="margin-top:12px"
                  v-model="state.confirm.note" placeholder="附言（可选）"></textarea>
        <div class="m-btns">
          <button class="btn" @click="state.confirm=null" :disabled="state.confirm.busy">取消</button>
          <button class="btn btn-primary" @click="state.confirm.run()" :disabled="state.confirm.busy">
            <span v-if="state.confirm.busy" class="spinner"></span> 确认
          </button>
        </div>
      </div>
    </div>

    <!-- 图片预览 -->
    <div v-if="state.lightbox" class="img-view" @click="closeImg()">
      <img :src="state.lightbox" alt="证据预览">
    </div>
  </div>
  `,
}).mount('#app');
