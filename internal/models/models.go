package models

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"
)

// 角色
const (
	RoleManager = "manager" // 店长：可正式提交 / 补件 / 撤回
	RoleStaff   = "staff"   // 普通员工：只能整理证据
)

// 处罚类型
const (
	PenaltyTimeout     = "timeout"      // 超时
	PenaltyMissingItem = "missing_item" // 缺品
	PenaltyRejected    = "rejected"     // 拒单
)

// 证据槽位
const (
	SlotMealReady   = "meal_ready"   // 出餐
	SlotPacked      = "packed"       // 打包
	SlotRiderPickup = "rider_pickup" // 骑手取餐
	SlotOther       = "other"        // 其他材料
)

// 处罚申诉状态
const (
	StatusPending   = "pending"   // 待申诉（期限内）
	StatusSubmitted = "submitted" // 已提交，等待平台结论
	StatusWithdrawn = "withdrawn" // 已撤回（期限内可重新提交）
	StatusExpired   = "expired"   // 超期未申诉
	StatusClosed    = "closed"    // 平台已给出最终结论
)

// 申诉动作链类型（只追加，不改写历史）
const (
	ActionSubmit     = "submit"     // 店长正式提交
	ActionSupplement = "supplement" // 补件
	ActionWithdraw   = "withdraw"   // 撤回
	ActionResult     = "result"     // 平台首次结论
	ActionCorrection = "correction" // 平台更正
)

// 平台结论
const (
	DecisionUpheld   = "upheld"   // 维持处罚
	DecisionRevoked  = "revoked"  // 撤销
	DecisionAdjusted = "adjusted" // 部分调整
)

const (
	ReceiptResult     = "result"     // 首次处理结果回执
	ReceiptCorrection = "correction" // 更正回执
)

// RequiredSlots 返回某类处罚申诉所需的证据槽位。
func RequiredSlots(penaltyType string) []string {
	switch penaltyType {
	case PenaltyTimeout:
		return []string{SlotMealReady, SlotPacked, SlotRiderPickup}
	case PenaltyMissingItem:
		return []string{SlotPacked}
	case PenaltyRejected:
		// 拒单争议没有强制的出/取餐证据，可上传其他接单截图
		return nil
	default:
		return nil
	}
}

func PenaltyTypeLabel(t string) string {
	return map[string]string{
		PenaltyTimeout:     "配送超时",
		PenaltyMissingItem: "商品缺品",
		PenaltyRejected:    "拒绝接单",
	}[t]
}

func SlotLabel(s string) string {
	return map[string]string{
		SlotMealReady:   "出餐凭证",
		SlotPacked:      "打包凭证",
		SlotRiderPickup: "骑手取餐凭证",
		SlotOther:       "其他材料",
	}[s]
}

func StatusLabel(s string) string {
	return map[string]string{
		StatusPending:   "待申诉",
		StatusSubmitted: "审核中",
		StatusWithdrawn: "已撤回",
		StatusExpired:   "已超期",
		StatusClosed:    "已结案",
	}[s]
}

func DecisionLabel(d string) string {
	return map[string]string{
		DecisionUpheld:   "维持处罚",
		DecisionRevoked:  "撤销处罚",
		DecisionAdjusted: "部分调整",
	}[d]
}

// JSONInts 在 appeal_actions.snapshot_ids 中以 JSON 数组持久化证据版本 ID。
type JSONInts []uint

func (s JSONInts) Value() (driver.Value, error) {
	if len(s) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(s)
	return string(b), err
}

func (s *JSONInts) Scan(src any) error {
	if src == nil {
		*s = nil
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	default:
		return errors.New("unsupported snapshot type")
	}
	if len(b) == 0 {
		*s = nil
		return nil
	}
	return json.Unmarshal(b, s)
}

type Store struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"not null" json:"name"`
	Code      string    `gorm:"uniqueIndex;not null" json:"code"`
	CreatedAt time.Time `json:"created_at"`
}

type User struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	StoreID      uint      `gorm:"index;not null" json:"store_id"`
	Store        *Store    `gorm:"foreignKey:StoreID" json:"store,omitempty"`
	Username     string    `gorm:"uniqueIndex;not null" json:"username"`
	PasswordHash string    `gorm:"not null" json:"-"`
	Role         string    `gorm:"not null" json:"role"`
	DisplayName  string    `json:"display_name"`
	CreatedAt    time.Time `json:"created_at"`
}

// Order 外卖订单。
type Order struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	StoreID         uint      `gorm:"index;not null" json:"store_id"`
	PlatformOrderNo string    `gorm:"not null" json:"platform_order_no"`
	CustomerNote    string    `json:"customer_note"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
}

// TimelineEvent 订单时间线（下单/确认/出餐/打包/骑手取餐/送达等）。
type TimelineEvent struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	OrderID    uint      `gorm:"index;not null" json:"order_id"`
	Kind       string    `gorm:"not null" json:"kind"`
	Label      string    `json:"label"`
	OccurredAt time.Time `gorm:"not null" json:"occurred_at"`
	Note       string    `json:"note"`
	SortOrder  int       `json:"sort_order"`
}

// Penalty 一条处罚记录；同一订单可有多条，各自独立计算期限。
type Penalty struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	StoreID     uint       `gorm:"index;not null" json:"store_id"`
	OrderID     uint       `gorm:"index;not null" json:"order_id"`
	Order       *Order     `gorm:"foreignKey:OrderID" json:"order,omitempty"`
	PlatformRef string     `gorm:"not null" json:"platform_ref"`
	Type        string     `gorm:"not null" json:"type"`
	AmountCents int64      `json:"amount_cents"`
	Reason      string     `json:"reason"`
	IssuedAt    time.Time  `gorm:"not null" json:"issued_at"`
	DeadlineAt  time.Time  `gorm:"not null;index" json:"deadline_at"`
	Status      string     `gorm:"not null;index;default:pending" json:"status"`
	SubmittedAt *time.Time `json:"submitted_at"`
	WithdrawnAt *time.Time `json:"withdrawn_at"`
	ExpiredAt   *time.Time `json:"expired_at"`
	// 当前生效结论（最新回执），历史回执与动作永久保留
	CurrentDecision string     `gorm:"default:''" json:"current_decision"`
	AdjustedCents   int64      `json:"adjusted_cents"`
	FinalizedAt     *time.Time `json:"finalized_at"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// EvidenceVersion 证据版本。一经随动作归档（appeal_action_id 非空）即不可修改、删除，
// 补件只会追加新版本并通过 supersedes_id 串起同槽位的版本链。
type EvidenceVersion struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	StoreID        uint      `gorm:"index;not null" json:"store_id"`
	OrderID        uint      `gorm:"index;not null" json:"order_id"`
	PenaltyID      uint      `gorm:"index;not null" json:"penalty_id"`
	AppealActionID *uint     `gorm:"index" json:"appeal_action_id"`
	Slot           string    `gorm:"not null" json:"slot"`
	Kind           string    `gorm:"not null" json:"kind"` // image / video / doc / note
	OriginalName   string    `json:"original_name"`
	StoredPath     string    `json:"-"`
	MimeType       string    `json:"mime_type"`
	SizeBytes      int64     `json:"size_bytes"`
	Note           string    `json:"note"`
	UploaderID     uint      `json:"uploader_id"`
	UploaderName   string    `json:"uploader_name"`
	SupersedesID   *uint     `json:"supersedes_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// AppealAction 申诉动作链中的一环。
type AppealAction struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	PenaltyID   uint      `gorm:"index;not null" json:"penalty_id"`
	StoreID     uint      `gorm:"index;not null" json:"store_id"`
	Kind        string    `gorm:"not null" json:"kind"`
	ActorID     *uint     `json:"actor_id"` // 平台动作为 nil
	ActorName   string    `json:"actor_name"`
	Note        string    `json:"note"`
	SnapshotIDs JSONInts  `gorm:"column:snapshot_ids;type:TEXT" json:"snapshot_ids"`
	ReceiptID   *uint     `gorm:"index" json:"receipt_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// PlatformReceipt 平台回执。receipt_key 为平台幂等键，加唯一索引：
// 同一回执重复送达只更新送达计数，绝不产生第二条结论。
type PlatformReceipt struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	StoreID       uint      `gorm:"index;not null" json:"store_id"`
	PenaltyID     uint      `gorm:"index;not null" json:"penalty_id"`
	ReceiptKey    string    `gorm:"uniqueIndex;not null" json:"receipt_key"`
	Kind          string    `gorm:"not null" json:"kind"` // result / correction
	Decision      string    `gorm:"not null" json:"decision"`
	AdjustedCents int64     `json:"adjusted_cents"`
	Message       string    `json:"message"`
	RawPayload    string    `gorm:"type:TEXT" json:"raw_payload"`
	ReceivedAt    time.Time `gorm:"not null" json:"received_at"`
	ProcessedAt   time.Time `gorm:"not null" json:"processed_at"`
	LastSeenAt    time.Time `gorm:"not null" json:"last_seen_at"`
	DeliveryCount int       `gorm:"not null;default:1" json:"delivery_count"`
	SupersedesID  *uint     `json:"supersedes_receipt_id"`
}

// Notification 门店内通知（结论送达 / 超期提醒等）。
type Notification struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Category  string    `gorm:"not null" json:"category"`
	RefKey    string    `gorm:"not null;uniqueIndex:idx_notif_store_ref,priority:2" json:"ref_key"` // 同店同 ref 只通知一次
	StoreID   uint      `gorm:"index;uniqueIndex:idx_notif_store_ref,priority:1" json:"store_id"`
	Title     string    `gorm:"not null" json:"title"`
	Body      string    `json:"body"`
	Read      bool      `gorm:"not null;default:false" json:"read"`
	CreatedAt time.Time `json:"created_at"`
}
