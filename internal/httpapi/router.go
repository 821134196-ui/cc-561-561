package httpapi

import (
	"io/fs"
	"net/http"
	"path/filepath"
	"time"

	"appeal/internal/appeal"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// NewRouter 组装全部路由。webFS 为嵌入的前端静态资源；uploadsDir 为附件目录。
func NewRouter(db *gorm.DB, svc *appeal.Service, webFS fs.FS, uploadsDir string) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Next()
	})

	h := New(db, svc)

	api := r.Group("/api")
	api.POST("/auth/login", h.Login)

	authed := api.Group("")
	authed.Use(h.Auth.RequireAuth(db))
	{
		authed.POST("/auth/logout", h.Logout)
		authed.GET("/me", h.Me)
		authed.GET("/evidence/:vid/download", h.Download)

		s := authed.Group("/stores/:storeId")
		{
			s.GET("/dashboard", h.Dashboard)
			s.GET("/penalties", h.ListPenalties)
			s.GET("/penalties/:id", h.GetPenalty)

			s.POST("/penalties/:id/draft", h.UploadDraft)
			s.DELETE("/penalties/:id/evidence/:vid", h.DeleteEvidence)

			// 以下三者要求店长身份
			s.POST("/penalties/:id/submit", h.Submit)
			s.POST("/penalties/:id/withdraw", h.Withdraw)
			s.POST("/penalties/:id/supplement", h.Supplement)

			s.POST("/penalties/:id/receipts", h.PushReceipt)
			s.GET("/penalties/:id/receipts", h.ListReceipts)

			s.GET("/notifications", h.ListNotifications)
			s.POST("/notifications/read", h.ReadNotifications)
		}
	}

	// 注意：附件不在此暴露静态目录——下载必须经过 /api/evidence/:vid/download
	// 的登录与门店归属校验，避免跨店通过文件名猜测越权下载。

	if webFS != nil {
		spaFS, err := fs.Sub(webFS, "web")
		if err == nil {
			fileServer := http.FileServer(http.FS(spaFS))
			r.NoRoute(func(c *gin.Context) {
				path := c.Request.URL.Path
				if len(path) >= 4 && path[:4] == "/api" {
					c.JSON(404, gin.H{"error": "not_found"})
					return
				}
				// SPA 回退：资源不存在时返回 index.html
				if _, err := fs.Stat(spaFS, filepath.Clean(path)[1:]); err != nil {
					c.Request.URL.Path = "/"
				}
				fileServer.ServeHTTP(c.Writer, c.Request)
			})
		}
	}

	return r
}

// StartSweeper 每分钟把过期限的待申诉处罚落库为 expired，并产生门店通知。
func StartSweeper(svc *appeal.Service) {
	go func() {
		_, _ = svc.SweepExpired()
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			_, _ = svc.SweepExpired()
		}
	}()
}
