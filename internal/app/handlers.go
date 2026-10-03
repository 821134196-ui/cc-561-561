package app

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const timeFmt = "2006-01-02 15:04:05"

// ---------- 视图辅助 ----------

// penaltyStatus 计算展示状态：pending/withdrawn 且已过截止 → expired（派生，不落库）
func (s *Server) penaltyStatus(p *Penalty) string {
	if (p.Status == PenaltyPending || p.Status == PenaltyWithdrawn) && s.Now().After(p.Deadline) {
		return "expired"
	}
	return p.Status
}

func (s *Server) penaltyView(p *Penalty, orderNo string) gin.H {
	remaining := int64(p.Deadline.Sub(s.Now()).Seconds())
	return gin.H{
		"id": p.ID, "type": p.Type, "amount": p.Amount, "reason": p.Reason,
		"order_id": p.OrderID, "order_no": orderNo,
		"issued_at": p.IssuedAt.Format(timeFmt), "deadline": p.Deadline.Format(timeFmt),
		"status": s.penaltyStatus(p), "remaining_seconds": remaining,
	}
}

// scopedPenalty 取回本门店的处罚；跨门店一律 404，不泄露存在性
func (s *Server) scopedPenalty(c *gin.Context, u *User) (*Penalty, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的处罚ID"})
		return nil, false
	}
	var p Penalty
	if err := s.DB.Where("id = ? AND store_id = ?", id, u.StoreID).First(&p).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "处罚记录不存在"})
		return nil, false
	}
	return &p, true
}

func (s *Server) notify(storeID uint, typ, msg string) {
	s.DB.Create(&Notification{StoreID: storeID, Type: typ, Message: msg, CreatedAt: s.Now()})
}

// ---------- 处罚列表 / 详情 ----------

func (s *Server) listPenalties(c *gin.Context) {
	u := currentUser(c)
	var penalties []Penalty
	q := s.DB.Where("store_id = ?", u.StoreID).Order("deadline asc")
	if st := c.Query("status"); st != "" {
		q = q.Where("status = ?", st)
	}
	q.Find(&penalties)

	out := make([]gin.H, 0, len(penalties))
	for i := range penalties {
		p := &penalties[i]
		var o Order
		s.DB.First(&o, p.OrderID)
		out = append(out, s.penaltyView(p, o.OrderNo))
	}
	c.JSON(http.StatusOK, gin.H{"penalties": out, "server_time": s.Now().Format(timeFmt)})
}

func (s *Server) getPenalty(c *gin.Context) {
	u := currentUser(c)
	p, ok := s.scopedPenalty(c, u)
	if !ok {
		return
	}
	var o Order
	s.DB.First(&o, p.OrderID)

	var events []OrderEvent
	s.DB.Where("order_id = ?", o.ID).Order("at asc").Find(&events)
	timeline := make([]gin.H, 0, len(events))
	for _, e := range events {
		timeline = append(timeline, gin.H{"type": e.Type, "at": e.At.Format(timeFmt), "note": e.Note})
	}

	var items []EvidenceItem
	s.DB.Where("penalty_id = ?", p.ID).Order("created_at asc, id asc").Find(&items)

	// 证据缺口：要求类别在时间线事件或已上传证据中均无覆盖 → 缺失
	eventSet := map[string]bool{}
	for _, e := range events {
		eventSet[e.Type] = true
	}
	evCount := map[string]int{}
	for _, it := range items {
		evCount[it.Category]++
	}
	reqs := make([]gin.H, 0)
	missing := make([]string, 0)
	for _, cat := range RequiredCategories(p.Type) {
		covered := eventSet[cat] || evCount[cat] > 0
		reqs = append(reqs, gin.H{
			"category": cat, "label": CategoryLabel(cat),
			"event_present": eventSet[cat], "evidence_count": evCount[cat], "covered": covered,
		})
		if !covered {
			missing = append(missing, CategoryLabel(cat))
		}
	}

	itemViews := make([]gin.H, 0, len(items))
	for _, it := range items {
		itemViews = append(itemViews, gin.H{
			"id": it.ID, "category": it.Category, "category_label": CategoryLabel(it.Category),
			"file_name": it.FileName, "note": it.Note,
			"uploader_name": it.UploaderName, "created_at": it.CreatedAt.Format(timeFmt),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"penalty":      s.penaltyView(p, o.OrderNo),
		"order":        gin.H{"id": o.ID, "order_no": o.OrderNo, "amount": o.Amount, "placed_at": o.PlacedAt.Format(timeFmt)},
		"timeline":     timeline,
		"requirements": reqs,
		"missing":      missing,
		"evidence":     itemViews,
		"appeal":       s.appealViewForPenalty(p.ID),
		"server_time":  s.Now().Format(timeFmt),
	})
}

func (s *Server) appealViewForPenalty(penaltyID uint) gin.H {
	var a Appeal
	if err := s.DB.Where("penalty_id = ?", penaltyID).First(&a).Error; err != nil {
		return nil
	}
	return s.appealView(&a)
}

func (s *Server) appealView(a *Appeal) gin.H {
	var versions []EvidenceVersion
	s.DB.Where("appeal_id = ?", a.ID).Order("version_no asc").Find(&versions)
	vviews := make([]gin.H, 0, len(versions))
	for i := range versions {
		v := &versions[i]
		var items []EvidenceItem
		s.DB.Model(v).Association("Items").Find(&items)
		iv := make([]gin.H, 0, len(items))
		for _, it := range items {
			iv = append(iv, gin.H{"id": it.ID, "file_name": it.FileName, "category_label": CategoryLabel(it.Category)})
		}
		vviews = append(vviews, gin.H{
			"id": v.ID, "version_no": v.VersionNo, "type": v.Type, "note": v.Note,
			"created_by_name": v.CreatedByName, "created_at": v.CreatedAt.Format(timeFmt), "items": iv,
		})
	}
	var result *gin.H
	var r AppealResult
	if err := s.DB.Where("appeal_id = ?", a.ID).First(&r).Error; err == nil {
		result = &gin.H{
			"receipt_id": r.ReceiptID, "outcome": r.Outcome, "adjusted_amount": r.AdjustedAmount,
			"reason": r.Reason, "version_no": r.VersionNo, "received_at": r.ReceivedAt.Format(timeFmt),
		}
	}
	view := gin.H{
		"id": a.ID, "penalty_id": a.PenaltyID, "status": a.Status,
		"current_version": a.CurrentVersion, "submitted_at": a.SubmittedAt.Format(timeFmt),
		"versions": vviews, "result": result,
	}
	if a.DecidedAt != nil {
		view["decided_at"] = a.DecidedAt.Format(timeFmt)
	}
	return view
}

// ---------- 证据上传（员工+店长，申诉提交前） ----------

func (s *Server) uploadEvidence(c *gin.Context) {
	u := currentUser(c)
	p, ok := s.scopedPenalty(c, u)
	if !ok {
		return
	}
	var a Appeal
	if err := s.DB.Where("penalty_id = ?", p.ID).First(&a).Error; err == nil {
		switch a.Status {
		case AppealSubmitted:
			c.JSON(http.StatusBadRequest, gin.H{"error": "申诉已提交，补充材料请由店长通过“补件”提交（将生成新版本，原版本保留）"})
			return
		case AppealDecided:
			c.JSON(http.StatusBadRequest, gin.H{"error": "平台已出具处理结论，不能再上传证据"})
			return
		}
	}
	category := c.PostForm("category")
	if category != CatMealReady && category != CatPacked && category != CatRiderPickup && category != CatOther {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的证据类别"})
		return
	}
	fh, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择要上传的文件"})
		return
	}
	if fh.Size > 10<<20 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "文件不能超过 10MB"})
		return
	}
	dir := filepath.Join(s.UploadDir, strconv.FormatUint(uint64(p.ID), 10))
	name := fmt.Sprintf("%d_%s", s.Now().UnixNano(), filepath.Base(fh.Filename))
	name = strings.ReplaceAll(name, "..", "_")
	dst := filepath.Join(dir, name)
	if err := ensureDir(dir); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "存储目录创建失败"})
		return
	}
	if err := c.SaveUploadedFile(fh, dst); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "文件保存失败"})
		return
	}
	item := EvidenceItem{
		PenaltyID: p.ID, Category: category, FileName: fh.Filename, StoredPath: dst,
		Note: c.PostForm("note"), UploadedByID: u.ID, UploaderName: u.Name, CreatedAt: s.Now(),
	}
	s.DB.Create(&item)
	c.JSON(http.StatusCreated, gin.H{"id": item.ID})
}

// ---------- 提交申诉（仅店长，期限内） ----------

func (s *Server) submitAppeal(c *gin.Context) {
	u, ok := requireManager(c)
	if !ok {
		return
	}
	p, ok := s.scopedPenalty(c, u)
	if !ok {
		return
	}
	now := s.Now()
	// 期限边界：now <= deadline 可提交，now > deadline 拒绝并明确提示
	if now.After(p.Deadline) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("已超过申诉期限（截止 %s），无法提交申诉", p.Deadline.Format(timeFmt)),
			"code":  "deadline_exceeded",
		})
		return
	}
	switch p.Status {
	case PenaltySubmitted:
		c.JSON(http.StatusBadRequest, gin.H{"error": "该处罚已有进行中的申诉"})
		return
	case PenaltyUpheld, PenaltyRevoked, PenaltyPartial:
		c.JSON(http.StatusBadRequest, gin.H{"error": "该处罚已有平台处理结论，不能重复申诉"})
		return
	}
	var items []EvidenceItem
	s.DB.Where("penalty_id = ?", p.ID).Find(&items)
	if len(items) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请至少上传一份证据材料再提交申诉"})
		return
	}
	note := c.PostForm("note")

	var appeal Appeal
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Where("penalty_id = ?", p.ID).First(&appeal).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			appeal = Appeal{
				PenaltyID: p.ID, StoreID: p.StoreID, Status: AppealSubmitted,
				CurrentVersion: 0, SubmittedByID: u.ID, SubmittedAt: now,
			}
			if err := tx.Create(&appeal).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			if appeal.Status != AppealWithdrawn {
				return errors.New("申诉状态不允许再次提交")
			}
			appeal.Status = AppealSubmitted
			appeal.SubmittedByID = u.ID
			appeal.SubmittedAt = now
			if err := tx.Save(&appeal).Error; err != nil {
				return err
			}
		}
		// 生成提交版本快照：当前全部证据材料
		if err := s.newVersion(tx, &appeal, VersionSubmission, note, u.ID, u.Name, items); err != nil {
			return err
		}
		return tx.Model(&Penalty{}).Where("id = ?", p.ID).Update("status", PenaltySubmitted).Error
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	s.notify(p.StoreID, "appeal_submitted",
		fmt.Sprintf("处罚 #%d 的申诉已提交（版本 v%d），等待平台处理", p.ID, appeal.CurrentVersion))
	if s.SimulatePlatform {
		go s.simulateResult(appeal.ID)
	}
	c.JSON(http.StatusOK, gin.H{"appeal_id": appeal.ID, "version": appeal.CurrentVersion})
}

// newVersion 在事务内生成新版本并快照给定证据集合（调用方需保证在 tx 中）
func (s *Server) newVersion(tx *gorm.DB, a *Appeal, vtype, note string, byID uint, byName string, items []EvidenceItem) error {
	a.CurrentVersion++
	v := EvidenceVersion{
		AppealID: a.ID, VersionNo: a.CurrentVersion, Type: vtype, Note: note,
		CreatedByID: byID, CreatedByName: byName, CreatedAt: s.Now(),
	}
	if err := tx.Create(&v).Error; err != nil {
		return err
	}
	if len(items) > 0 {
		if err := tx.Model(&v).Association("Items").Append(items); err != nil {
			return fmt.Errorf("快照证据失败: %w", err)
		}
	}
	return tx.Model(a).Update("current_version", a.CurrentVersion).Error
}

// ---------- 补件 / 撤回（仅店长，申诉处理中） ----------

func (s *Server) scopedAppeal(c *gin.Context, u *User) (*Appeal, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的申诉ID"})
		return nil, false
	}
	var a Appeal
	if err := s.DB.Where("id = ? AND store_id = ?", id, u.StoreID).First(&a).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "申诉记录不存在"})
		return nil, false
	}
	return &a, true
}

func (s *Server) supplementAppeal(c *gin.Context) {
	u, ok := requireManager(c)
	if !ok {
		return
	}
	a, ok := s.scopedAppeal(c, u)
	if !ok {
		return
	}
	if a.Status != AppealSubmitted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "只有处理中的申诉可以补件"})
		return
	}
	category := c.PostForm("category")
	if category == "" {
		category = CatOther
	}
	fh, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择要上传的补充文件"})
		return
	}
	dir := filepath.Join(s.UploadDir, strconv.FormatUint(uint64(a.PenaltyID), 10))
	name := strings.ReplaceAll(fmt.Sprintf("%d_%s", s.Now().UnixNano(), filepath.Base(fh.Filename)), "..", "_")
	dst := filepath.Join(dir, name)
	if err := ensureDir(dir); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "存储目录创建失败"})
		return
	}
	if err := c.SaveUploadedFile(fh, dst); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "文件保存失败"})
		return
	}
	note := c.PostForm("note")
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		item := EvidenceItem{
			PenaltyID: a.PenaltyID, Category: category, FileName: fh.Filename, StoredPath: dst,
			Note: note, UploadedByID: u.ID, UploaderName: u.Name, CreatedAt: s.Now(),
		}
		if err := tx.Create(&item).Error; err != nil {
			return err
		}
		var all []EvidenceItem
		tx.Where("penalty_id = ?", a.PenaltyID).Find(&all)
		// 补件生成新版本，快照包含全部材料；历史版本原样保留
		return s.newVersion(tx, a, VersionSupplement, note, u.ID, u.Name, all)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "补件失败"})
		return
	}
	s.notify(a.StoreID, "appeal_supplemented",
		fmt.Sprintf("处罚 #%d 的申诉已补件（版本 v%d），原版本保留", a.PenaltyID, a.CurrentVersion))
	c.JSON(http.StatusOK, gin.H{"version": a.CurrentVersion})
}

func (s *Server) withdrawAppeal(c *gin.Context) {
	u, ok := requireManager(c)
	if !ok {
		return
	}
	a, ok := s.scopedAppeal(c, u)
	if !ok {
		return
	}
	if a.Status != AppealSubmitted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "只有处理中的申诉可以撤回"})
		return
	}
	note := c.PostForm("note")
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var all []EvidenceItem
		tx.Where("penalty_id = ?", a.PenaltyID).Find(&all)
		// 撤回同样留痕：生成撤回版本，材料快照保留
		if err := s.newVersion(tx, a, VersionWithdrawal, note, u.ID, u.Name, all); err != nil {
			return err
		}
		if err := tx.Model(a).Update("status", AppealWithdrawn).Error; err != nil {
			return err
		}
		return tx.Model(&Penalty{}).Where("id = ?", a.PenaltyID).Update("status", PenaltyWithdrawn).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "撤回失败"})
		return
	}
	s.notify(a.StoreID, "appeal_withdrawn", fmt.Sprintf("处罚 #%d 的申诉已撤回（记录保留于 v%d）", a.PenaltyID, a.CurrentVersion))
	c.JSON(http.StatusOK, gin.H{"version": a.CurrentVersion})
}

// ---------- 申诉详情 / 文件 / 消息 ----------

func (s *Server) getAppeal(c *gin.Context) {
	u := currentUser(c)
	a, ok := s.scopedAppeal(c, u)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"appeal": s.appealView(a)})
}

func (s *Server) downloadFile(c *gin.Context) {
	u := currentUser(c)
	id, _ := strconv.Atoi(c.Param("id"))
	var item EvidenceItem
	if err := s.DB.First(&item, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
		return
	}
	var p Penalty
	if err := s.DB.First(&p, item.PenaltyID).Error; err != nil || p.StoreID != u.StoreID {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
		return
	}
	c.FileAttachment(item.StoredPath, item.FileName)
}

func (s *Server) listNotifications(c *gin.Context) {
	u := currentUser(c)
	var ns []Notification
	s.DB.Where("store_id = ?", u.StoreID).Order("id desc").Limit(50).Find(&ns)
	out := make([]gin.H, 0, len(ns))
	for _, n := range ns {
		out = append(out, gin.H{
			"id": n.ID, "type": n.Type, "message": n.Message,
			"read": n.Read, "created_at": n.CreatedAt.Format(timeFmt),
		})
	}
	c.JSON(http.StatusOK, gin.H{"notifications": out})
}

func (s *Server) readAllNotifications(c *gin.Context) {
	u := currentUser(c)
	s.DB.Model(&Notification{}).Where("store_id = ? AND read = ?", u.StoreID, false).Update("read", true)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
