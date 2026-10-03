package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"appealsystem/internal/app"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := env("ADDR", ":8080")
	dbPath := env("DB_PATH", "data/app.db")
	uploadDir := env("UPLOAD_DIR", "uploads")
	webDir := env("WEB_DIR", "web")

	for _, d := range []string{"data", uploadDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatal(err)
		}
	}

	db, err := app.OpenDB(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := app.Seed(db, time.Now(), uploadDir); err != nil {
		log.Fatal(err)
	}

	s := app.NewServer(db, uploadDir, webDir)

	log.Printf("外卖商户申诉与证据归档系统已启动: http://localhost%s", addr)
	log.Printf("演示账号：店长 manager1/123456（文一店）、员工 staff1/123456；文二店 manager2、staff2")
	log.Fatal(http.ListenAndServe(addr, s.Handler()))
}
