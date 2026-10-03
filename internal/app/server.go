package app

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Server 聚合依赖；Now 可注入以便测试期限边界
type Server struct {
	DB               *gorm.DB
	Now              func() time.Time
	UploadDir        string
	WebDir           string
	SimulatePlatform bool          // 提交后由本地模拟平台自动给出结论
	SimulateDelay    time.Duration // 模拟平台回执延迟

	tokens map[string]uint
	mu     sync.Mutex

	engine *gin.Engine
}

func NewServer(db *gorm.DB, uploadDir, webDir string) *Server {
	s := &Server{
		DB:               db,
		Now:              time.Now,
		UploadDir:        uploadDir,
		WebDir:           webDir,
		SimulatePlatform: true,
		SimulateDelay:    3 * time.Second,
		tokens:           map[string]uint{},
	}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	api := r.Group("/api")
	api.POST("/login", s.login)

	// 模拟平台回调（回执、平台更正）
	pf := api.Group("/platform")
	pf.POST("/receipts", s.postReceipt)
	pf.POST("/corrections", s.postCorrection)

	auth := api.Group("")
	auth.Use(s.authRequired)
	auth.GET("/me", s.me)
	auth.GET("/penalties", s.listPenalties)
	auth.GET("/penalties/:id", s.getPenalty)
	auth.POST("/penalties/:id/evidence", s.uploadEvidence)
	auth.POST("/penalties/:id/submit", s.submitAppeal)
	auth.GET("/appeals/:id", s.getAppeal)
	auth.POST("/appeals/:id/supplement", s.supplementAppeal)
	auth.POST("/appeals/:id/withdraw", s.withdrawAppeal)
	auth.GET("/files/:id", s.downloadFile)
	auth.GET("/notifications", s.listNotifications)
	auth.POST("/notifications/read-all", s.readAllNotifications)

	// 本地模拟平台的操作入口（供前端“模拟平台”面板触发）
	dev := api.Group("/dev")
	dev.Use(s.authRequired)
	dev.POST("/appeals/:id/simulate-result", s.devSimulateResult)
	dev.POST("/appeals/:id/redeliver-receipt", s.devRedeliverReceipt)

	// 前端静态资源
	r.Static("/assets", webDir)
	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method == http.MethodGet {
			c.File(webDir + "/index.html")
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	})

	s.engine = r
	return s
}

func (s *Server) Handler() http.Handler { return s.engine }

func (s *Server) me(c *gin.Context) {
	u := currentUser(c)
	var store Store
	s.DB.First(&store, u.StoreID)
	c.JSON(http.StatusOK, gin.H{
		"id": u.ID, "username": u.Username, "name": u.Name,
		"role": u.Role, "store_id": u.StoreID, "store_name": store.Name,
	})
}
