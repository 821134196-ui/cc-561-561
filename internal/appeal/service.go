package appeal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"time"

	"appeal/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// APIError 携带 HTTP 状态码，handler 层直接映射。
type APIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

func apiError(status int, code, msg string) *APIError {
	return &APIError{Status: status, Code: code, Msg: msg}
}

func IsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// ReceiptInput 模拟平台回执报文。
type ReceiptInput struct {
	PenaltyID     uint   `json:"penalty_id"`
	ReceiptKey    string `json:"receipt_key"`
	Kind          string `json:"kind"`           // result / correction
	Decision      string `json:"decision"`       // upheld / revoked / adjusted
	AdjustedCents int64  `json:"adjusted_cents"` // decision=adjusted 时的调整后金额
	Message       string `json:"message"`
	RawPayload    string `json:"raw_payload"`
}

// ReceiptOutcome 供 handler 决定响应内容。
type ReceiptOutcome struct {
	Receipt    *models.PlatformReceipt
	Duplicated bool // 同一回执重复送达
	Penalty    *models.Penalty
}

// EvidenceFile 上传文件句柄（nil 表示纯文字说明）。
type EvidenceFile struct {
	Header *multipart.FileHeader
	Note   string
}

type Service struct {
	DB        *gorm.DB
	UploadDir string
	nowFunc   func() time.Time
}

func NewService(db *gorm.DB, uploadDir string) *Service {
	return &Service{DB: db, UploadDir: uploadDir, nowFunc: time.Now}
}

func (s *Service) now() time.Time { return s.nowFunc() }

// runTxRetry 包裹事务：WAL 下快照型 SQLITE_BUSY 只能由应用层整体重试。
func (s *Service) runTxRetry(fn func(tx *gorm.DB) error) error {
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		err = s.DB.Transaction(fn)
		if err == nil || !isLockedErr(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
	return err
}

func isLockedErr(err error) bool {
	return strings.Contains(err.Error(), "database is locked") ||
		strings.Contains(err.Error(), "SQLITE_BUSY")
}

// SetClock 仅供测试注入时间。
func (s *Service) SetClock(f func() time.Time) {
	if f == nil {
		s.nowFunc = time.Now
	} else {
		s.nowFunc = f
	}
}

// loadPenalty 跨店访问统一按 404 处理（不向其他门店暴露资源是否存在）。
func (s *Service) loadPenalty(tx *gorm.DB, penaltyID, storeID uint) (*models.Penalty, error) {
	var p models.Penalty
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND store_id = ?", penaltyID, storeID).First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, apiError(404, "penalty_not_found", "处罚记录不存在或不属于当前门店")
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// withinDeadline 期限边界：deadline 当刻仍可提交（!now.After(deadline)）。
func withinDeadline(deadline time.Time, now time.Time) bool { return !now.After(deadline) }

// IsOverdue 供列表/详情计算实时超期标记（pending/withdrawn 过了期限）。
func IsOverdue(p *models.Penalty, now time.Time) bool {
	if p == nil {
		return false
	}
	if p.Status != models.StatusPending && p.Status != models.StatusWithdrawn {
		return false
	}
	return now.After(p.DeadlineAt)
}

// MissingSlots 返回尚缺证据的槽位（该处罚下任意版本——含草稿——存在即视为已覆盖）。
func MissingSlots(db *gorm.DB, penaltyID uint) ([]string, error) {
	var p models.Penalty
	if err := db.Select("type").First(&p, penaltyID).Error; err != nil {
		return nil, err
	}
	required := models.RequiredSlots(p.Type)
	if len(required) == 0 {
		return nil, nil
	}
	var have []string
	if err := db.Model(&models.EvidenceVersion{}).
		Where("penalty_id = ?", penaltyID).Distinct().Pluck("slot", &have).Error; err != nil {
		return nil, err
	}
	haveSet := map[string]bool{}
	for _, h := range have {
		haveSet[h] = true
	}
	var missing []string
	for _, slot := range required {
		if !haveSet[slot] {
			missing = append(missing, slot)
		}
	}
	return missing, nil
}

// ---------------- 证据整理（草稿） / 补件 ----------------

var allowedExt = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".gif": "image/gif", ".webp": "image/webp",
	".pdf": "application/pdf", ".mp4": "video/mp4",
	".mov": "video/quicktime", ".txt": "text/plain",
}

func randToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// AddDraftEvidence 员工/店长在待申诉或已撤回状态下整理证据（草稿，可删改）。
func (s *Service) AddDraftEvidence(penaltyID, storeID, uploaderID uint, uploaderName, slot, kind, note string, fh *multipart.FileHeader) (*models.EvidenceVersion, error) {
	var created *models.EvidenceVersion
	err := s.runTxRetry(func(tx *gorm.DB) error {
		p, err := s.loadPenalty(tx, penaltyID, storeID)
		if err != nil {
			return err
		}
		if p.Status == models.StatusExpired ||
			((p.Status == models.StatusPending || p.Status == models.StatusWithdrawn) &&
				!withinDeadline(p.DeadlineAt, s.now())) {
			return apiError(409, "deadline_passed",
				fmt.Sprintf("申诉期限已于 %s 截止，无法再补充材料", p.DeadlineAt.Format("2006-01-02 15:04")))
		}
		if p.Status != models.StatusPending && p.Status != models.StatusWithdrawn {
			return apiError(400, "draft_not_allowed",
				"当前状态为「"+models.StatusLabel(p.Status)+"」，新增材料将作为补件归档，不能存为草稿")
		}
		v, err := s.buildVersion(p, slot, kind, note, uploaderID, uploaderName, fh)
		if err != nil {
			return err
		}
		// 草稿链接同槽位上一版本（可能也是草稿）。
		var prev models.EvidenceVersion
		if e := tx.Where("penalty_id = ? AND slot = ?", penaltyID, slot).
			Order("id DESC").First(&prev).Error; e == nil {
			v.SupersedesID = &prev.ID
		}
		if err := tx.Create(v).Error; err != nil {
			return err
		}
		created = v
		return nil
	})
	return created, err
}

// AddSupplement 已提交状态下的补件：立即归档为 supplement 动作，原版本一律保留。
func (s *Service) AddSupplement(penaltyID, storeID, actorID uint, actorName, slot, kind, note string, fh *multipart.FileHeader) (*models.EvidenceVersion, error) {
	var created *models.EvidenceVersion
	err := s.runTxRetry(func(tx *gorm.DB) error {
		p, err := s.loadPenalty(tx, penaltyID, storeID)
		if err != nil {
			return err
		}
		if p.Status != models.StatusSubmitted {
			return apiError(400, "supplement_not_allowed", "仅「审核中」的处罚可以补件，当前状态："+models.StatusLabel(p.Status))
		}
		v, err := s.buildVersion(p, slot, kind, note, actorID, actorName, fh)
		if err != nil {
			return err
		}
		var prev models.EvidenceVersion
		if e := tx.Where("penalty_id = ? AND slot = ?", penaltyID, slot).
			Order("id DESC").First(&prev).Error; e == nil {
			v.SupersedesID = &prev.ID
		}
		act := &models.AppealAction{
			PenaltyID: p.ID, StoreID: storeID, Kind: models.ActionSupplement,
			ActorID: &actorID, ActorName: actorName,
			Note:      note,
			CreatedAt: s.now(),
		}
		if err := tx.Create(act).Error; err != nil {
			return err
		}
		v.AppealActionID = &act.ID
		if err := tx.Create(v).Error; err != nil {
			return err
		}
		act.SnapshotIDs = models.JSONInts{v.ID}
		if err := tx.Save(act).Error; err != nil {
			return err
		}
		created = v
		return nil
	})
	return created, err
}

func (s *Service) buildVersion(p *models.Penalty, slot, kind, note string, uploaderID uint, uploaderName string, fh *multipart.FileHeader) (*models.EvidenceVersion, error) {
	if slot == "" {
		slot = models.SlotOther
	}
	valid := map[string]bool{models.SlotMealReady: true, models.SlotPacked: true, models.SlotRiderPickup: true, models.SlotOther: true}
	if !valid[slot] {
		return nil, apiError(400, "bad_slot", "证据类型无效")
	}
	v := &models.EvidenceVersion{
		StoreID: p.StoreID, OrderID: p.OrderID, PenaltyID: p.ID,
		Slot: slot, Kind: kind, Note: note,
		UploaderID: uploaderID, UploaderName: uploaderName,
		CreatedAt: s.now(),
	}
	if kind == "" {
		if fh != nil {
			kind = "file"
		} else {
			kind = "note"
		}
	}
	if kind == "note" {
		if strings.TrimSpace(note) == "" {
			return nil, apiError(400, "empty_note", "文字说明不能为空")
		}
		v.Kind = "note"
		return v, nil
	}
	if fh == nil {
		return nil, apiError(400, "no_file", "请选择附件或改用文字说明")
	}
	if fh.Size > 20<<20 {
		return nil, apiError(400, "file_too_large", "附件不能超过 20MB")
	}
	ext := strings.ToLower(filepath.Ext(fh.Filename))
	mime, ok := allowedExt[ext]
	if !ok {
		return nil, apiError(400, "bad_file_type", "不支持的文件类型："+ext)
	}
	relDir := filepath.Join(fmt.Sprintf("store-%d", p.StoreID), fmt.Sprintf("penalty-%d", p.ID))
	dir := filepath.Join(s.UploadDir, relDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s%s", s.now().Format("20060102T150405.000"), ext)
	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		name = s.now().Format("20060102T150405.000") + "-" + randToken() + ext
	}
	dst := filepath.Join(dir, name)
	if err := saveUpload(fh, dst); err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(dst)
	v.Kind = kindFromMime(mime)
	v.OriginalName = fh.Filename
	v.StoredPath = abs
	v.MimeType = mime
	v.SizeBytes = fh.Size
	return v, nil
}

func kindFromMime(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	default:
		return "doc"
	}
}

// DeleteDraft 仅允许删除尚未归档的草稿；已随提交/补件归档的版本不可删除。
func (s *Service) DeleteDraft(evidenceID, storeID uint) error {
	return s.runTxRetry(func(tx *gorm.DB) error {
		var v models.EvidenceVersion
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND store_id = ?", evidenceID, storeID).First(&v).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apiError(404, "evidence_not_found", "证据不存在或不属于当前门店")
		}
		if err != nil {
			return err
		}
		if v.AppealActionID != nil {
			return apiError(409, "version_archived", "该版本已随申诉归档，不能删除或替换；如需更正请上传新版本")
		}
		var p models.Penalty
		if err := tx.First(&p, v.PenaltyID).Error; err != nil {
			return err
		}
		if p.Status != models.StatusPending && p.Status != models.StatusWithdrawn {
			return apiError(409, "draft_locked", "处罚已提交，草稿已锁定")
		}
		if v.StoredPath != "" {
			_ = os.Remove(v.StoredPath)
		}
		return tx.Delete(&v).Error
	})
}

// ---------------- 提交 / 撤回 ----------------

// Submit 店长正式提交。员工调用应在 handler 层被拒（403）。
func (s *Service) Submit(penaltyID, storeID, actorID uint, actorName, note string) (*models.Penalty, []string, error) {
	var result *models.Penalty
	var missing []string
	var deadlineErr *APIError
	err := s.runTxRetry(func(tx *gorm.DB) error {
		p, err := s.loadPenalty(tx, penaltyID, storeID)
		if err != nil {
			return err
		}
		if p.Status == models.StatusSubmitted {
			return apiError(409, "already_submitted", "该处罚已在审核中，请勿重复提交")
		}
		if p.Status == models.StatusClosed {
			return apiError(409, "already_closed", "平台已出具结论，无需再提交")
		}
		if p.Status == models.StatusExpired {
			return apiError(409, "deadline_passed",
				fmt.Sprintf("申诉期限已于 %s 截止，无法提交", p.DeadlineAt.Format("2006-01-02 15:04")))
		}
		if p.Status != models.StatusPending && p.Status != models.StatusWithdrawn {
			return apiError(409, "status_not_submittable", "当前状态「"+models.StatusLabel(p.Status)+"」不可提交")
		}
		now := s.now()
		if !withinDeadline(p.DeadlineAt, now) {
			// 边界明确：超期状态需要落库提交，但接口仍要返回 409——
			// 因此先正常结束事务，再在事务外返回 deadline_passed。
			p.Status = models.StatusExpired
			exp := now
			p.ExpiredAt = &exp
			if err := tx.Save(p).Error; err != nil {
				return err
			}
			_ = s.notifyExpired(tx, p)
			deadlineErr = apiError(409, "deadline_passed",
				fmt.Sprintf("申诉期限已于 %s 截止，无法提交", p.DeadlineAt.Format("2006-01-02 15:04")))
			return nil
		}

		missing, err = MissingSlots(tx, p.ID)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			labels := make([]string, 0, len(missing))
			for _, slot := range missing {
				labels = append(labels, models.SlotLabel(slot))
			}
			return apiError(422, "evidence_missing", "尚缺必需证据："+strings.Join(labels, "、"))
		}

		// 归档所有草稿；若没有草稿（撤回后无新材料直接重申），取每槽位最新版本。
		var drafts []models.EvidenceVersion
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("penalty_id = ? AND appeal_action_id IS NULL", p.ID).
			Find(&drafts).Error; err != nil {
			return err
		}
		snapshotIDs := make(models.JSONInts, 0, len(drafts))
		for i := range drafts {
			snapshotIDs = append(snapshotIDs, drafts[i].ID)
		}
		if len(drafts) == 0 {
			var latest []models.EvidenceVersion
			if err := tx.Where("penalty_id = ?", p.ID).Order("slot ASC, id DESC").Find(&latest).Error; err != nil {
				return err
			}
			seen := map[string]bool{}
			for _, v := range latest {
				if !seen[v.Slot] {
					seen[v.Slot] = true
					snapshotIDs = append(snapshotIDs, v.ID)
				}
			}
		}

		act := &models.AppealAction{
			PenaltyID: p.ID, StoreID: storeID, Kind: models.ActionSubmit,
			ActorID: &actorID, ActorName: actorName, Note: note,
			SnapshotIDs: snapshotIDs, CreatedAt: now,
		}
		if err := tx.Create(act).Error; err != nil {
			return err
		}
		for i := range drafts {
			if err := tx.Model(&models.EvidenceVersion{}).Where("id = ?", drafts[i].ID).
				Update("appeal_action_id", act.ID).Error; err != nil {
				return err
			}
		}
		p.Status = models.StatusSubmitted
		p.SubmittedAt = &now
		if err := tx.Save(p).Error; err != nil {
			return err
		}
		result = p
		return nil
	})
	if err == nil && deadlineErr != nil {
		err = deadlineErr
	}
	return result, missing, err
}

// Withdraw 撤回：仅审核中可撤回；提交时归档的证据原样保留。
func (s *Service) Withdraw(penaltyID, storeID, actorID uint, actorName, note string) (*models.Penalty, error) {
	var result *models.Penalty
	err := s.runTxRetry(func(tx *gorm.DB) error {
		p, err := s.loadPenalty(tx, penaltyID, storeID)
		if err != nil {
			return err
		}
		if p.Status == models.StatusClosed {
			return apiError(409, "already_closed", "平台已出具结论，不能撤回")
		}
		if p.Status != models.StatusSubmitted {
			return apiError(409, "not_submitted", "仅「审核中」的处罚可以撤回，当前状态："+models.StatusLabel(p.Status))
		}
		now := s.now()
		act := &models.AppealAction{
			PenaltyID: p.ID, StoreID: storeID, Kind: models.ActionWithdraw,
			ActorID: &actorID, ActorName: actorName, Note: note, CreatedAt: now,
		}
		if err := tx.Create(act).Error; err != nil {
			return err
		}
		p.Status = models.StatusWithdrawn
		p.WithdrawnAt = &now
		if err := tx.Save(p).Error; err != nil {
			return err
		}
		// 撤回时若已过期限，直接置为超期。
		if !withinDeadline(p.DeadlineAt, now) {
			p.Status = models.StatusExpired
			p.ExpiredAt = &now
			if err := tx.Save(p).Error; err != nil {
				return err
			}
			_ = s.notifyExpired(tx, p)
		}
		result = p
		return nil
	})
	return result, err
}

// ---------------- 平台回执（模拟） ----------------

// ReceiveReceipt 处理平台回执。
// 关键保证：
//  1. receipt_key 唯一索引兜底幂等——重复送达只累加 delivery_count，不产生第二条结论/动作/通知；
//  2. 首次结论以外的「结果」回执被拒绝，改变结论只能走 correction，且更正保留原版本；
//  3. 整段在一个事务 + 行锁内完成，避免并发重复送达竞态。
func (s *Service) ReceiveReceipt(storeID uint, in ReceiptInput) (*ReceiptOutcome, error) {
	if in.ReceiptKey == "" {
		return nil, apiError(400, "bad_receipt", "缺少回执编号 receipt_key")
	}
	if in.Kind != models.ReceiptResult && in.Kind != models.ReceiptCorrection {
		return nil, apiError(400, "bad_receipt", "回执类型必须是 result 或 correction")
	}
	if in.Decision != models.DecisionUpheld && in.Decision != models.DecisionRevoked && in.Decision != models.DecisionAdjusted {
		return nil, apiError(400, "bad_receipt", "结论必须是 upheld / revoked / adjusted")
	}
	if in.Decision == models.DecisionAdjusted && in.AdjustedCents < 0 {
		return nil, apiError(400, "bad_receipt", "调整后金额不能为负")
	}

	out := &ReceiptOutcome{}
	err := s.runTxRetry(func(tx *gorm.DB) error {
		p, err := s.loadPenalty(tx, in.PenaltyID, storeID)
		if err != nil {
			return err
		}

		// 幂等检查：同一 key 可能已落到其他处罚上。
		var existing models.PlatformReceipt
		err = tx.Where("receipt_key = ?", in.ReceiptKey).First(&existing).Error
		if err == nil {
			if existing.PenaltyID != p.ID || existing.StoreID != storeID {
				return apiError(409, "receipt_key_conflict", "回执编号已属于其他处罚，报文可能有误")
			}
			existing.DeliveryCount++
			existing.LastSeenAt = s.now()
			if err := tx.Save(&existing).Error; err != nil {
				return err
			}
			out.Receipt = &existing
			out.Duplicated = true
			out.Penalty = p
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		now := s.now()
		receipt := &models.PlatformReceipt{
			StoreID:       storeID,
			PenaltyID:     p.ID,
			ReceiptKey:    in.ReceiptKey,
			Kind:          in.Kind,
			Decision:      in.Decision,
			AdjustedCents: in.AdjustedCents,
			Message:       in.Message,
			RawPayload:    in.RawPayload,
			ReceivedAt:    now,
			ProcessedAt:   now,
			LastSeenAt:    now,
			DeliveryCount: 1,
		}

		if in.Kind == models.ReceiptResult {
			if p.Status != models.StatusSubmitted {
				return apiError(409, "not_submitted", "门店尚未提交申诉（或已有结论），不能接收首次结果回执")
			}
			if dup, err := s.insertReceiptOrMarkDup(tx, receipt, p, out); err != nil || dup {
				return err
			}
			act := &models.AppealAction{
				PenaltyID: p.ID, StoreID: storeID, Kind: models.ActionResult,
				ActorName: "平台", Note: in.Message, ReceiptID: &receipt.ID, CreatedAt: now,
			}
			if err := tx.Create(act).Error; err != nil {
				return err
			}
			p.Status = models.StatusClosed
			p.CurrentDecision = in.Decision
			p.AdjustedCents = in.AdjustedCents
			p.FinalizedAt = &now
			if err := tx.Save(p).Error; err != nil {
				return err
			}
			if err := s.notifyResult(tx, p, receipt, false); err != nil {
				return err
			}
		} else {
			// 更正：必须已有生效结论；旧回执与旧动作原样保留。
			if p.Status != models.StatusClosed {
				return apiError(409, "no_prior_result", "该处罚尚无首次结论，不能发送更正回执")
			}
			var prior models.PlatformReceipt
			if err := tx.Where("penalty_id = ?", p.ID).Order("id DESC").First(&prior).Error; err != nil {
				return err
			}
			receipt.SupersedesID = &prior.ID
			if dup, err := s.insertReceiptOrMarkDup(tx, receipt, p, out); err != nil || dup {
				return err
			}
			act := &models.AppealAction{
				PenaltyID: p.ID, StoreID: storeID, Kind: models.ActionCorrection,
				ActorName: "平台", Note: in.Message, ReceiptID: &receipt.ID, CreatedAt: now,
			}
			if err := tx.Create(act).Error; err != nil {
				return err
			}
			p.CurrentDecision = in.Decision
			p.AdjustedCents = in.AdjustedCents
			p.FinalizedAt = &now
			if err := tx.Save(p).Error; err != nil {
				return err
			}
			if err := s.notifyResult(tx, p, receipt, true); err != nil {
				return err
			}
		}
		out.Receipt = receipt
		out.Penalty = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// insertReceiptOrMarkDup 写入回执。并发竞态下若唯一索引拦截到同 key 记录，
// 回查已有回执、累加送达次数并标记 dup=true（调用方随即结束事务，不再产生结论）。
// 返回 (dup, error)。
func (s *Service) insertReceiptOrMarkDup(tx *gorm.DB, receipt *models.PlatformReceipt, p *models.Penalty, out *ReceiptOutcome) (bool, error) {
	err := tx.Create(receipt).Error
	if err == nil {
		return false, nil
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		return false, err
	}
	var existing models.PlatformReceipt
	if e := tx.Where("receipt_key = ?", receipt.ReceiptKey).First(&existing).Error; e != nil {
		return false, e
	}
	if existing.PenaltyID != p.ID || existing.StoreID != p.StoreID {
		return false, apiError(409, "receipt_key_conflict", "回执编号已属于其他处罚，报文可能有误")
	}
	existing.DeliveryCount++
	existing.LastSeenAt = s.now()
	if err := tx.Save(&existing).Error; err != nil {
		return false, err
	}
	out.Receipt = &existing
	out.Duplicated = true
	out.Penalty = p
	return true, nil
}

func (s *Service) notifyResult(tx *gorm.DB, p *models.Penalty, r *models.PlatformReceipt, correction bool) error {
	var o models.Order
	_ = tx.Select("platform_order_no").First(&o, p.OrderID)
	title := "申诉结果：" + models.DecisionLabel(r.Decision)
	if correction {
		title = "平台更正：" + models.DecisionLabel(r.Decision)
	}
	n := &models.Notification{
		StoreID: p.StoreID, Category: "result", RefKey: r.ReceiptKey,
		Title: title,
		Body:  fmt.Sprintf("订单 %s 的「%s」处罚：%s", o.PlatformOrderNo, models.PenaltyTypeLabel(p.Type), r.Message),
	}
	return insertNotificationOnce(tx, n)
}

func (s *Service) notifyExpired(tx *gorm.DB, p *models.Penalty) error {
	var o models.Order
	_ = tx.Select("platform_order_no").First(&o, p.OrderID)
	return insertNotificationOnce(tx, &models.Notification{
		StoreID: p.StoreID, Category: "expired", RefKey: fmt.Sprintf("expired-%d", p.ID),
		Title: "申诉已超期",
		Body:  fmt.Sprintf("订单 %s 的「%s」处罚已于 %s 截止申诉", o.PlatformOrderNo, models.PenaltyTypeLabel(p.Type), p.DeadlineAt.Format("01-02 15:04")),
	})
}

func insertNotificationOnce(tx *gorm.DB, n *models.Notification) error {
	// ref_key 同店唯一兜底（model 层复合唯一索引），重复直接忽略。
	err := tx.Create(n).Error
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return nil
	}
	return err
}

// SweepExpired 启动时与定时任务调用：把过了期限仍待处理/已撤回的处罚置为超期。
func (s *Service) SweepExpired() (int, error) {
	now := s.now()
	var ids []uint
	if err := s.DB.Model(&models.Penalty{}).
		Where("status IN ? AND deadline_at < ?", []string{models.StatusPending, models.StatusWithdrawn}, now).
		Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	for _, id := range ids {
		err := s.runTxRetry(func(tx *gorm.DB) error {
			var p models.Penalty
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&p, id).Error; err != nil {
				return err
			}
			if p.Status != models.StatusPending && p.Status != models.StatusWithdrawn {
				return nil
			}
			if !now.After(p.DeadlineAt) {
				return nil
			}
			p.Status = models.StatusExpired
			t := now
			p.ExpiredAt = &t
			if err := tx.Save(&p).Error; err != nil {
				return err
			}
			return s.notifyExpired(tx, &p)
		})
		if err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
