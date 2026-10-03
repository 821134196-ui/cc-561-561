package app

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gorm.io/gorm"
)

// Seed 写入演示数据：两个门店、店长/员工账号、订单时间线与多条处罚。
// 仅在空库时执行。uploadDir 用于为预置证据生成真实文件。
func Seed(db *gorm.DB, now time.Time, uploadDir string) error {
	var count int64
	db.Model(&Store{}).Count(&count)
	if count > 0 {
		return nil
	}

	s1 := Store{Name: "鲜丰水果（文一店）"}
	s2 := Store{Name: "鲜丰水果（文二店）"}
	db.Create(&s1)
	db.Create(&s2)

	users := []User{
		{StoreID: s1.ID, Username: "manager1", Password: "123456", Role: RoleManager, Name: "王店长"},
		{StoreID: s1.ID, Username: "staff1", Password: "123456", Role: RoleStaff, Name: "小李"},
		{StoreID: s2.ID, Username: "manager2", Password: "123456", Role: RoleManager, Name: "陈店长"},
		{StoreID: s2.ID, Username: "staff2", Password: "123456", Role: RoleStaff, Name: "小赵"},
	}
	db.Create(&users)

	// 门店1订单
	o1 := Order{StoreID: s1.ID, OrderNo: "MT20261001001", Amount: 58.5, PlacedAt: now.Add(-30 * time.Hour)}
	o2 := Order{StoreID: s1.ID, OrderNo: "MT20261002007", Amount: 32.0, PlacedAt: now.Add(-50 * time.Hour)}
	o3 := Order{StoreID: s1.ID, OrderNo: "ELM20261002003", Amount: 41.8, PlacedAt: now.Add(-90 * time.Hour)}
	db.Create(&o1)
	db.Create(&o2)
	db.Create(&o3)

	ev := func(orderID uint, typ string, at time.Time, note string) OrderEvent {
		return OrderEvent{OrderID: orderID, Type: typ, At: at, Note: note}
	}
	db.Create([]OrderEvent{
		// o1：链路完整
		ev(o1.ID, "accepted", o1.PlacedAt.Add(2*time.Minute), "商户接单"),
		ev(o1.ID, CatMealReady, o1.PlacedAt.Add(18*time.Minute), "出餐完成"),
		ev(o1.ID, CatPacked, o1.PlacedAt.Add(22*time.Minute), "打包完成"),
		ev(o1.ID, CatRiderPickup, o1.PlacedAt.Add(30*time.Minute), "骑手取餐"),
		ev(o1.ID, "delivered", o1.PlacedAt.Add(55*time.Minute), "送达"),
		// o2：缺打包、骑手取餐记录（同一订单对应两条处罚）
		ev(o2.ID, "accepted", o2.PlacedAt.Add(3*time.Minute), "商户接单"),
		ev(o2.ID, CatMealReady, o2.PlacedAt.Add(25*time.Minute), "出餐完成"),
		// o3：缺出餐记录
		ev(o3.ID, "accepted", o3.PlacedAt.Add(1*time.Minute), "商户接单"),
		ev(o3.ID, CatPacked, o3.PlacedAt.Add(20*time.Minute), "打包完成"),
		ev(o3.ID, CatRiderPickup, o3.PlacedAt.Add(28*time.Minute), "骑手取餐"),
	})

	pen := func(orderID uint, typ string, amount float64, reason string, issued time.Time) Penalty {
		return Penalty{
			StoreID: s1.ID, OrderID: orderID, Type: typ, Amount: amount, Reason: reason,
			IssuedAt: issued, Deadline: issued.Add(AppealWindow), Status: PenaltyPending,
		}
	}
	p1 := pen(o2.ID, PenaltyTimeout, 20, "订单履约超时，扣除履约分并罚款", now.Add(-24*time.Hour))
	p2 := pen(o2.ID, PenaltyMissingItem, 32, "顾客投诉缺品：少送一份饮品", now.Add(-70*time.Hour)) // 临期：约2小时后截止
	p3 := pen(o3.ID, PenaltyRejected, 50, "高峰期拒单", now.Add(-80*time.Hour))            // 已超期
	p4 := pen(o1.ID, PenaltyTimeout, 15, "配送超时 12 分钟", now.Add(-10*time.Hour))
	db.Create(&p1)
	db.Create(&p2)
	db.Create(&p3)
	db.Create(&p4)

	// p4 预置一份员工上传的出餐证据（同时在磁盘生成真实文件，保证可下载）
	seedFile := filepath.Join(uploadDir, fmt.Sprint(p4.ID), "seed_出餐小票照片.jpg")
	if err := os.MkdirAll(filepath.Dir(seedFile), 0o755); err == nil {
		_ = os.WriteFile(seedFile, []byte("出餐小票照片（演示文件）\n出餐耗时 18 分钟，未超时。\n"), 0o644)
	}
	db.Create(&EvidenceItem{
		PenaltyID: p4.ID, Category: CatMealReady, FileName: "出餐小票照片.jpg",
		StoredPath: seedFile,
		Note:       "出餐时间 18 分钟，未超时", UploadedByID: users[1].ID, UploaderName: users[1].Name,
		CreatedAt: now.Add(-9 * time.Hour),
	})

	// 门店2：用于跨门店隔离演示
	o4 := Order{StoreID: s2.ID, OrderNo: "MT20261002009", Amount: 66.0, PlacedAt: now.Add(-20 * time.Hour)}
	db.Create(&o4)
	db.Create([]OrderEvent{
		ev(o4.ID, "accepted", o4.PlacedAt.Add(2*time.Minute), "商户接单"),
		ev(o4.ID, CatMealReady, o4.PlacedAt.Add(15*time.Minute), "出餐完成"),
	})
	p5 := Penalty{
		StoreID: s2.ID, OrderID: o4.ID, Type: PenaltyTimeout, Amount: 25,
		Reason: "订单履约超时", IssuedAt: now.Add(-12 * time.Hour),
		Deadline: now.Add(-12 * time.Hour).Add(AppealWindow), Status: PenaltyPending,
	}
	db.Create(&p5)

	db.Create([]Notification{
		{StoreID: s1.ID, Type: "penalty_issued", Message: "收到新处罚：订单 MT20261002007 履约超时，罚款 20 元，请在期限内申诉", CreatedAt: now.Add(-24 * time.Hour)},
		{StoreID: s1.ID, Type: "penalty_issued", Message: "收到新处罚：订单 MT20261002007 缺品投诉，罚款 32 元，申诉即将截止", CreatedAt: now.Add(-70 * time.Hour)},
		{StoreID: s2.ID, Type: "penalty_issued", Message: "收到新处罚：订单 MT20261002009 履约超时，罚款 25 元", CreatedAt: now.Add(-12 * time.Hour)},
	})
	return nil
}
