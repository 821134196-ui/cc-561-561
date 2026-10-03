package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"appeal/internal/appeal"
	"appeal/internal/models"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type Handler struct {
	DB   *gorm.DB
	Svc  *appeal.Service
	Auth *Auth
}

func New(db *gorm.DB, svc *appeal.Service) *Handler {
	return &Handler{DB: db, Svc: svc, Auth: NewAuth()}
}

func (h *Handler) fail(c *gin.Context, err error) {
	if ae, ok := appeal.IsAPIError(err); ok {
		c.AbortWithStatusJSON(ae.Status, gin.H{"error": ae.Code, "message": ae.Msg})
		return
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal", "message": err.Error()})
}

// requirePathStore 路径中的门店必须是当前会话所属门店，否则一律 404。
func requirePathStore(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("storeId"), 10, 64)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "store_not_found", "message": "门店不存在"})
		return 0, false
	}
	sess := currentSession(c)
	if uint(id) != sess.StoreID {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "store_not_found", "message": "门店不存在或无权访问"})
		return 0, false
	}
	return uint(id), true
}

func pathUint(c *gin.Context, name string) (uint, bool) {
	id, err := strconv.ParseUint(c.Param(name), 10, 64)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "资源不存在"})
		return 0, false
	}
	return uint(id), true
}

// ---------------- 登录 / 当前用户 ----------------

func (h *Handler) Login(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad_request", "message": "请求格式错误"})
		return
	}
	var u models.User
	err := h.DB.Preload("Store").Where("username = ?", req.Username).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		c.JSON(401, gin.H{"error": "bad_credentials", "message": "用户名或密码错误"})
		return
	}
	if err != nil {
		h.fail(c, err)
		return
	}
	tok := h.Auth.Issue(Session{UserID: u.ID, StoreID: u.StoreID, Role: u.Role, Name: u.DisplayName})
	c.JSON(200, gin.H{
		"token": tok,
		"user": gin.H{
			"id": u.ID, "username": u.Username, "display_name": u.DisplayName,
			"role": u.Role, "store_id": u.StoreID,
			"store_name": u.Store.Name,
		},
	})
}

func (h *Handler) Logout(c *gin.Context) {
	if t, ok := c.Get("token"); ok {
		h.Auth.Revoke(t.(string))
	}
	c.JSON(200, gin.H{"ok": true})
}

func (h *Handler) Me(c *gin.Context) {
	sess := currentSession(c)
	var u models.User
	if err := h.DB.Preload("Store").First(&u, sess.UserID).Error; err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(200, gin.H{
		"id": u.ID, "username": u.Username, "display_name": u.DisplayName,
		"role": u.Role, "store_id": u.StoreID, "store_name": u.Store.Name,
	})
}

// ---------------- 处罚列表 / 详情 ----------------

func penaltySummary(p *models.Penalty, now time.Time) gin.H {
	overdue := appeal.IsOverdue(p, now)
	status := p.Status
	if overdue {
		status = models.StatusExpired // 实时展示超期（落库状态由 sweep/提交时纠正）
	}
	return gin.H{
		"id": p.ID, "store_id": p.StoreID, "order_id": p.OrderID,
		"platform_ref": p.PlatformRef, "type": p.Type, "type_label": models.PenaltyTypeLabel(p.Type),
		"amount_cents": p.AmountCents, "reason": p.Reason,
		"issued_at": p.IssuedAt, "deadline_at": p.DeadlineAt,
		"status": status, "status_label": models.StatusLabel(status),
		"current_decision": p.CurrentDecision,
		"decision_label":   models.DecisionLabel(p.CurrentDecision),
		"submitted_at":     p.SubmittedAt, "finalized_at": p.FinalizedAt,
		"overdue":           overdue,
		"seconds_remaining": int64(p.DeadlineAt.Sub(now).Seconds()),
	}
}

func (h *Handler) ListPenalties(c *gin.Context) {
	storeID, ok := requirePathStore(c)
	if !ok {
		return
	}
	q := h.DB.Preload("Order").Where("store_id = ?", storeID)
	if st := c.Query("status"); st != "" {
		if st == models.StatusExpired {
			// 超期包含「已落库 expired」与「实时已过 deadline 的 pending/withdrawn」
			q = q.Where("status = ? OR (status IN ? AND deadline_at < ?)",
				models.StatusExpired, []string{models.StatusPending, models.StatusWithdrawn}, time.Now())
		} else {
			q = q.Where("status = ?", st)
		}
	}
	var ps []models.Penalty
	if err := q.Order("deadline_at ASC").Find(&ps).Error; err != nil {
		h.fail(c, err)
		return
	}
	now := time.Now()
	out := make([]gin.H, 0, len(ps))
	for i := range ps {
		row := penaltySummary(&ps[i], now)
		row["order_no"] = ps[i].Order.PlatformOrderNo
		out = append(out, row)
	}
	c.JSON(200, gin.H{"items": out})
}

func evidenceDTO(v *models.EvidenceVersion) gin.H {
	return gin.H{
		"id": v.ID, "penalty_id": v.PenaltyID, "slot": v.Slot,
		"slot_label": models.SlotLabel(v.Slot), "kind": v.Kind,
		"original_name": v.OriginalName, "mime_type": v.MimeType,
		"size_bytes": v.SizeBytes, "note": v.Note,
		"uploader_name": v.UploaderName, "archived": v.AppealActionID != nil,
		"supersedes_id": v.SupersedesID, "created_at": v.CreatedAt,
		"download_url": fmt.Sprintf("/api/evidence/%d/download", v.ID),
	}
}

func (h *Handler) GetPenalty(c *gin.Context) {
	storeID, ok := requirePathStore(c)
	if !ok {
		return
	}
	pid, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var p models.Penalty
	err := h.DB.Preload("Order").Where("id = ? AND store_id = ?", pid, storeID).First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(404, gin.H{"error": "penalty_not_found", "message": "处罚记录不存在或不属于当前门店"})
		return
	}
	if err != nil {
		h.fail(c, err)
		return
	}

	var events []models.TimelineEvent
	h.DB.Where("order_id = ?", p.OrderID).Order("sort_order ASC, occurred_at ASC").Find(&events)
	var versions []models.EvidenceVersion
	h.DB.Where("penalty_id = ?", p.ID).Order("slot ASC, id ASC").Find(&versions)
	var actions []models.AppealAction
	h.DB.Where("penalty_id = ?", p.ID).Order("id ASC").Find(&actions)
	var receipts []models.PlatformReceipt
	h.DB.Where("penalty_id = ?", p.ID).Order("id ASC").Find(&receipts)

	missing, _ := appeal.MissingSlots(h.DB, p.ID)
	now := time.Now()

	verByID := map[uint]gin.H{}
	evList := make([]gin.H, 0, len(versions))
	for i := range versions {
		dto := evidenceDTO(&versions[i])
		verByID[versions[i].ID] = dto
		evList = append(evList, dto)
	}

	actionList := make([]gin.H, 0, len(actions))
	kindLabel := map[string]string{
		models.ActionSubmit: "提交申诉", models.ActionSupplement: "补充材料",
		models.ActionWithdraw: "撤回申诉", models.ActionResult: "平台结论",
		models.ActionCorrection: "平台更正",
	}
	for _, a := range actions {
		snaps := make([]gin.H, 0, len(a.SnapshotIDs))
		for _, vid := range a.SnapshotIDs {
			if dto, ok := verByID[vid]; ok {
				snaps = append(snaps, dto)
			}
		}
		actionList = append(actionList, gin.H{
			"id": a.ID, "kind": a.Kind, "kind_label": kindLabel[a.Kind],
			"actor_name": a.ActorName, "note": a.Note,
			"snapshots": snaps, "receipt_id": a.ReceiptID,
			"created_at": a.CreatedAt,
		})
	}

	receiptList := make([]gin.H, 0, len(receipts))
	for _, r := range receipts {
		receiptList = append(receiptList, gin.H{
			"id": r.ID, "receipt_key": r.ReceiptKey, "kind": r.Kind,
			"decision": r.Decision, "decision_label": models.DecisionLabel(r.Decision),
			"adjusted_cents": r.AdjustedCents, "message": r.Message,
			"delivery_count": r.DeliveryCount, "received_at": r.ReceivedAt,
			"last_seen_at": r.LastSeenAt, "supersedes_receipt_id": r.SupersedesID,
		})
	}

	// 同一订单上的其他处罚（各自独立期限）。
	var siblings []models.Penalty
	h.DB.Where("order_id = ? AND id <> ?", p.OrderID, p.ID).Order("id ASC").Find(&siblings)
	sib := make([]gin.H, 0, len(siblings))
	for i := range siblings {
		sib = append(sib, penaltySummary(&siblings[i], now))
	}

	timeline := make([]gin.H, 0, len(events))
	kindIcon := map[string]string{
		"created": "📝", "accepted": "✅", "meal_ready": "🍳", "packed": "📦",
		"rider_pickup": "🛵", "delivered": "🏠", "rejected_mark": "⛔",
	}
	for _, e := range events {
		timeline = append(timeline, gin.H{
			"kind": e.Kind, "icon": kindIcon[e.Kind], "label": e.Label,
			"occurred_at": e.OccurredAt, "note": e.Note,
		})
	}

	missingLabels := make([]gin.H, 0, len(missing))
	for _, slot := range missing {
		missingLabels = append(missingLabels, gin.H{"slot": slot, "label": models.SlotLabel(slot)})
	}

	detail := penaltySummary(&p, now)
	detail["order"] = gin.H{
		"id": p.Order.ID, "platform_order_no": p.Order.PlatformOrderNo,
		"customer_note": p.Order.CustomerNote, "status": p.Order.Status,
		"created_at": p.Order.CreatedAt,
	}
	detail["timeline"] = timeline
	detail["evidence"] = evList
	detail["actions"] = actionList
	detail["receipts"] = receiptList
	detail["sibling_penalties"] = sib
	detail["required_slots"] = models.RequiredSlots(p.Type)
	detail["missing_slots"] = missingLabels
	detail["within_deadline"] = !now.After(p.DeadlineAt)
	detail["adjusted_cents"] = p.AdjustedCents
	c.JSON(200, detail)
}

// ---------------- 证据：草稿 / 补件 / 删除 / 下载 ----------------

func (h *Handler) UploadDraft(c *gin.Context) {
	storeID, ok := requirePathStore(c)
	if !ok {
		return
	}
	pid, ok := pathUint(c, "id")
	if !ok {
		return
	}
	sess := currentSession(c)
	slot := c.PostForm("slot")
	note := c.PostForm("note")
	fh, _ := c.FormFile("file")
	v, err := h.Svc.AddDraftEvidence(pid, storeID, sess.UserID, sess.Name, slot, "", note, fh)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(201, evidenceDTO(v))
}

func (h *Handler) Supplement(c *gin.Context) {
	if !requireManager(c) {
		return
	}
	storeID, ok := requirePathStore(c)
	if !ok {
		return
	}
	pid, ok := pathUint(c, "id")
	if !ok {
		return
	}
	sess := currentSession(c)
	slot := c.PostForm("slot")
	note := c.PostForm("note")
	fh, _ := c.FormFile("file")
	v, err := h.Svc.AddSupplement(pid, storeID, sess.UserID, sess.Name, slot, "", note, fh)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(201, evidenceDTO(v))
}

func (h *Handler) DeleteEvidence(c *gin.Context) {
	storeID, ok := requirePathStore(c)
	if !ok {
		return
	}
	vid, ok := pathUint(c, "vid")
	if !ok {
		return
	}
	if err := h.Svc.DeleteDraft(vid, storeID); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (h *Handler) Download(c *gin.Context) {
	vid, err := strconv.ParseUint(c.Param("vid"), 10, 64)
	if err != nil {
		c.JSON(404, gin.H{"error": "not_found"})
		return
	}
	var v models.EvidenceVersion
	if err := h.DB.First(&v, vid).Error; err != nil {
		c.JSON(404, gin.H{"error": "evidence_not_found", "message": "证据不存在"})
		return
	}
	// 跨店隔离：附件下载同样校验门店归属。
	sess := currentSession(c)
	if v.StoreID != sess.StoreID {
		c.JSON(404, gin.H{"error": "evidence_not_found", "message": "证据不存在或不属于当前门店"})
		return
	}
	if v.Kind == "note" || v.StoredPath == "" {
		c.JSON(200, gin.H{"note": v.Note})
		return
	}
	if _, err := os.Stat(v.StoredPath); err != nil {
		c.JSON(410, gin.H{"error": "file_gone", "message": "原始文件已不可用（元数据与版本记录保留）"})
		return
	}
	// 防路径穿越：StoredPath 由服务端生成，仍以 Base 字段兜底。
	c.FileAttachment(filepath.Clean(v.StoredPath), v.OriginalName)
}

// ---------------- 提交 / 撤回 ----------------

type noteReq struct {
	Note string `json:"note"`
}

func (h *Handler) Submit(c *gin.Context) {
	if !requireManager(c) {
		return
	}
	storeID, ok := requirePathStore(c)
	if !ok {
		return
	}
	pid, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var req noteReq
	_ = c.ShouldBindJSON(&req)
	sess := currentSession(c)
	p, _, err := h.Svc.Submit(pid, storeID, sess.UserID, sess.Name, req.Note)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true, "status": p.Status})
}

func (h *Handler) Withdraw(c *gin.Context) {
	if !requireManager(c) {
		return
	}
	storeID, ok := requirePathStore(c)
	if !ok {
		return
	}
	pid, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var req noteReq
	_ = c.ShouldBindJSON(&req)
	sess := currentSession(c)
	p, err := h.Svc.Withdraw(pid, storeID, sess.UserID, sess.Name, req.Note)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true, "status": p.Status})
}

// ---------------- 模拟平台回执 ----------------

func (h *Handler) PushReceipt(c *gin.Context) {
	if !requireManager(c) {
		return
	}
	storeID, ok := requirePathStore(c)
	if !ok {
		return
	}
	pid, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var in appeal.ReceiptInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, gin.H{"error": "bad_request", "message": "请求格式错误"})
		return
	}
	in.PenaltyID = pid
	if in.RawPayload == "" {
		in.RawPayload = fmt.Sprintf("{simulated at %s}", time.Now().Format(time.RFC3339))
	}
	out, err := h.Svc.ReceiveReceipt(storeID, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(200, gin.H{
		"ok": true, "duplicated": out.Duplicated,
		"receipt_key":    out.Receipt.ReceiptKey,
		"delivery_count": out.Receipt.DeliveryCount,
		"status":         out.Penalty.Status,
		"decision":       out.Penalty.CurrentDecision,
		"message": func() string {
			if out.Duplicated {
				return "同一平台回执重复送达：已忽略，仅累加送达次数，结论未发生变化"
			}
			return "回执已处理"
		}(),
	})
}

func (h *Handler) ListReceipts(c *gin.Context) {
	storeID, ok := requirePathStore(c)
	if !ok {
		return
	}
	pid, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var n int64
	h.DB.Model(&models.Penalty{}).Where("id = ? AND store_id = ?", pid, storeID).Count(&n)
	if n == 0 {
		c.JSON(404, gin.H{"error": "penalty_not_found", "message": "处罚记录不存在或不属于当前门店"})
		return
	}
	var rs []models.PlatformReceipt
	h.DB.Where("penalty_id = ?", pid).Order("id ASC").Find(&rs)
	out := make([]gin.H, 0, len(rs))
	for _, r := range rs {
		out = append(out, gin.H{
			"id": r.ID, "receipt_key": r.ReceiptKey, "kind": r.Kind,
			"decision": r.Decision, "decision_label": models.DecisionLabel(r.Decision),
			"adjusted_cents": r.AdjustedCents, "message": r.Message,
			"delivery_count": r.DeliveryCount, "received_at": r.ReceivedAt,
		})
	}
	c.JSON(200, gin.H{"items": out})
}

// ---------------- 通知 ----------------

func (h *Handler) ListNotifications(c *gin.Context) {
	storeID, ok := requirePathStore(c)
	if !ok {
		return
	}
	var ns []models.Notification
	h.DB.Where("store_id = ?", storeID).Order("id DESC").Limit(100).Find(&ns)
	unread := int64(0)
	h.DB.Model(&models.Notification{}).Where("store_id = ? AND read = ?", storeID, false).Count(&unread)
	c.JSON(200, gin.H{"items": ns, "unread": unread})
}

func (h *Handler) ReadNotifications(c *gin.Context) {
	storeID, ok := requirePathStore(c)
	if !ok {
		return
	}
	if err := h.DB.Model(&models.Notification{}).
		Where("store_id = ? AND read = ?", storeID, false).
		Update("read", true).Error; err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

// ---------------- 首页统计 ----------------

func (h *Handler) Dashboard(c *gin.Context) {
	storeID, ok := requirePathStore(c)
	if !ok {
		return
	}
	now := time.Now()
	var ps []models.Penalty
	h.DB.Where("store_id = ?", storeID).Find(&ps)
	counts := map[string]int{"pending": 0, "submitted": 0, "withdrawn": 0, "expired": 0, "closed": 0}
	expiringSoon := 0
	for i := range ps {
		p := &ps[i]
		switch {
		case appeal.IsOverdue(p, now):
			counts[models.StatusExpired]++
		default:
			counts[p.Status]++
		}
		if (p.Status == models.StatusPending || p.Status == models.StatusWithdrawn) &&
			p.DeadlineAt.Sub(now) <= 24*time.Hour && p.DeadlineAt.After(now) {
			expiringSoon++
		}
	}
	c.JSON(200, gin.H{"counts": counts, "expiring_soon": expiringSoon, "total": len(ps)})
}
