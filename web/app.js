/* 外卖商户申诉与证据归档系统 - 前端逻辑（Vue 3 全局构建，无构建步骤） */
const { createApp } = Vue;

const PLATFORM_TOKEN = 'local-sim'; // 本地模拟平台接口令牌

function authHeaders() {
  const t = localStorage.getItem('token') || '';
  return t ? { Authorization: 'Bearer ' + t } : {};
}

async function api(path, { method = 'GET', json, form, platform } = {}) {
  const headers = platform ? { 'X-Platform-Token': PLATFORM_TOKEN } : authHeaders();
  let body;
  if (json !== undefined) { headers['Content-Type'] = 'application/json'; body = JSON.stringify(json); }
  if (form !== undefined) { body = form; }
  const resp = await fetch('/api' + path, { method, headers, body });
  let data = {};
  try { data = await resp.json(); } catch (e) { /* 空响应 */ }
  if (resp.status === 401 && !platform && path !== '/login') {
    localStorage.clear();
    location.hash = '#/';
    location.reload();
    throw new Error('登录已失效');
  }
  if (!resp.ok) throw Object.assign(new Error(data.error || ('请求失败 ' + resp.status)), { status: resp.status, data });
  return data;
}

const fmt = {
  penaltyType: t => ({ timeout: '超时', missing_item: '缺品', rejected: '拒单' }[t] || t),
  status: s => ({
    pending: '待申诉', submitted: '申诉处理中', withdrawn: '已撤回',
    upheld: '维持处罚', revoked: '已撤销', partial: '部分调整', expired: '已超期',
  }[s] || s),
  statusClass: s => ({
    pending: 'warn', submitted: 'info', withdrawn: 'muted',
    upheld: 'bad', revoked: 'good', partial: 'warn', expired: 'bad',
  }[s] || 'muted'),
  outcome: o => ({ upheld: '维持处罚', revoked: '撤销处罚', partial: '部分调整' }[o] || o),
  event: t => ({ accepted: '商户接单', meal_ready: '出餐完成', packed: '打包完成', rider_pickup: '骑手取餐', delivered: '送达' }[t] || t),
  versionType: t => ({ submission: '正式提交', supplement: '补件', withdrawal: '撤回', correction: '平台更正' }[t] || t),
  money: n => '¥' + Number(n).toFixed(2),
};

function remainText(deadline) {
  const ms = new Date(deadline.replace(' ', 'T')) - Date.now();
  if (ms <= 0) return '已超期';
  const h = Math.floor(ms / 3600000), m = Math.floor((ms % 3600000) / 60000);
  if (h >= 24) return `剩余 ${Math.floor(h / 24)} 天 ${h % 24} 小时`;
  if (h >= 1) return `剩余 ${h} 小时 ${m} 分`;
  return `剩余 ${m} 分`;
}

/* ---------------- 处罚列表 ---------------- */
const PenaltyList = {
  data: () => ({ rows: [], loading: true, error: '', tick: 0 }),
  async created() {
    try {
      const d = await api('/penalties');
      this.rows = d.penalties;
    } catch (e) { this.error = e.message; }
    this.loading = false;
    this.timer = setInterval(() => { this.tick++; }, 30000);
  },
  unmounted() { clearInterval(this.timer); },
  methods: {
    fmt, remainText,
    go(id) { location.hash = '#/penalties/' + id; },
  },
  template: `
  <section>
    <h2>处罚记录</h2>
    <p class="muted">每条处罚单独计算申诉期限（下达后 72 小时）。点击行查看订单时间线与证据缺口。</p>
    <p v-if="error" class="error">{{ error }}</p>
    <p v-if="loading">加载中…</p>
    <p v-else-if="!rows.length" class="muted">暂无处罚记录</p>
    <table v-else class="tbl">
      <thead><tr>
        <th>类型</th><th>订单号</th><th>罚款</th><th>原因</th>
        <th>下达时间</th><th>申诉截止</th><th>倒计时</th><th>状态</th>
      </tr></thead>
      <tbody>
        <tr v-for="p in rows" :key="p.id" @click="go(p.id)" class="clickable">
          <td><span class="tag">{{ fmt.penaltyType(p.type) }}</span></td>
          <td>{{ p.order_no }}</td>
          <td>{{ fmt.money(p.amount) }}</td>
          <td class="reason">{{ p.reason }}</td>
          <td>{{ p.issued_at }}</td>
          <td>{{ p.deadline }}</td>
          <td :data-tick="tick" :class="{urgent: p.status==='pending' && p.remaining_seconds < 4*3600, bad: p.status==='expired'}">
            {{ p.status === 'expired' ? '已超期' : (['pending','withdrawn'].includes(p.status) ? remainText(p.deadline) : '—') }}
          </td>
          <td><span class="badge" :class="fmt.statusClass(p.status)">{{ fmt.status(p.status) }}</span></td>
        </tr>
      </tbody>
    </table>
  </section>`,
};

/* ---------------- 处罚详情 ---------------- */
const PenaltyDetail = {
  props: ['id', 'me'],
  data: () => ({
    d: null, error: '', actionError: '', actionOk: '',
    upload: { category: 'meal_ready', note: '', file: null },
    submitNote: '', supp: { category: 'other', note: '', file: null },
    withdrawNote: '', correctionNote: '',
    busy: false, showPlatform: false, tick: 0,
  }),
  async created() {
    await this.load();
    this.timer = setInterval(() => { this.tick++; this.load(true); }, 3000);
  },
  unmounted() { clearInterval(this.timer); },
  computed: {
    p() { return this.d && this.d.penalty; },
    appeal() { return this.d && this.d.appeal; },
    result() { return this.appeal && this.appeal.result; },
    isManager() { return this.me.role === 'manager'; },
    canUploadDraft() {
      if (!this.p) return false;
      if (this.appeal && this.appeal.status !== 'withdrawn') return false;
      return ['pending', 'withdrawn'].includes(this.p.status);
    },
    canSubmit() {
      return this.p && ['pending', 'withdrawn'].includes(this.p.status) && this.d.evidence.length > 0;
    },
    inProgress() { return this.appeal && this.appeal.status === 'submitted'; },
  },
  methods: {
    fmt, remainText,
    async load(silent) {
      try {
        this.d = await api('/penalties/' + this.id);
        if (!silent) this.actionError = '';
      } catch (e) { if (!silent) this.error = e.message; }
    },
    onFile(e, which) { this[which].file = e.target.files[0] || null; },
    async doUpload() {
      if (!this.upload.file) { this.actionError = '请选择文件'; return; }
      this.busy = true; this.actionError = ''; this.actionOk = '';
      const fd = new FormData();
      fd.append('file', this.upload.file);
      fd.append('category', this.upload.category);
      fd.append('note', this.upload.note);
      try {
        await api(`/penalties/${this.id}/evidence`, { method: 'POST', form: fd });
        this.upload = { category: 'meal_ready', note: '', file: null };
        this.$refs.fileInput.value = '';
        this.actionOk = '证据已上传';
        await this.load();
      } catch (e) { this.actionError = e.message; }
      this.busy = false;
    },
    async doSubmit() {
      if (!confirm('确认正式提交申诉？提交后证据不可替换，补充材料需通过补件生成新版本。')) return;
      this.busy = true; this.actionError = ''; this.actionOk = '';
      const fd = new FormData();
      fd.append('note', this.submitNote);
      try {
        await api(`/penalties/${this.id}/submit`, { method: 'POST', form: fd });
        this.actionOk = '申诉已提交，等待平台处理';
        await this.load();
      } catch (e) { this.actionError = e.message; }
      this.busy = false;
    },
    async doSupplement() {
      if (!this.supp.file) { this.actionError = '请选择补充文件'; return; }
      this.busy = true; this.actionError = ''; this.actionOk = '';
      const fd = new FormData();
      fd.append('file', this.supp.file);
      fd.append('category', this.supp.category);
      fd.append('note', this.supp.note);
      try {
        const r = await api(`/appeals/${this.appeal.id}/supplement`, { method: 'POST', form: fd });
        this.supp = { category: 'other', note: '', file: null };
        this.$refs.suppInput.value = '';
        this.actionOk = `已补件，生成版本 v${r.version}（原版本保留）`;
        await this.load();
      } catch (e) { this.actionError = e.message; }
      this.busy = false;
    },
    async doWithdraw() {
      if (!confirm('确认撤回申诉？撤回会保留记录并生成撤回版本。')) return;
      this.busy = true; this.actionError = ''; this.actionOk = '';
      const fd = new FormData();
      fd.append('note', this.withdrawNote);
      try {
        const r = await api(`/appeals/${this.appeal.id}/withdraw`, { method: 'POST', form: fd });
        this.actionOk = `已撤回（记录保留于 v${r.version}）`;
        await this.load();
      } catch (e) { this.actionError = e.message; }
      this.busy = false;
    },
    async devSimulate() {
      this.actionError = '';
      try {
        await api(`/dev/appeals/${this.appeal.id}/simulate-result`, { method: 'POST' });
        await this.load();
      } catch (e) { this.actionError = e.message; }
    },
    async devRedeliver() {
      this.actionError = '';
      try {
        const r = await api(`/dev/appeals/${this.appeal.id}/redeliver-receipt`, { method: 'POST' });
        this.actionOk = `回执 ${r.receipt_id} 重复送达：幂等处理，结论仍为「${fmt.outcome(r.outcome)}」，未产生新结论`;
      } catch (e) { this.actionError = e.message; }
    },
    async devCorrect() {
      if (!this.correctionNote.trim()) { this.actionError = '请填写平台更正说明'; return; }
      this.actionError = '';
      try {
        await api('/platform/corrections', { method: 'POST', platform: true,
          json: { appeal_id: this.appeal.id, note: this.correctionNote } });
        this.correctionNote = '';
        this.actionOk = '平台更正已登记为新版本';
        await this.load();
      } catch (e) { this.actionError = e.message; }
    },
    async download(item) {
      const resp = await fetch('/api/files/' + item.id, { headers: authHeaders() });
      if (!resp.ok) { this.actionError = '文件下载失败'; return; }
      const blob = await resp.blob();
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = item.file_name;
      a.click();
      URL.revokeObjectURL(a.href);
    },
  },
  template: `
  <section v-if="d">
    <p><a href="#/">← 返回处罚列表</a></p>

    <div v-if="p.status === 'expired'" class="banner bad">
      ⚠ 已超过申诉期限（截止 {{ p.deadline }}），无法提交申诉。如有疑问请联系平台运营。
    </div>
    <div v-else-if="['pending','withdrawn'].includes(p.status)" class="banner warn">
      申诉截止：{{ p.deadline }}（{{ remainText(p.deadline) }}），逾期将无法提交。
    </div>

    <div class="grid2">
      <div class="card">
        <h3>处罚信息 <span class="badge" :class="fmt.statusClass(p.status)">{{ fmt.status(p.status) }}</span></h3>
        <table class="kv">
          <tr><td>处罚类型</td><td>{{ fmt.penaltyType(p.type) }}</td></tr>
          <tr><td>罚款金额</td><td>{{ fmt.money(p.amount) }}</td></tr>
          <tr><td>原因</td><td>{{ p.reason }}</td></tr>
          <tr><td>下达时间</td><td>{{ p.issued_at }}</td></tr>
          <tr><td>申诉截止</td><td><b>{{ p.deadline }}</b></td></tr>
        </table>
        <h3>订单 {{ d.order.order_no }}</h3>
        <table class="kv">
          <tr><td>下单时间</td><td>{{ d.order.placed_at }}</td></tr>
          <tr><td>订单金额</td><td>{{ fmt.money(d.order.amount) }}</td></tr>
        </table>
      </div>

      <div class="card">
        <h3>原订单时间线</h3>
        <ul class="timeline">
          <li v-for="(e, i) in d.timeline" :key="i">
            <span class="tldot"></span>
            <div><b>{{ fmt.event(e.type) }}</b><span class="muted">　{{ e.at }}</span>
              <div class="muted" v-if="e.note">{{ e.note }}</div></div>
          </li>
        </ul>
        <h3>证据缺口</h3>
        <div class="reqs">
          <div v-for="r in d.requirements" :key="r.category" class="req" :class="r.covered ? 'ok' : 'miss'">
            <b>{{ r.covered ? '✓' : '✗' }} {{ r.label }}</b>
            <span class="muted">
              时间线记录：{{ r.event_present ? '有' : '无' }} · 已上传证据：{{ r.evidence_count }} 份
            </span>
          </div>
        </div>
        <p v-if="d.missing.length" class="bad">尚缺：{{ d.missing.join('、') }}证据，请尽快补充。</p>
        <p v-else class="good">关键证据类别已覆盖。</p>
      </div>
    </div>

    <div class="card">
      <h3>证据材料（{{ d.evidence.length }}）</h3>
      <p class="muted">证据一经登记不可替换、不可删除；提交后的补充、撤回与平台更正都会生成新版本并保留原版本。</p>
      <table v-if="d.evidence.length" class="tbl">
        <thead><tr><th>类别</th><th>文件</th><th>备注</th><th>上传人</th><th>时间</th><th></th></tr></thead>
        <tbody>
          <tr v-for="it in d.evidence" :key="it.id">
            <td>{{ it.category_label }}</td>
            <td>{{ it.file_name }}</td>
            <td class="reason">{{ it.note }}</td>
            <td>{{ it.uploader_name }}</td>
            <td>{{ it.created_at }}</td>
            <td><a href="javascript:;" @click="download(it)">下载</a></td>
          </tr>
        </tbody>
      </table>
      <p v-else class="muted">尚未上传证据。</p>

      <div v-if="canUploadDraft" class="uploadbar">
        <select v-model="upload.category">
          <option value="meal_ready">出餐</option>
          <option value="packed">打包</option>
          <option value="rider_pickup">骑手取餐</option>
          <option value="other">其他</option>
        </select>
        <input type="file" ref="fileInput" @change="onFile($event, 'upload')">
        <input v-model="upload.note" placeholder="备注（可选）">
        <button class="btn" :disabled="busy" @click="doUpload">上传证据</button>
        <span class="muted">员工与店长均可上传</span>
      </div>
      <p v-else-if="appeal && appeal.status === 'submitted'" class="muted">
        申诉已提交，补充材料须由店长通过下方「补件」提交（生成新版本）。
      </p>
    </div>

    <div class="card" v-if="['pending','withdrawn'].includes(p.status)">
      <h3>提交申诉</h3>
      <div v-if="!isManager" class="banner muted">正式提交必须由店长完成。您可以继续整理证据，并联系店长提交。</div>
      <div v-else-if="p.status === 'expired'" class="banner bad">已超过申诉期限（截止 {{ p.deadline }}），无法提交。</div>
      <template v-else>
        <textarea v-model="submitNote" placeholder="申诉说明（可选）" rows="2"></textarea>
        <button class="btn primary" :disabled="busy || !canSubmit" @click="doSubmit">
          店长正式提交申诉
        </button>
        <span v-if="!d.evidence.length" class="bad">　请先上传至少一份证据材料</span>
      </template>
    </div>

    <div class="card" v-if="appeal">
      <h3>申诉进度与版本归档</h3>
      <p>
        状态：<span class="badge" :class="appeal.status === 'decided' ? 'good' : (appeal.status === 'submitted' ? 'info' : 'muted')">
          {{ { submitted: '平台处理中', withdrawn: '已撤回', decided: '已有结论' }[appeal.status] }}</span>
        提交时间：{{ appeal.submitted_at }}　当前版本：v{{ appeal.current_version }}
      </p>

      <div v-if="result" class="result" :class="result.outcome">
        <h3>平台处理结论：{{ fmt.outcome(result.outcome) }}
          <span v-if="result.outcome === 'partial'">（调整后 {{ fmt.money(result.adjusted_amount) }}）</span>
        </h3>
        <p>{{ result.reason }}</p>
        <p class="muted">回执号 {{ result.receipt_id }} · 收到时间 {{ result.received_at }} · 关联材料版本 v{{ result.version_no }}</p>
      </div>

      <ul class="versions">
        <li v-for="v in appeal.versions" :key="v.id">
          <b>v{{ v.version_no }} · {{ fmt.versionType(v.type) }}</b>
          <span class="muted">　{{ v.created_by_name }} · {{ v.created_at }}</span>
          <div v-if="v.note" class="muted">备注：{{ v.note }}</div>
          <div class="muted">材料快照：{{ v.items.length ? v.items.map(i => i.file_name).join('、') : '（无）' }}</div>
        </li>
      </ul>

      <div v-if="inProgress && isManager" class="uploadbar">
        <select v-model="supp.category">
          <option value="meal_ready">出餐</option>
          <option value="packed">打包</option>
          <option value="rider_pickup">骑手取餐</option>
          <option value="other">其他</option>
        </select>
        <input type="file" ref="suppInput" @change="onFile($event, 'supp')">
        <input v-model="supp.note" placeholder="补件说明">
        <button class="btn" :disabled="busy" @click="doSupplement">补件（生成新版本）</button>
        <button class="btn danger" :disabled="busy" @click="doWithdraw">撤回申诉</button>
      </div>
      <p v-else-if="inProgress" class="muted">申诉处理中。补件与撤回需店长操作。</p>

      <div class="platform-sim">
        <a href="javascript:;" @click="showPlatform = !showPlatform">{{ showPlatform ? '收起' : '展开' }}模拟平台（本地）</a>
        <div v-if="showPlatform" class="simbox">
          <p class="muted">以下为本地模拟的平台接口，用于演示结论回传、重复回执幂等与平台更正。</p>
          <button v-if="inProgress" class="btn" @click="devSimulate">立即生成平台结论</button>
          <button v-if="result" class="btn" @click="devRedeliver">重新送达上次回执（幂等验证）</button>
          <div v-if="appeal">
            <input v-model="correctionNote" placeholder="平台更正说明">
            <button class="btn" @click="devCorrect">平台更正（生成新版本）</button>
          </div>
        </div>
      </div>
    </div>

    <p v-if="actionError" class="error">{{ actionError }}</p>
    <p v-if="actionOk" class="good">{{ actionOk }}</p>
  </section>
  <p v-else-if="error" class="error">{{ error }}</p>
  <p v-else>加载中…</p>`,
};

/* ---------------- 消息通知 ---------------- */
const NotificationsView = {
  data: () => ({ rows: [] }),
  async created() { await this.load(); },
  methods: {
    async load() {
      const d = await api('/notifications');
      this.rows = d.notifications;
    },
    async readAll() {
      await api('/notifications/read-all', { method: 'POST' });
      await this.load();
      this.$root.refreshUnread();
    },
  },
  template: `
  <section>
    <h2>消息通知 <button v-if="rows.some(n => !n.read)" class="btn" @click="readAll">全部标为已读</button></h2>
    <p v-if="!rows.length" class="muted">暂无消息</p>
    <ul class="notifs">
      <li v-for="n in rows" :key="n.id" :class="{unread: !n.read}">
        <span class="muted">{{ n.created_at }}</span>　{{ n.message }}
      </li>
    </ul>
  </section>`,
};

/* ---------------- 根组件 ---------------- */
function parseHash() {
  const h = location.hash || '#/';
  const m = h.match(/^#\/penalties\/(\d+)/);
  if (m) return { name: 'detail', id: m[1] };
  if (h.startsWith('#/notifications')) return { name: 'notifications' };
  return { name: 'list' };
}

const app = createApp({
  components: { PenaltyList, PenaltyDetail, NotificationsView },
  data: () => ({
    user: JSON.parse(localStorage.getItem('user') || 'null'),
    route: parseHash(),
    loginForm: { username: '', password: '' },
    loginError: '', logging: false, unread: 0,
  }),
  created() {
    window.addEventListener('hashchange', () => { this.route = parseHash(); });
    if (this.user) this.refreshUnread();
    this.unreadTimer = setInterval(() => { if (this.user) this.refreshUnread(); }, 15000);
  },
  methods: {
    async doLogin() {
      this.logging = true; this.loginError = '';
      try {
        const d = await api('/login', { method: 'POST', json: this.loginForm });
        localStorage.setItem('token', d.token);
        localStorage.setItem('user', JSON.stringify(d.user));
        this.user = d.user;
        this.refreshUnread();
      } catch (e) { this.loginError = e.message; }
      this.logging = false;
    },
    logout() {
      localStorage.clear();
      location.hash = '#/';
      location.reload();
    },
    async refreshUnread() {
      try {
        const d = await api('/notifications');
        this.unread = d.notifications.filter(n => !n.read).length;
      } catch (e) { /* 忽略 */ }
    },
  },
});

app.component('penalty-list', PenaltyList);
app.component('penalty-detail', PenaltyDetail);
app.component('notifications-view', NotificationsView);
app.mount('#app');
