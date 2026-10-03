package app

import "time"

// ---------- 常量 ----------

const (
	RoleStaff   = "staff"   // 普通员工：整理证据
	RoleManager = "manager" // 店长：正式提交/补件/撤回
)

const (
	PenaltyTimeout     = "timeout"      // 超时
	PenaltyMissingItem = "missing_item" // 缺品
	PenaltyRejected    = "rejected"     // 拒单
)

// 处罚状态（expired 为派生状态，不落库）
const (
	PenaltyPending   = "pending"
	PenaltySubmitted = "submitted"
	PenaltyWithdrawn = "withdrawn"
	PenaltyUpheld    = "upheld"  // 维持处罚
	PenaltyRevoked   = "revoked" // 撤销
	PenaltyPartial   = "partial" // 部分调整
)

const (
	AppealSubmitted = "submitted"
	AppealWithdrawn = "withdrawn"
	AppealDecided   = "decided"
)

// 证据版本类型：提交 / 补件 / 撤回 / 平台更正
const (
	VersionSubmission = "submission"
	VersionSupplement = "supplement"
	VersionWithdrawal = "withdrawal"
	VersionCorrection = "correction"
)

// 证据类别
const (
	CatMealReady   = "meal_ready"   // 出餐
	CatPacked      = "packed"       // 打包
	CatRiderPickup = "rider_pickup" // 骑手取餐
	CatOther       = "other"
)

// 平台结论
const (
	OutcomeUpheld  = "upheld"
	OutcomeRevoked = "revoked"
	OutcomePartial = "partial"
)

// AppealWindow 每条处罚独立的申诉期限（自下达时刻起算）
const AppealWindow = 72 * time.Hour

// CategoryLabel 证据类别中文名
func CategoryLabel(c string) string {
	switch c {
	case CatMealReady:
		return "出餐"
	case CatPacked:
		return "打包"
	case CatRiderPickup:
		return "骑手取餐"
	default:
		return "其他"
	}
}

// RequiredCategories 该处罚类型需要的关键证据类别
func RequiredCategories(penaltyType string) []string {
	switch penaltyType {
	case PenaltyMissingItem:
		return []string{CatMealReady, CatPacked}
	default: // timeout / rejected 都需要完整出餐-打包-取餐链路
		return []string{CatMealReady, CatPacked, CatRiderPickup}
	}
}

// ---------- 模型 ----------

type Store struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Name string `json:"name"`
}

type User struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	StoreID  uint   `gorm:"index" json:"store_id"`
	Username string `gorm:"uniqueIndex" json:"username"`
	Password string `json:"-"` // 本地演示，明文存储
	Role     string `json:"role"`
	Name     string `json:"name"`
}

type Order struct {
	ID       uint      `gorm:"primaryKey" json:"id"`
	StoreID  uint      `gorm:"index" json:"store_id"`
	OrderNo  string    `gorm:"index" json:"order_no"`
	Amount   float64   `json:"amount"`
	PlacedAt time.Time `json:"placed_at"`
}

// OrderEvent 订单时间线事件，Type 与证据类别同义（meal_ready/packed/rider_pickup/...）
type OrderEvent struct {
	ID      uint      `gorm:"primaryKey" json:"id"`
	OrderID uint      `gorm:"index" json:"order_id"`
	Type    string    `json:"type"`
	At      time.Time `json:"at"`
	Note    string    `json:"note"`
}

type Penalty struct {
	ID       uint      `gorm:"primaryKey" json:"id"`
	StoreID  uint      `gorm:"index" json:"store_id"`
	OrderID  uint      `gorm:"index" json:"order_id"`
	Type     string    `json:"type"`
	Amount   float64   `json:"amount"`
	Reason   string    `json:"reason"`
	IssuedAt time.Time `json:"issued_at"`
	Deadline time.Time `json:"deadline"` // 每条处罚单独计算：IssuedAt + AppealWindow
	Status   string    `json:"status"`
}

// EvidenceItem 证据文件，创建后不可修改、不可删除（无更新/删除接口）
type EvidenceItem struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	PenaltyID    uint      `gorm:"index" json:"penalty_id"`
	Category     string    `json:"category"`
	FileName     string    `json:"file_name"`
	StoredPath   string    `json:"-"`
	Note         string    `json:"note"`
	UploadedByID uint      `json:"uploaded_by_id"`
	UploaderName string    `json:"uploader_name"`
	CreatedAt    time.Time `json:"created_at"`
}

type Appeal struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	PenaltyID      uint       `gorm:"uniqueIndex" json:"penalty_id"`
	StoreID        uint       `gorm:"index" json:"store_id"`
	Status         string     `json:"status"`
	CurrentVersion int        `json:"current_version"`
	SubmittedByID  uint       `json:"submitted_by_id"`
	SubmittedAt    time.Time  `json:"submitted_at"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
}

// EvidenceVersion 每次提交/补件/撤回/平台更正都生成新版本，历史版本保留
type EvidenceVersion struct {
	ID            uint           `gorm:"primaryKey" json:"id"`
	AppealID      uint           `gorm:"index" json:"appeal_id"`
	VersionNo     int            `json:"version_no"`
	Type          string         `json:"type"`
	Note          string         `json:"note"`
	CreatedByID   uint           `json:"created_by_id"`
	CreatedByName string         `json:"created_by_name"`
	CreatedAt     time.Time      `json:"created_at"`
	Items         []EvidenceItem `gorm:"many2many:evidence_version_items" json:"items,omitempty"`
}

// AppealResult 平台处理结论，一个申诉只有一条；ReceiptID 唯一保证幂等
type AppealResult struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	AppealID       uint      `gorm:"uniqueIndex" json:"appeal_id"`
	ReceiptID      string    `gorm:"uniqueIndex" json:"receipt_id"`
	Outcome        string    `json:"outcome"`
	AdjustedAmount *float64  `json:"adjusted_amount,omitempty"`
	Reason         string    `json:"reason"`
	VersionNo      int       `json:"version_no"` // 结论关联的材料版本
	ReceivedAt     time.Time `json:"received_at"`
}

// PlatformReceipt 平台回执原始记录，receipt_id 唯一；重复送达时比对内容哈希
type PlatformReceipt struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	ReceiptID   string    `gorm:"uniqueIndex" json:"receipt_id"`
	AppealID    uint      `gorm:"index" json:"appeal_id"`
	PayloadHash string    `json:"payload_hash"`
	Payload     string    `json:"payload"`
	ProcessedAt time.Time `json:"processed_at"`
}

// Notification 消息接口（本地模拟）
type Notification struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	StoreID   uint      `gorm:"index" json:"store_id"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	Read      bool      `json:"read"`
	CreatedAt time.Time `json:"created_at"`
}
