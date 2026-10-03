package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// receiptPayload 平台回执（本地模拟的平台接口报文）
type receiptPayload struct {
	ReceiptID      string   `json:"receipt_id"`
	AppealID       uint     `json:"appeal_id"`
	Outcome        string   `json:"outcome"` // upheld / revoked / partial
	AdjustedAmount *float64 `json:"adjusted_amount,omitempty"`
	Reason         string   `json:"reason"`
}

var errReceiptConflict = errors.New("receipt conflict")

// processReceipt 幂等处理平台回执：
//   - receipt_id 首次到达 → 落库并生成唯一结论
//   - 相同 receipt_id + 相同内容 → 返回已有结论（duplicate=true），不重复处理
//   - 相同 receipt_id + 不同内容 → 409 冲突，保留原结论，绝不产生两个相互冲突的结果
func (s *Server) processReceipt(p receiptPayload) (result *AppealResult, duplicate bool, err error) {
	if p.ReceiptID == "" || p.AppealID == 0 {
		return nil, false, errors.New("回执缺少 receipt_id 或 appeal_id")
	}
	if p.Outcome != OutcomeUpheld && p.Outcome != OutcomeRevoked && p.Outcome != OutcomePartial {
		return nil, false, errors.New("无效的结论类型")
	}
	if p.Outcome == OutcomePartial && p.AdjustedAmount == nil {
		return nil, false, errors.New("部分调整必须给出调整后金额")
	}
	canonical, _ := json.Marshal(p)
	hash := sha256.Sum256(canonical)
	hashStr := hex.EncodeToString(hash[:])
	now := s.Now()

	err = s.DB.Transaction(func(tx *gorm.DB) error {
		var existing PlatformReceipt
		findErr := tx.Where("receipt_id = ?", p.ReceiptID).First(&existing).Error
		if findErr == nil {
			// 已处理过的回执：比对内容
			if existing.PayloadHash != hashStr {
				return errReceiptConflict
			}
			var r AppealResult
			if err := tx.Where("receipt_id = ?", p.ReceiptID).First(&r).Error; err != nil {
				return err
			}
			result = &r
			duplicate = true
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}

		var a Appeal
		if err := tx.First(&a, p.AppealID).Error; err != nil {
			return errors.New("申诉不存在")
		}
		if a.Status == AppealDecided {
			return errors.New("该申诉已有处理结论，拒绝重复回执")
		}
		if a.Status != AppealSubmitted {
			return errors.New("申诉不在处理中，无法登记结论")
		}

		// 登记回执（receipt_id 唯一索引兜底并发重复）
		rec := PlatformReceipt{
			ReceiptID: p.ReceiptID, AppealID: a.ID,
			PayloadHash: hashStr, Payload: string(canonical), ProcessedAt: now,
		}
		if err := tx.Create(&rec).Error; err != nil {
			return err
		}
		r := AppealResult{
			AppealID: a.ID, ReceiptID: p.ReceiptID, Outcome: p.Outcome,
			AdjustedAmount: p.AdjustedAmount, Reason: p.Reason,
			VersionNo: a.CurrentVersion, ReceivedAt: now,
		}
		if err := tx.Create(&r).Error; err != nil {
			return err
		}
		if err := tx.Model(&a).Updates(map[string]interface{}{"status": AppealDecided, "decided_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&Penalty{}).Where("id = ?", a.PenaltyID).Update("status", p.Outcome).Error; err != nil {
			return err
		}
		result = &r
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if !duplicate {
		s.notify(resultAppealStore(s, result.AppealID), "appeal_decided",
			fmt.Sprintf("处罚申诉已有平台结论：%s（回执 %s）", outcomeLabel(result.Outcome), result.ReceiptID))
	}
	return result, duplicate, nil
}

func resultAppealStore(s *Server, appealID uint) uint {
	var a Appeal
	if err := s.DB.First(&a, appealID).Error; err != nil {
		return 0
	}
	return a.StoreID
}

func outcomeLabel(o string) string {
	switch o {
	case OutcomeUpheld:
		return "维持处罚"
	case OutcomeRevoked:
		return "撤销处罚"
	case OutcomePartial:
		return "部分调整"
	}
	return o
}

// postReceipt 模拟平台回调入口
func (s *Server) postReceipt(c *gin.Context) {
	if !platformAuth(c) {
		return
	}
	var p receiptPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "回执报文格式错误"})
		return
	}
	result, dup, err := s.processReceipt(p)
	if err != nil {
		if errors.Is(err, errReceiptConflict) {
			var existing AppealResult
			s.DB.Where("receipt_id = ?", p.ReceiptID).First(&existing)
			c.JSON(http.StatusConflict, gin.H{
				"error":    "相同 receipt_id 的回执内容不一致，已保留原结论",
				"existing": gin.H{"outcome": existing.Outcome, "reason": existing.Reason},
			})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	status := http.StatusCreated
	if dup {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{
		"duplicate": dup,
		"result": gin.H{
			"appeal_id": result.AppealID, "receipt_id": result.ReceiptID,
			"outcome": result.Outcome, "received_at": result.ReceivedAt.Format(timeFmt),
		},
	})
}

// postCorrection 平台更正：生成 correction 版本，历史版本保留
func (s *Server) postCorrection(c *gin.Context) {
	if !platformAuth(c) {
		return
	}
	var req struct {
		AppealID uint   `json:"appeal_id"`
		Note     string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.AppealID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	var a Appeal
	if err := s.DB.First(&a, req.AppealID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "申诉不存在"})
		return
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var all []EvidenceItem
		tx.Where("penalty_id = ?", a.PenaltyID).Find(&all)
		return s.newVersion(tx, &a, VersionCorrection, req.Note, 0, "平台", all)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "平台更正失败"})
		return
	}
	s.notify(a.StoreID, "appeal_corrected",
		fmt.Sprintf("平台对处罚 #%d 的申诉材料作出更正（版本 v%d），原版本保留", a.PenaltyID, a.CurrentVersion))
	c.JSON(http.StatusOK, gin.H{"version": a.CurrentVersion})
}

// ---------- 本地模拟平台 ----------

// simulateResult 提交后由“平台”在短暂延迟后给出确定性结论（便于演示与测试）
func (s *Server) simulateResult(appealID uint) {
	if s.SimulateDelay > 0 {
		time.Sleep(s.SimulateDelay)
	}
	var a Appeal
	if err := s.DB.First(&a, appealID).Error; err != nil || a.Status != AppealSubmitted {
		return
	}
	var p Penalty
	s.DB.First(&p, a.PenaltyID)
	outcomes := []string{OutcomeUpheld, OutcomeRevoked, OutcomePartial}
	outcome := outcomes[appealID%3]
	var adj *float64
	if outcome == OutcomePartial {
		half := p.Amount / 2
		adj = &half
	}
	receipt := receiptPayload{
		ReceiptID:      fmt.Sprintf("RCPT-%d-%d", appealID, s.Now().UnixNano()),
		AppealID:       appealID,
		Outcome:        outcome,
		AdjustedAmount: adj,
		Reason:         "平台审核完成：" + outcomeLabel(outcome),
	}
	_, _, _ = s.processReceipt(receipt)
}

// devSimulateResult 前端“模拟平台”面板：立即生成一次平台结论
func (s *Server) devSimulateResult(c *gin.Context) {
	u := currentUser(c)
	a, ok := s.scopedAppeal(c, u)
	if !ok {
		return
	}
	if a.Status != AppealSubmitted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "申诉不在处理中"})
		return
	}
	s.SimulateDelay = 0
	s.simulateResult(a.ID)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// devRedeliverReceipt 重新送达该申诉最近一次回执，验证幂等（不会产生第二个结论）
func (s *Server) devRedeliverReceipt(c *gin.Context) {
	u := currentUser(c)
	a, ok := s.scopedAppeal(c, u)
	if !ok {
		return
	}
	var rec PlatformReceipt
	if err := s.DB.Where("appeal_id = ?", a.ID).Order("id desc").First(&rec).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "该申诉还没有平台回执"})
		return
	}
	var p receiptPayload
	if err := json.Unmarshal([]byte(rec.Payload), &p); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "回执解析失败"})
		return
	}
	result, dup, err := s.processReceipt(p)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"duplicate": dup, "receipt_id": result.ReceiptID, "outcome": result.Outcome,
		"message": "重复回执已被幂等处理，未产生新结论",
	})
}

func ensureDir(dir string) error { return os.MkdirAll(dir, 0o755) }
