package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

var testBase = time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)

func setupTest(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Seed(db, testBase, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s := NewServer(db, t.TempDir(), t.TempDir())
	s.SimulatePlatform = false // 测试中关闭自动模拟平台，避免异步干扰
	s.Now = func() time.Time { return testBase }
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func login(t *testing.T, ts *httptest.Server, username, password string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := http.Post(ts.URL+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login %s: status %d", username, resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Token
}

func doReq(t *testing.T, method, url, token, platformTok string, body io.Reader, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if platformTok != "" {
		req.Header.Set("X-Platform-Token", platformTok)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func getJSON(t *testing.T, ts *httptest.Server, token, path string) (int, map[string]interface{}) {
	t.Helper()
	resp := doReq(t, "GET", ts.URL+path, token, "", nil, "")
	defer resp.Body.Close()
	var out map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func uploadEvidence(t *testing.T, ts *httptest.Server, token string, penaltyID uint, category, filename, content string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("file", filename)
	fw.Write([]byte(content))
	w.WriteField("category", category)
	w.WriteField("note", "测试证据")
	w.Close()
	return doReq(t, "POST", fmt.Sprintf("%s/api/penalties/%d/evidence", ts.URL, penaltyID),
		token, "", &buf, w.FormDataContentType())
}

func postEmpty(t *testing.T, ts *httptest.Server, token, path string) *http.Response {
	t.Helper()
	return doReq(t, "POST", ts.URL+path, token, "", nil, "")
}

func readErr(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&out)
	if e, ok := out["error"].(string); ok {
		return e
	}
	return ""
}

// penaltyByOrder 按订单号+类型查处罚
func penaltyByOrder(t *testing.T, s *Server, orderNo, ptype string) Penalty {
	t.Helper()
	var o Order
	if err := s.DB.Where("order_no = ?", orderNo).First(&o).Error; err != nil {
		t.Fatal(err)
	}
	var p Penalty
	if err := s.DB.Where("order_id = ? AND type = ?", o.ID, ptype).First(&p).Error; err != nil {
		t.Fatal(err)
	}
	return p
}

// ---------- 1. 跨门店数据隔离 ----------

func TestCrossStoreIsolation(t *testing.T) {
	s, ts := setupTest(t)
	m2 := login(t, ts, "manager2", "123456")

	// 门店1的处罚，门店2访问 → 404
	p1 := penaltyByOrder(t, s, "MT20261002007", PenaltyTimeout)
	code, _ := getJSON(t, ts, m2, fmt.Sprintf("/api/penalties/%d", p1.ID))
	if code != http.StatusNotFound {
		t.Fatalf("跨门店访问处罚应 404，实际 %d", code)
	}

	// 门店2的列表不应包含门店1的处罚
	code, out := getJSON(t, ts, m2, "/api/penalties")
	if code != 200 {
		t.Fatalf("列表状态码 %d", code)
	}
	for _, it := range out["penalties"].([]interface{}) {
		m := it.(map[string]interface{})
		if m["order_no"] == "MT20261002007" {
			t.Fatal("门店2的处罚列表泄露了门店1的数据")
		}
	}

	// 门店1的证据文件，门店2下载 → 404
	staff1 := login(t, ts, "staff1", "123456")
	resp := uploadEvidence(t, ts, staff1, p1.ID, CatPacked, "pack.jpg", "img")
	resp.Body.Close()
	var item EvidenceItem
	s.DB.Where("penalty_id = ?", p1.ID).First(&item)
	resp = doReq(t, "GET", fmt.Sprintf("%s/api/files/%d", ts.URL, item.ID), m2, "", nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("跨门店下载文件应 404，实际 %d", resp.StatusCode)
	}

	// 门店1的申诉，门店2访问 → 404
	m1 := login(t, ts, "manager1", "123456")
	resp = postEmpty(t, ts, m1, fmt.Sprintf("/api/penalties/%d/submit", p1.ID))
	var submitOut map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&submitOut)
	resp.Body.Close()
	appealID := uint(submitOut["appeal_id"].(float64))
	code, _ = getJSON(t, ts, m2, fmt.Sprintf("/api/appeals/%d", appealID))
	if code != http.StatusNotFound {
		t.Fatalf("跨门店访问申诉应 404，实际 %d", code)
	}
}

// ---------- 2. 角色权限：员工整理证据，店长正式提交 ----------

func TestStaffCanPrepareButManagerSubmits(t *testing.T) {
	s, ts := setupTest(t)
	p1 := penaltyByOrder(t, s, "MT20261002007", PenaltyTimeout)
	staff := login(t, ts, "staff1", "123456")
	manager := login(t, ts, "manager1", "123456")

	// 员工可以上传证据
	resp := uploadEvidence(t, ts, staff, p1.ID, CatPacked, "packed.jpg", "img")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("员工上传证据应 201，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 员工提交 → 403
	resp = postEmpty(t, ts, staff, fmt.Sprintf("/api/penalties/%d/submit", p1.ID))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("员工提交应 403，实际 %d", resp.StatusCode)
	}
	if msg := readErr(t, resp); msg == "" {
		t.Fatal("员工提交被拒时应给出提示")
	}

	// 店长提交 → 200
	resp = postEmpty(t, ts, manager, fmt.Sprintf("/api/penalties/%d/submit", p1.ID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("店长提交应 200，实际 %d: %s", resp.StatusCode, readErr(t, resp))
	}
	resp.Body.Close()
}

// ---------- 3. 申诉期限边界 ----------

func TestDeadlineBoundary(t *testing.T) {
	s, ts := setupTest(t)
	manager := login(t, ts, "manager1", "123456")

	mkPenalty := func(deadline time.Time) Penalty {
		var o Order
		s.DB.Where("order_no = ?", "MT20261001001").First(&o)
		p := Penalty{
			StoreID: 1, OrderID: o.ID, Type: PenaltyTimeout, Amount: 10,
			Reason: "边界测试", IssuedAt: deadline.Add(-AppealWindow),
			Deadline: deadline, Status: PenaltyPending,
		}
		s.DB.Create(&p)
		s.DB.Create(&EvidenceItem{
			PenaltyID: p.ID, Category: CatMealReady, FileName: "a.jpg",
			UploadedByID: 1, UploaderName: "王店长", CreatedAt: testBase,
		})
		return p
	}

	// 恰好在截止时刻 → 允许提交（边界含端点）
	pAt := mkPenalty(testBase)
	resp := postEmpty(t, ts, manager, fmt.Sprintf("/api/penalties/%d/submit", pAt.ID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("截止时刻提交应成功，实际 %d: %s", resp.StatusCode, readErr(t, resp))
	}
	resp.Body.Close()

	// 超过截止 1 秒 → 拒绝并明确提示
	pAfter := mkPenalty(testBase.Add(-time.Second))
	resp = postEmpty(t, ts, manager, fmt.Sprintf("/api/penalties/%d/submit", pAfter.ID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("超期提交应 400，实际 %d", resp.StatusCode)
	}
	if msg := readErr(t, resp); !bytes.Contains([]byte(msg), []byte("已超过申诉期限")) {
		t.Fatalf("超期提示不明确: %q", msg)
	}

	// 详情页状态应展示为 expired
	_, out := getJSON(t, ts, manager, fmt.Sprintf("/api/penalties/%d", pAfter.ID))
	if out["penalty"].(map[string]interface{})["status"] != "expired" {
		t.Fatal("超期处罚状态应为 expired")
	}
}

// ---------- 4. 相同平台回执重复送达：幂等，不产生冲突结果 ----------

func TestDuplicateReceiptIdempotent(t *testing.T) {
	s, ts := setupTest(t)
	manager := login(t, ts, "manager1", "123456")
	p1 := penaltyByOrder(t, s, "MT20261002007", PenaltyTimeout)

	resp := uploadEvidence(t, ts, manager, p1.ID, CatPacked, "p.jpg", "img")
	resp.Body.Close()
	resp = postEmpty(t, ts, manager, fmt.Sprintf("/api/penalties/%d/submit", p1.ID))
	var submitOut map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&submitOut)
	resp.Body.Close()
	appealID := uint(submitOut["appeal_id"].(float64))

	receipt := map[string]interface{}{
		"receipt_id": "RCPT-TEST-1", "appeal_id": appealID,
		"outcome": "revoked", "reason": "证据充分，撤销处罚",
	}
	postReceipt := func(payload map[string]interface{}) *http.Response {
		body, _ := json.Marshal(payload)
		return doReq(t, "POST", ts.URL+"/api/platform/receipts", "", platformToken,
			bytes.NewReader(body), "application/json")
	}

	// 首次送达 → 201
	resp = postReceipt(receipt)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("首次回执应 201，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 相同回执重复送达 → 200 duplicate，不产生第二个结论
	resp = postReceipt(receipt)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("重复回折应 200，实际 %d", resp.StatusCode)
	}
	var dupOut map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&dupOut)
	resp.Body.Close()
	if dupOut["duplicate"] != true {
		t.Fatal("重复回执应标记 duplicate=true")
	}
	var cnt int64
	s.DB.Model(&AppealResult{}).Where("appeal_id = ?", appealID).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("重复回执产生了 %d 条结论", cnt)
	}

	// 相同 receipt_id 但内容冲突 → 409，原结论保留
	conflict := map[string]interface{}{
		"receipt_id": "RCPT-TEST-1", "appeal_id": appealID,
		"outcome": "upheld", "reason": "篡改的冲突回执",
	}
	resp = postReceipt(conflict)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("冲突回执应 409，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()
	var r AppealResult
	s.DB.Where("appeal_id = ?", appealID).First(&r)
	if r.Outcome != OutcomeRevoked {
		t.Fatalf("冲突回执覆盖了原结论: %s", r.Outcome)
	}
	s.DB.Model(&AppealResult{}).Where("appeal_id = ?", appealID).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("冲突回执后结论数应为 1，实际 %d", cnt)
	}

	// dev 重新送达端点同样幂等
	resp = postEmpty(t, ts, manager, fmt.Sprintf("/api/dev/appeals/%d/redeliver-receipt", appealID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("重新送达应 200，实际 %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&dupOut)
	resp.Body.Close()
	if dupOut["duplicate"] != true {
		t.Fatal("重新送达应被识别为重复回执")
	}
	s.DB.Model(&AppealResult{}).Where("appeal_id = ?", appealID).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("重新送达后结论数应为 1，实际 %d", cnt)
	}
}

// ---------- 5. 证据不可替换：补件/撤回/更正均生成新版本，原版本保留 ----------

func TestEvidenceVersionsPreserved(t *testing.T) {
	s, ts := setupTest(t)
	manager := login(t, ts, "manager1", "123456")
	p1 := penaltyByOrder(t, s, "MT20261002007", PenaltyTimeout)

	resp := uploadEvidence(t, ts, manager, p1.ID, CatPacked, "v1.jpg", "v1-content")
	resp.Body.Close()
	resp = postEmpty(t, ts, manager, fmt.Sprintf("/api/penalties/%d/submit", p1.ID))
	var submitOut map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&submitOut)
	resp.Body.Close()
	appealID := uint(submitOut["appeal_id"].(float64))

	// 提交后不能直接替换证据：草稿上传被拒
	resp = uploadEvidence(t, ts, manager, p1.ID, CatPacked, "replace.jpg", "replaced")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("提交后应禁止直接替换证据，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()
	// 也不存在更新/删除证据的路由
	resp = doReq(t, "PUT", fmt.Sprintf("%s/api/penalties/%d/evidence/1", ts.URL, p1.ID), manager, "", nil, "")
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("不应存在证据更新接口，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 补件 → v2
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("file", "v2.jpg")
	fw.Write([]byte("v2-content"))
	w.WriteField("category", CatRiderPickup)
	w.WriteField("note", "补充取餐监控截图")
	w.Close()
	resp = doReq(t, "POST", fmt.Sprintf("%s/api/appeals/%d/supplement", ts.URL, appealID),
		manager, "", &buf, w.FormDataContentType())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("补件应 200，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 撤回 → v3
	resp = postEmpty(t, ts, manager, fmt.Sprintf("/api/appeals/%d/withdraw", appealID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("撤回应 200，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 平台更正 → v4
	body, _ := json.Marshal(map[string]interface{}{"appeal_id": appealID, "note": "平台更正材料归档信息"})
	resp = doReq(t, "POST", ts.URL+"/api/platform/corrections", "", platformToken,
		bytes.NewReader(body), "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("平台更正应 200，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 四个版本全部保留，v1 内容未被后续操作改变
	_, out := getJSON(t, ts, manager, fmt.Sprintf("/api/appeals/%d", appealID))
	versions := out["appeal"].(map[string]interface{})["versions"].([]interface{})
	if len(versions) != 4 {
		t.Fatalf("应保留 4 个版本，实际 %d", len(versions))
	}
	wantTypes := []string{VersionSubmission, VersionSupplement, VersionWithdrawal, VersionCorrection}
	for i, wt := range wantTypes {
		v := versions[i].(map[string]interface{})
		if v["type"] != wt {
			t.Fatalf("版本 %d 类型应为 %s，实际 %s", i+1, wt, v["type"])
		}
	}
	v1 := versions[0].(map[string]interface{})
	v1Items := v1["items"].([]interface{})
	if len(v1Items) != 1 || v1Items[0].(map[string]interface{})["file_name"] != "v1.jpg" {
		t.Fatal("v1 版本的原始材料被改变")
	}
	// v2 快照应包含 v1 + 补件（累积快照）
	v2Items := versions[1].(map[string]interface{})["items"].([]interface{})
	if len(v2Items) != 2 {
		t.Fatalf("v2 应包含 2 份材料，实际 %d", len(v2Items))
	}
}

// ---------- 6. 结论关联提交材料版本 ----------

func TestResultLinkedToSubmittedVersion(t *testing.T) {
	s, ts := setupTest(t)
	manager := login(t, ts, "manager1", "123456")
	p1 := penaltyByOrder(t, s, "MT20261002007", PenaltyTimeout)

	resp := uploadEvidence(t, ts, manager, p1.ID, CatPacked, "a.jpg", "x")
	resp.Body.Close()
	resp = postEmpty(t, ts, manager, fmt.Sprintf("/api/penalties/%d/submit", p1.ID))
	var submitOut map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&submitOut)
	resp.Body.Close()
	appealID := uint(submitOut["appeal_id"].(float64))

	adj := 10.0
	body, _ := json.Marshal(map[string]interface{}{
		"receipt_id": "RCPT-LINK-1", "appeal_id": appealID,
		"outcome": "partial", "adjusted_amount": adj, "reason": "部分成立，减半处罚",
	})
	resp = doReq(t, "POST", ts.URL+"/api/platform/receipts", "", platformToken,
		bytes.NewReader(body), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("回执应 201，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()

	var r AppealResult
	s.DB.Where("appeal_id = ?", appealID).First(&r)
	if r.VersionNo != 1 {
		t.Fatalf("结论应关联提交版本 v1，实际 v%d", r.VersionNo)
	}
	if r.AdjustedAmount == nil || *r.AdjustedAmount != 10 {
		t.Fatal("部分调整金额未记录")
	}
	// 处罚状态同步为 partial
	var p Penalty
	s.DB.First(&p, p1.ID)
	if p.Status != PenaltyPartial {
		t.Fatalf("处罚状态应为 partial，实际 %s", p.Status)
	}
	// 详情页可见结论
	_, out := getJSON(t, ts, manager, fmt.Sprintf("/api/penalties/%d", p1.ID))
	appeal := out["appeal"].(map[string]interface{})
	result := appeal["result"].(map[string]interface{})
	if result["receipt_id"] != "RCPT-LINK-1" || result["outcome"] != "partial" {
		t.Fatal("详情页未展示平台结论")
	}
}

// ---------- 7. 缺失证据提示 ----------

func TestMissingEvidenceHints(t *testing.T) {
	s, ts := setupTest(t)
	manager := login(t, ts, "manager1", "123456")
	// o2 时间线缺 packed / rider_pickup
	p1 := penaltyByOrder(t, s, "MT20261002007", PenaltyTimeout)
	_, out := getJSON(t, ts, manager, fmt.Sprintf("/api/penalties/%d", p1.ID))
	missing := out["missing"].([]interface{})
	if len(missing) != 2 {
		t.Fatalf("应提示 2 项缺失证据，实际 %v", missing)
	}
	got := fmt.Sprintf("%v,%v", missing[0], missing[1])
	if got != "打包,骑手取餐" {
		t.Fatalf("缺失提示不正确: %s", got)
	}
}
