package main

import (
	"embed"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"appeal/internal/appeal"
	"appeal/internal/httpapi"
	"appeal/internal/store"
)

//go:embed all:web
var webFS embed.FS

func main() {
	addr := envOr("ADDR", ":8080")
	dataDir := envOr("DATA_DIR", "./data")
	flag.StringVar(&addr, "addr", addr, "监听地址")
	flag.StringVar(&dataDir, "data", dataDir, "数据目录（SQLite 与附件）")
	flag.Parse()

	if err := os.MkdirAll(filepath.Join(dataDir, "uploads"), 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	dbPath := filepath.Join(dataDir, "appeal.db")
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	if err := store.Seed(db, filepath.Join(dataDir, "uploads")); err != nil {
		log.Fatalf("初始化演示数据失败: %v", err)
	}

	svc := appeal.NewService(db, filepath.Join(dataDir, "uploads"))
	httpapi.StartSweeper(svc)

	r := httpapi.NewRouter(db, svc, webFS, filepath.Join(dataDir, "uploads"))
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("外卖申诉归档系统已启动: http://localhost%s", addr)
	log.Printf("演示账号: 王店长 wang/123456 | 张员工 zhang/123456（A店）；李店长 li/123456 | 陈员工 chen/123456（B店）")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
