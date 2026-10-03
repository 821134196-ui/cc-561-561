package store

import (
	"bytes"
	"encoding/base64"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"appeal/internal/models"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open 打开/创建 SQLite 数据库并执行迁移。
func Open(dsn string) (*gorm.DB, error) {
	// pragma 写入 DSN，保证连接池中的每条连接都生效（WAL 单写多读 + 写锁等待）。
	if !strings.Contains(dsn, "_busy_timeout") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "_journal_mode=WAL&_busy_timeout=8000&_foreign_keys=on&_txlock=immediate"
	}
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.New(
			log.New(os.Stderr, "\r", log.LstdFlags),
			logger.Config{SlowThreshold: 200 * time.Millisecond, LogLevel: logger.Warn, IgnoreRecordNotFoundError: true},
		),
	})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(
		&models.Store{}, &models.User{}, &models.Order{}, &models.TimelineEvent{},
		&models.Penalty{}, &models.EvidenceVersion{}, &models.AppealAction{},
		&models.PlatformReceipt{}, &models.Notification{},
	); err != nil {
		return nil, err
	}
	// SQLite 默认隔离较弱，开启外键约束；WAL + busy_timeout 让并发写等待而非立即报锁。
	if err := db.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		return nil, err
	}
	if err := db.Exec("PRAGMA journal_mode = WAL").Error; err != nil {
		return nil, err
	}
	if err := db.Exec("PRAGMA busy_timeout = 8000").Error; err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err == nil {
		sqlDB.SetMaxOpenConns(8)
	}
	return db, nil
}

// Seed 在空库中写入演示数据（两个门店、各一名店长/员工、含多种处罚状态）。
func Seed(db *gorm.DB, uploadDir string) error {
	var n int64
	db.Model(&models.Store{}).Count(&n)
	if n > 0 {
		return nil
	}

	hash := func(pwd string) string {
		h, _ := bcrypt.GenerateFromPassword([]byte(pwd), bcrypt.MinCost)
		return string(h)
	}
	now := time.Now()

	stores := []models.Store{
		{Name: "望京一店", Code: "A001"},
		{Name: "国贸二店", Code: "B002"},
	}
	if err := db.Create(&stores).Error; err != nil {
		return err
	}
	storeA, storeB := stores[0].ID, stores[1].ID

	users := []models.User{
		{StoreID: storeA, Username: "wang", PasswordHash: hash("123456"), Role: models.RoleManager, DisplayName: "王店长"},
		{StoreID: storeA, Username: "zhang", PasswordHash: hash("123456"), Role: models.RoleStaff, DisplayName: "张员工"},
		{StoreID: storeB, Username: "li", PasswordHash: hash("123456"), Role: models.RoleManager, DisplayName: "李店长"},
		{StoreID: storeB, Username: "chen", PasswordHash: hash("123456"), Role: models.RoleStaff, DisplayName: "陈员工"},
	}
	if err := db.Create(&users).Error; err != nil {
		return err
	}
	wangID, zhangID := users[0].ID, users[1].ID
	liID, chenID := users[2].ID, users[3].ID

	// 1x1 像素 PNG，作为模拟截图附件。
	png1x1, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")
	writeSeedFile := func(rel, name string) (string, int64) {
		dir := filepath.Join(uploadDir, rel)
		_ = os.MkdirAll(dir, 0o755)
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, png1x1, 0o644)
		abs, _ := filepath.Abs(p)
		return abs, int64(len(png1x1))
	}

	mkOrder := func(storeID uint, no, note string, minutesAgo int, events [][3]string) models.Order {
		o := models.Order{
			StoreID:         storeID,
			PlatformOrderNo: no,
			CustomerNote:    note,
			Status:          "已完成",
			CreatedAt:       now.Add(-time.Duration(minutesAgo) * time.Minute),
		}
		db.Create(&o)
		for i, ev := range events {
			t, _ := time.ParseDuration(ev[2])
			db.Create(&models.TimelineEvent{
				OrderID:    o.ID,
				Kind:       ev[0],
				Label:      ev[1],
				OccurredAt: now.Add(-t),
				SortOrder:  i,
			})
		}
		return o
	}

	tl := func(base [][3]string) [][3]string { return base }
	// ev: kind, label, 距现在时长
	order1Events := tl([][3]string{
		{"created", "用户下单", "26h"},
		{"accepted", "门店接单", "25h50m"},
		{"meal_ready", "后厨出餐", "25h20m"},
		{"packed", "打包完成", "25h15m"},
		{"rider_pickup", "骑手取餐", "25h10m"},
		{"delivered", "用户送达", "24h30m"},
	})
	order2Events := tl([][3]string{
		{"created", "用户下单", "70h"},
		{"accepted", "门店接单", "69h55m"},
		{"packed", "打包完成", "69h40m"},
		{"delivered", "用户送达", "69h"},
	})
	order3Events := tl([][3]string{
		{"created", "用户下单", "12h"},
		{"accepted", "门店接单", "11h55m"},
		{"rejected_mark", "平台标记拒单", "11h50m"},
	})

	o1 := mkOrder(storeA, "MT2026100201", "不要辣，两双筷子", 26*60, order1Events)
	o2 := mkOrder(storeA, "MT2026093008", "少冰", 70*60, order2Events)
	o3 := mkOrder(storeA, "MT2026100307", "", 12*60, order3Events)
	o4 := mkOrder(storeB, "MT2026100209", "B 店订单", 30*60, order1Events)

	ptrT := func(t time.Time) *time.Time { return &t }

	// A 店：同一订单 o1 上挂两条处罚，期限各自独立计算
	p1 := models.Penalty{StoreID: storeA, OrderID: o1.ID, PlatformRef: "P-A-1001", Type: models.PenaltyTimeout,
		AmountCents: 2000, Reason: "订单配送超时 18 分钟", IssuedAt: now.Add(-24 * time.Hour),
		DeadlineAt: now.Add(23 * time.Hour), Status: models.StatusPending}
	p2 := models.Penalty{StoreID: storeA, OrderID: o1.ID, PlatformRef: "P-A-1002", Type: models.PenaltyMissingItem,
		AmountCents: 1500, Reason: "用户反馈缺少小吃一份", IssuedAt: now.Add(-24 * time.Hour),
		DeadlineAt: now.Add(2 * time.Hour), Status: models.StatusPending}
	// A 店：已超期
	p3 := models.Penalty{StoreID: storeA, OrderID: o3.ID, PlatformRef: "P-A-1003", Type: models.PenaltyRejected,
		AmountCents: 500, Reason: "系统判定门店拒绝接单", IssuedAt: now.Add(-10 * time.Hour),
		DeadlineAt: now.Add(-2 * time.Hour), Status: models.StatusExpired, ExpiredAt: ptrT(now.Add(-2 * time.Hour))}
	// A 店：已提交待结论（mock 平台可推送回执）
	p4 := models.Penalty{StoreID: storeA, OrderID: o2.ID, PlatformRef: "P-A-1004", Type: models.PenaltyMissingItem,
		AmountCents: 1200, Reason: "用户反馈漏放餐具", IssuedAt: now.Add(-48 * time.Hour),
		DeadlineAt: now.Add(-24 * time.Hour), Status: models.StatusSubmitted, SubmittedAt: ptrT(now.Add(-30 * time.Hour))}
	// A 店：已结案-撤销
	p5 := models.Penalty{StoreID: storeA, OrderID: o2.ID, PlatformRef: "P-A-1005", Type: models.PenaltyTimeout,
		AmountCents: 800, Reason: "申诉演练-撤销", IssuedAt: now.Add(-72 * time.Hour),
		DeadlineAt: now.Add(-48 * time.Hour), Status: models.StatusClosed,
		CurrentDecision: models.DecisionRevoked, FinalizedAt: ptrT(now.Add(-20 * time.Hour))}
	// A 店：期限内已撤回，可重新提交
	p6 := models.Penalty{StoreID: storeA, OrderID: o3.ID, PlatformRef: "P-A-1006", Type: models.PenaltyRejected,
		AmountCents: 500, Reason: "误操作撤回演示", IssuedAt: now.Add(-3 * time.Hour),
		DeadlineAt: now.Add(21 * time.Hour), Status: models.StatusWithdrawn, WithdrawnAt: ptrT(now.Add(-1 * time.Hour))}
	// B 店：待申诉，用于验证跨店不可见
	p7 := models.Penalty{StoreID: storeB, OrderID: o4.ID, PlatformRef: "P-B-2001", Type: models.PenaltyTimeout,
		AmountCents: 3000, Reason: "B 店超时处罚", IssuedAt: now.Add(-26 * time.Hour),
		DeadlineAt: now.Add(22 * time.Hour), Status: models.StatusPending}
	for _, p := range []*models.Penalty{&p1, &p2, &p3, &p4, &p5, &p6, &p7} {
		if err := db.Create(p).Error; err != nil {
			return err
		}
	}

	// 为已提交/已结案的处罚预置归档证据与动作链
	mkEvidence := func(p *models.Penalty, slot, name string, uploaderID uint, uploader string, actionID *uint) {
		path, size := writeSeedFile("seed", name)
		db.Create(&models.EvidenceVersion{
			StoreID: p.StoreID, OrderID: p.OrderID, PenaltyID: p.ID,
			AppealActionID: actionID, Slot: slot, Kind: "image", OriginalName: name,
			StoredPath: path, MimeType: "image/png", SizeBytes: size,
			Note: "模拟平台截图", UploaderID: uploaderID, UploaderName: uploader,
		})
	}
	act4 := models.AppealAction{PenaltyID: p4.ID, StoreID: storeA, Kind: models.ActionSubmit,
		ActorID: &wangID, ActorName: "王店长", Note: "餐具已随餐放置，附监控截图"}
	db.Create(&act4)
	mkEvidence(&p4, models.SlotPacked, "packed_p4.png", wangID, "王店长", &act4.ID)
	var v4 models.EvidenceVersion
	db.Where("penalty_id = ?", p4.ID).First(&v4)
	act4.SnapshotIDs = models.JSONInts{v4.ID}
	db.Save(&act4)

	r5 := models.PlatformReceipt{StoreID: storeA, PenaltyID: p5.ID, ReceiptKey: "RCP-5005",
		Kind: models.ReceiptResult, Decision: models.DecisionRevoked, Message: "证据充分，撤销处罚",
		RawPayload: `{"receipt_key":"RCP-5005","decision":"revoked"}`,
		ReceivedAt: now.Add(-20 * time.Hour), ProcessedAt: now.Add(-20 * time.Hour), LastSeenAt: now.Add(-20 * time.Hour)}
	db.Create(&r5)
	db.Create(&models.AppealAction{PenaltyID: p5.ID, StoreID: storeA, Kind: models.ActionSubmit,
		ActorID: &wangID, ActorName: "王店长", CreatedAt: now.Add(-26 * time.Hour)})
	db.Create(&models.AppealAction{PenaltyID: p5.ID, StoreID: storeA, Kind: models.ActionResult,
		ActorName: "平台", Note: "证据充分，撤销处罚", ReceiptID: &r5.ID})
	db.Create(&models.Notification{StoreID: storeA, Category: "result", RefKey: "RCP-5005",
		Title: "申诉结果：撤销处罚", Body: "订单 MT2026093008 的超时处罚已撤销"})

	act6 := models.AppealAction{PenaltyID: p6.ID, StoreID: storeA, Kind: models.ActionWithdraw,
		ActorID: &wangID, ActorName: "王店长", Note: "先撤回，补齐骑手凭证再提交",
		CreatedAt: now.Add(-1 * time.Hour)}
	db.Create(&act6)

	// 未读通知给 A 店演示用（其余账号保持干净）
	_ = zhangID
	_ = liID
	_ = chenID
	if !bytes.Contains(png1x1, []byte("PNG")) {
		return os.ErrInvalid
	}
	return nil
}
