package app

import (
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// OpenDB 打开 SQLite 数据库并自动迁移
func OpenDB(path string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	err = db.AutoMigrate(
		&Store{}, &User{}, &Order{}, &OrderEvent{}, &Penalty{},
		&EvidenceItem{}, &Appeal{}, &EvidenceVersion{}, &AppealResult{},
		&PlatformReceipt{}, &Notification{},
	)
	if err != nil {
		return nil, err
	}
	return db, nil
}
