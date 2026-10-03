package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"appeal/internal/appeal"
	"appeal/internal/httpapi"
	"appeal/internal/models"
	"appeal/internal/store"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type harness struct {
	t      *testing.T
	r      *gin.Engine
	db     *gorm.DB
	svc    *appeal.Service
	clock  *time.Time
	dir    string
	tokens map[string]string // username -> token
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	uploads := filepath.Join(dir, "uploads")
	if err := store.Seed(db, uploads); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := appeal.NewService(db, uploads)
	now := time.Now()
	svc.SetClock(func() time.Time { return now })
	r := httpapi.NewRouter(db, svc, nil, uploads)
	h := &harness{t: t, r: r, db: db, svc: svc, clock: &now, dir: dir, tokens: map[string]string{}}
	for _, u := range []string{"wang", "zhang", "li", "chen"} {
		h.tokens[u] = h.login(u, "123456")
	}
	return h
}

func (h *harness) login(user, pwd string) string {
	body, _ := json.Marshal(gin.H{"username": user, "password": pwd})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.r.ServeHTTP(w, req)
	if w.Code != 200 {
		h.t.Fatalf("login %s: %d %s", user, w.Code, w.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Token
}

func (h *harness) do(method, path, user string, body any) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+h.tokens[user])
	}
	w := httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	return w
}

// uploadDraft 以 multipart 上传一份证据（tiny PNG），可只传纯文字 note。
func (h *harness) uploadDraft(user, path string, withFile bool, fields map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if withFile {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, fields["_filename"]))
		hdr.Set("Content-Type", "image/png")
		fw, _ := mw.CreatePart(hdr)
		_, _ = fw.Write([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 1, 2, 3})
	}
	_ = mw.Close()
	req, _ := http.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+h.tokens[user])
	w := httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	return w
}

func jget(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("json: %v body=%s", err, w.Body.String())
	}
	return m
}

// ---------- 1. 跨门店数据隔离 ----------

func TestCrossStoreIsolation(t *testing.T) {
	h := newHarness(t)
	// B 店店长访问 A 店门店作用域 → 404
	w := h.do("GET", "/api/stores/1/penalties", "li", nil)
	if w.Code != 404 {
		t.Fatalf("B店访问A店列表, want 404, got %d: %s", w.Code, w.Body.String())
	}
	// B 店店长直接访问 A 店处罚详情 → 404（不暴露资源存在性）
	w = h.do("GET", "/api/stores/1/penalties/1", "li", nil)
	if w.Code != 404 {
		t.Fatalf("B店访问A店处罚, want 404, got %d", w.Code)
	}
	// B 店店长尝试对 A 店处罚推回执 → 404
	w = h.do("POST", "/api/stores/1/penalties/1/receipts", "li", gin.H{
		"receipt_key": "X-B-1", "kind": "result", "decision": "revoked",
	})
	if w.Code != 404 {
		t.Fatalf("B店对A店推回执, want 404, got %d", w.Code)
	}
	// A 店自己可见处罚 1
	w = h.do("GET", "/api/stores/1/penalties/1", "wang", nil)
	if w.Code != 200 {
		t.Fatalf("A店访问本店处罚, want 200, got %d: %s", w.Code, w.Body.String())
	}
	// B 店列表不应出现 A 店处罚
	w = h.do("GET", "/api/stores/2/penalties", "li", nil)
	if w.Code != 200 {
		t.Fatalf("B店列表: %d", w.Code)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Items) != 1 || list.Items[0]["platform_ref"] != "P-B-2001" {
		t.Fatalf("B店只应看到自己的 1 条处罚, got %v", list.Items)
	}
	// 未登录 → 401
	w = h.do("GET", "/api/stores/1/penalties", "", nil)
	if w.Code != 401 {
		t.Fatalf("未登录 want 401, got %d", w.Code)
	}
	// 附件下载同样隔离：拿到 A 店证据 ID 后用 B 店账号下载 → 404
	w = h.uploadDraft("zhang", "/api/stores/1/penalties/1/draft", true,
		map[string]string{"slot": "meal_ready", "note": "n", "_filename": "a.png"})
	if w.Code != 201 {
		t.Fatalf("员工传草稿: %d %s", w.Code, w.Body.String())
	}
	ev := jget(t, w)
	dl := fmt.Sprintf("/api/evidence/%v/download", ev["id"])
	w = h.do("GET", dl, "li", nil)
	if w.Code != 404 {
		t.Fatalf("B店下载A店证据 want 404, got %d", w.Code)
	}
	w = h.do("GET", dl, "wang", nil)
	if w.Code != 200 {
		t.Fatalf("A店下载本店证据 want 200, got %d", w.Code)
	}
}

// ---------- 2. 员工可整理证据，正式提交仅店长 ----------

func TestRoleBoundary(t *testing.T) {
	h := newHarness(t)
	// 员工可以传草稿
	w := h.uploadDraft("zhang", "/api/stores/1/penalties/2/draft", true,
		map[string]string{"slot": "packed", "note": "打包台截图", "_filename": "p.png"})
	if w.Code != 201 {
		t.Fatalf("员工上传草稿 want 201, got %d: %s", w.Code, w.Body.String())
	}
	// 员工不能正式提交
	w = h.do("POST", "/api/stores/1/penalties/2/submit", "zhang", gin.H{"note": "请处理"})
	if w.Code != 403 {
		t.Fatalf("员工提交 want 403, got %d", w.Code)
	}
	// 员工不能补件
	w = h.do("POST", "/api/stores/1/penalties/4/supplement", "zhang", gin.H{})
	if w.Code != 403 {
		t.Fatalf("员工补件 want 403, got %d", w.Code)
	}
	// 员工不能撤回
	w = h.do("POST", "/api/stores/1/penalties/4/withdraw", "zhang", gin.H{})
	if w.Code != 403 {
		t.Fatalf("员工撤回 want 403, got %d", w.Code)
	}
	// 员工不能模拟平台回执
	w = h.do("POST", "/api/stores/1/penalties/4/receipts", "zhang", gin.H{
		"receipt_key": "X", "kind": "result", "decision": "revoked",
	})
	if w.Code != 403 {
		t.Fatalf("员工推回执 want 403, got %d", w.Code)
	}
	// 店长提交成功（p2 缺品只需 packed，上面员工已传）
	w = h.do("POST", "/api/stores/1/penalties/2/submit", "wang", gin.H{"note": "证据齐全"})
	if w.Code != 200 {
		t.Fatalf("店长提交 want 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- 3. 期限边界 ----------

func TestDeadlineBoundary(t *testing.T) {
	h := newHarness(t)

	// 构造一条 deadline 正好等于当前时钟的处罚：边界当刻仍可提交
	deadline := *h.clock
	p := models.Penalty{StoreID: 1, OrderID: 1, PlatformRef: "P-EDGE-EQ", Type: models.PenaltyRejected,
		DeadlineAt: deadline, IssuedAt: deadline.Add(-time.Hour), Status: models.StatusPending}
	if err := h.db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	w := h.do("POST", fmt.Sprintf("/api/stores/1/penalties/%d/submit", p.ID), "wang", nil)
	if w.Code != 200 {
		t.Fatalf("deadline 当刻提交 want 200, got %d: %s", w.Code, w.Body.String())
	}

	// deadline 早 1 纳秒 → 超期，拒绝提交并落库 expired
	p2m := models.Penalty{StoreID: 1, OrderID: 1, PlatformRef: "P-EDGE-PAST", Type: models.PenaltyRejected,
		DeadlineAt: h.clock.Add(-time.Nanosecond), IssuedAt: h.clock.Add(-2 * time.Hour), Status: models.StatusPending}
	if err := h.db.Create(&p2m).Error; err != nil {
		t.Fatal(err)
	}
	w = h.do("POST", fmt.Sprintf("/api/stores/1/penalties/%d/submit", p2m.ID), "wang", nil)
	if w.Code != 409 {
		t.Fatalf("超期提交 want 409, got %d", w.Code)
	}
	var got models.Penalty
	h.db.First(&got, p2m.ID)
	if got.Status != models.StatusExpired {
		t.Fatalf("超期后状态应落库 expired, got %s", got.Status)
	}

	// 超期处罚连草稿都不能再补
	w = h.uploadDraft("zhang", fmt.Sprintf("/api/stores/1/penalties/%d/draft", p2m.ID), true,
		map[string]string{"slot": "other", "_filename": "x.png"})
	if w.Code != 409 {
		t.Fatalf("超期后补草稿 want 409, got %d", w.Code)
	}

	// 种子中的已超期处罚（id=3）直接被拒
	w = h.do("POST", "/api/stores/1/penalties/3/submit", "wang", nil)
	if w.Code != 409 {
		t.Fatalf("种子超期处罚提交 want 409, got %d", w.Code)
	}

	// SweepExpired 不应改动未到期记录；到期的 pending/withdrawn 转 expired
	future := models.Penalty{StoreID: 1, OrderID: 1, PlatformRef: "P-SWEEP", Type: models.PenaltyRejected,
		DeadlineAt: h.clock.Add(time.Hour), IssuedAt: h.clock.Add(-time.Hour), Status: models.StatusPending}
	if err := h.db.Create(&future).Error; err != nil {
		t.Fatal(err)
	}
	*h.clock = h.clock.Add(2 * time.Hour)
	n, err := h.svc.SweepExpired()
	if err != nil {
		t.Fatal(err)
	}
	// 至少包含 future 与种子里 deadline +2h 的 p2（但 p2 在本用例已被店长提交，不计入）
	if n < 1 {
		t.Fatalf("sweep 至少应处理 1 条, got %d", n)
	}
	h.db.First(&got, future.ID)
	if got.Status != models.StatusExpired {
		t.Fatalf("sweep 后应为 expired, got %s", got.Status)
	}
}

// ---------- 4. 缺证校验 ----------

func TestMissingEvidenceRejected(t *testing.T) {
	h := newHarness(t)
	// p1 为超时处罚，需要出餐/打包/骑手取餐三项，直接提交 → 422 并列出缺失
	w := h.do("POST", "/api/stores/1/penalties/1/submit", "wang", nil)
	if w.Code != 422 {
		t.Fatalf("缺证提交 want 422, got %d: %s", w.Code, w.Body.String())
	}
	m := jget(t, w)
	if msg, _ := m["message"].(string); msg == "" || !contains(msg, "出餐") {
		t.Fatalf("缺失提示应含「出餐」, got %v", m["message"])
	}
	// 详情接口应列出缺失槽位
	w = h.do("GET", "/api/stores/1/penalties/1", "wang", nil)
	m = jget(t, w)
	missing, _ := m["missing_slots"].([]any)
	if len(missing) != 3 {
		t.Fatalf("超时处罚缺 3 槽, got %v", missing)
	}
	// 拒单处罚无强制槽位，可直接提交（取一条期限内的 pending 拒单：种子 p6 已撤回，用它）
	w = h.do("POST", "/api/stores/1/penalties/6/submit", "wang", nil)
	if w.Code != 200 {
		t.Fatalf("撤回态重申(无缺证) want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}

// ---------- 5. 证据版本不可替换、补件追加 ----------

func TestEvidenceVersioning(t *testing.T) {
	h := newHarness(t)
	// 员工放草稿、店长提交
	w := h.uploadDraft("zhang", "/api/stores/1/penalties/2/draft", true,
		map[string]string{"slot": "packed", "note": "v1", "_filename": "v1.png"})
	if w.Code != 201 {
		t.Fatalf("draft: %s", w.Body.String())
	}
	v1 := jget(t, w)
	w = h.do("POST", "/api/stores/1/penalties/2/submit", "wang", nil)
	if w.Code != 200 {
		t.Fatalf("submit: %s", w.Body.String())
	}
	// 归档版本不可删
	vid := uint(v1["id"].(float64))
	w = h.do("DELETE", fmt.Sprintf("/api/stores/1/penalties/2/evidence/%d", vid), "wang", nil)
	if w.Code != 409 {
		t.Fatalf("删除归档版本 want 409, got %d", w.Code)
	}
	// 审核中补件：新版本入链，旧版本保留
	w = h.uploadDraft("wang", "/api/stores/1/penalties/2/supplement", true,
		map[string]string{"slot": "packed", "note": "v2 更清晰截图", "_filename": "v2.png"})
	if w.Code != 201 {
		t.Fatalf("supplement want 201, got %d: %s", w.Code, w.Body.String())
	}
	v2 := jget(t, w)
	if v2["supersedes_id"].(float64) != float64(vid) {
		t.Fatalf("v2 应指向 v1, got supersedes=%v", v2["supersedes_id"])
	}
	var cnt int64
	h.db.Model(&models.EvidenceVersion{}).Where("penalty_id = ?", 2).Count(&cnt)
	if cnt != 2 {
		t.Fatalf("补件后应保留 2 个版本, got %d", cnt)
	}
	// 动作链：提交 + 补件，且补件动作关联新快照
	var acts []models.AppealAction
	h.db.Where("penalty_id = ?", 2).Order("id ASC").Find(&acts)
	if len(acts) != 2 || acts[0].Kind != "submit" || acts[1].Kind != "supplement" {
		t.Fatalf("动作链异常: %+v", acts)
	}
	if len(acts[1].SnapshotIDs) != 1 || acts[1].SnapshotIDs[0] != uint(v2["id"].(float64)) {
		t.Fatalf("补件动作应快照 v2, got %v", acts[1].SnapshotIDs)
	}
	// 提交动作快照仍指向 v1（不可变）
	if len(acts[0].SnapshotIDs) != 1 || acts[0].SnapshotIDs[0] != vid {
		t.Fatalf("提交动作快照应仍是 v1, got %v", acts[0].SnapshotIDs)
	}
	// 非审核中状态补件被拒
	w = h.uploadDraft("wang", "/api/stores/1/penalties/1/supplement", true,
		map[string]string{"slot": "packed", "_filename": "x.png"})
	if w.Code != 400 {
		t.Fatalf("待申诉状态补件 want 400, got %d", w.Code)
	}
}

// ---------- 6. 相同平台回执重复送达（含并发） ----------

func TestDuplicateReceipt(t *testing.T) {
	h := newHarness(t)
	// p1 超时处罚需要三个槽位，逐一补齐
	for _, slot := range []string{"meal_ready", "packed", "rider_pickup"} {
		w := h.uploadDraft("zhang", "/api/stores/1/penalties/1/draft", true,
			map[string]string{"slot": slot, "_filename": slot + ".png"})
		if w.Code != 201 {
			t.Fatalf("补槽 %s: %s", slot, w.Body.String())
		}
	}
	w := h.do("POST", "/api/stores/1/penalties/1/submit", "wang", nil)
	if w.Code != 200 {
		t.Fatalf("证据齐全后提交: %s", w.Body.String())
	}

	receipt := gin.H{"receipt_key": "RCP-DUP-1", "kind": "result", "decision": "upheld", "message": "维持"}
	w = h.do("POST", "/api/stores/1/penalties/1/receipts", "wang", receipt)
	if w.Code != 200 {
		t.Fatalf("首次回执 want 200, got %d: %s", w.Code, w.Body.String())
	}
	first := jget(t, w)
	if first["duplicated"] != false {
		t.Fatalf("首次不应是重复")
	}

	// 同 key 再送（模拟消息重复投递），且内容冲突（revoked）——结论绝不能变
	conflict := gin.H{"receipt_key": "RCP-DUP-1", "kind": "result", "decision": "revoked", "message": "冲突内容"}
	w = h.do("POST", "/api/stores/1/penalties/1/receipts", "wang", conflict)
	if w.Code != 200 {
		t.Fatalf("重复回执 want 200 幂等, got %d: %s", w.Code, w.Body.String())
	}
	dup := jget(t, w)
	if dup["duplicated"] != true || dup["decision"] != "upheld" {
		t.Fatalf("重复回执必须幂等且结论不变, got %v", dup)
	}
	if dup["delivery_count"].(float64) != 2 {
		t.Fatalf("送达次数应为 2, got %v", dup["delivery_count"])
	}

	// 库里只有一条回执、一条 result 动作、一条结论通知
	var rc int64
	h.db.Model(&models.PlatformReceipt{}).Where("receipt_key = ?", "RCP-DUP-1").Count(&rc)
	if rc != 1 {
		t.Fatalf("回执记录只能有 1 条, got %d", rc)
	}
	var ac int64
	h.db.Model(&models.AppealAction{}).Where("penalty_id = ? AND kind = ?", 1, "result").Count(&ac)
	if ac != 1 {
		t.Fatalf("result 动作只能有 1 条, got %d", ac)
	}
	var nc int64
	h.db.Model(&models.Notification{}).Where("store_id = 1 AND ref_key = ?", "RCP-DUP-1").Count(&nc)
	if nc != 1 {
		t.Fatalf("结论通知只能有 1 条, got %d", nc)
	}

	// 并发重复送达：唯一索引 + 行事务，仍然只生效一次
	// （用新 key 在另一处罚 p2 上做并发；p2 需要先提交）
	h.uploadDraft("zhang", "/api/stores/1/penalties/2/draft", true,
		map[string]string{"slot": "packed", "_filename": "p.png"})
	// p2 在 TestRoleBoundary 之外是独立 DB，此处全新种子：直接提交
	if w := h.do("POST", "/api/stores/1/penalties/2/submit", "wang", nil); w.Code != 200 {
		t.Fatalf("p2 submit: %s", w.Body.String())
	}
	var wg sync.WaitGroup
	var codes []int
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ww := h.do("POST", "/api/stores/1/penalties/2/receipts", "wang",
				gin.H{"receipt_key": "RCP-RACE-1", "kind": "result", "decision": "adjusted", "adjusted_cents": 600})
			mu.Lock()
			codes = append(codes, ww.Code)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, c := range codes {
		if c != 200 {
			t.Fatalf("并发回执均应 200（幂等）, got %v", codes)
		}
	}
	h.db.Model(&models.PlatformReceipt{}).Where("receipt_key = ?", "RCP-RACE-1").Count(&rc)
	if rc != 1 {
		t.Fatalf("并发后回执仍只能 1 条, got %d", rc)
	}
	h.db.Model(&models.AppealAction{}).Where("penalty_id = ? AND kind = ?", 2, "result").Count(&ac)
	if ac != 1 {
		t.Fatalf("并发后 result 动作仍只能 1 条, got %d", ac)
	}
	var pen models.Penalty
	h.db.First(&pen, 2)
	if pen.Status != "closed" || pen.CurrentDecision != "adjusted" || pen.AdjustedCents != 600 {
		t.Fatalf("并发后结论异常: %+v", pen)
	}
}

// ---------- 7. 平台更正保留原版本 ----------

func TestReceiptCorrection(t *testing.T) {
	h := newHarness(t)
	// p4 种子里已是 submitted
	w := h.do("POST", "/api/stores/1/penalties/4/receipts", "wang",
		gin.H{"receipt_key": "RCP-COR-1", "kind": "result", "decision": "upheld", "message": "初判维持"})
	if w.Code != 200 {
		t.Fatalf("首次结论: %s", w.Body.String())
	}
	// 未结案的处罚不能收更正（用 p1）
	w = h.do("POST", "/api/stores/1/penalties/1/receipts", "wang",
		gin.H{"receipt_key": "RCP-COR-X", "kind": "correction", "decision": "revoked"})
	if w.Code != 409 {
		t.Fatalf("未结案收更正 want 409, got %d", w.Code)
	}
	// 平台自查后更正：撤销
	w = h.do("POST", "/api/stores/1/penalties/4/receipts", "wang",
		gin.H{"receipt_key": "RCP-COR-2", "kind": "correction", "decision": "revoked", "message": "复核后撤销"})
	if w.Code != 200 {
		t.Fatalf("更正: %s", w.Body.String())
	}
	var rs []models.PlatformReceipt
	h.db.Where("penalty_id = ?", 4).Order("id ASC").Find(&rs)
	if len(rs) != 2 {
		t.Fatalf("两条回执都应保留, got %d", len(rs))
	}
	if rs[0].Decision != "upheld" || rs[1].Decision != "revoked" {
		t.Fatalf("回执历史被改写: %+v", rs)
	}
	if rs[1].SupersedesID == nil || *rs[1].SupersedesID != rs[0].ID {
		t.Fatalf("更正回执应指向被更正的原回执")
	}
	var acts []models.AppealAction
	h.db.Where("penalty_id = ?", 4).Order("id ASC").Find(&acts)
	kinds := []string{}
	for _, a := range acts {
		kinds = append(kinds, a.Kind)
	}
	want := []string{"submit", "result", "correction"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("动作链应为 submit/result/correction, got %v", kinds)
	}
	var p models.Penalty
	h.db.First(&p, 4)
	if p.CurrentDecision != "revoked" || p.Status != "closed" {
		t.Fatalf("当前结论应为 revoked, got %+v", p)
	}

	// 已结案后不能再来一条「首次结果」
	w = h.do("POST", "/api/stores/1/penalties/4/receipts", "wang",
		gin.H{"receipt_key": "RCP-COR-3", "kind": "result", "decision": "upheld"})
	if w.Code != 409 {
		t.Fatalf("结案后再来 result want 409, got %d", w.Code)
	}
}

// ---------- 8. 同一订单多条处罚，期限各自独立 ----------

func TestSiblingPenaltiesIndependentDeadline(t *testing.T) {
	h := newHarness(t)
	// 种子 p1（deadline +23h）与 p2（deadline +2h）属于同一订单 1
	var p1, p2 models.Penalty
	h.db.First(&p1, 1)
	h.db.First(&p2, 2)
	if p1.OrderID != p2.OrderID {
		t.Fatalf("种子前提：p1/p2 同订单")
	}
	if p1.DeadlineAt.Equal(p2.DeadlineAt) {
		t.Fatalf("两条处罚期限必须独立计算，却相等")
	}
	w := h.do("GET", "/api/stores/1/penalties/1", "wang", nil)
	m := jget(t, w)
	sibs, _ := m["sibling_penalties"].([]any)
	if len(sibs) != 1 {
		t.Fatalf("p1 详情应展示同订单另一条处罚, got %v", sibs)
	}
	// 提交 p2 不影响 p1 状态
	h.uploadDraft("zhang", "/api/stores/1/penalties/2/draft", true,
		map[string]string{"slot": "packed", "_filename": "p.png"})
	if w := h.do("POST", "/api/stores/1/penalties/2/submit", "wang", nil); w.Code != 200 {
		t.Fatalf("p2 提交: %s", w.Body.String())
	}
	h.db.First(&p1, 1)
	h.db.First(&p2, 2)
	if p1.Status != "pending" || p2.Status != "submitted" {
		t.Fatalf("p1 应仍 pending, p2 submitted; got %s/%s", p1.Status, p2.Status)
	}
}

// ---------- 9. 撤回后可重申，原提交版本保留 ----------

func TestWithdrawAndResubmit(t *testing.T) {
	h := newHarness(t)
	h.uploadDraft("zhang", "/api/stores/1/penalties/2/draft", true,
		map[string]string{"slot": "packed", "_filename": "p.png"})
	if w := h.do("POST", "/api/stores/1/penalties/2/submit", "wang", nil); w.Code != 200 {
		t.Fatalf("submit: %s", w.Body.String())
	}
	if w := h.do("POST", "/api/stores/1/penalties/2/withdraw", "wang", gin.H{"note": "先撤回"}); w.Code != 200 {
		t.Fatalf("withdraw: %s", w.Body.String())
	}
	// 待申诉状态不能撤回
	if w := h.do("POST", "/api/stores/1/penalties/1/withdraw", "wang", nil); w.Code != 409 {
		t.Fatalf("未提交撤回 want 409, got %d", w.Code)
	}
	// 重申成功，动作链 3 环，历史证据不复制不删除
	if w := h.do("POST", "/api/stores/1/penalties/2/submit", "wang", nil); w.Code != 200 {
		t.Fatalf("resubmit: %s", w.Body.String())
	}
	var acts []models.AppealAction
	h.db.Where("penalty_id = ?", 2).Order("id ASC").Find(&acts)
	if len(acts) != 3 || acts[0].Kind != "submit" || acts[1].Kind != "withdraw" || acts[2].Kind != "submit" {
		t.Fatalf("动作链应为 submit/withdraw/submit, got %+v", acts)
	}
	var cnt int64
	h.db.Model(&models.EvidenceVersion{}).Where("penalty_id = ?", 2).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("重申不应产生重复证据, got %d", cnt)
	}
}
