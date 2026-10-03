package app

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// 平台模拟接口的共享令牌（本地模拟，真实环境应为签名/回调鉴权）
const platformToken = "local-sim"

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) login(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	var u User
	if err := s.DB.Where("username = ? AND password = ?", req.Username, req.Password).First(&u).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
		return
	}
	tok := newToken()
	s.mu.Lock()
	s.tokens[tok] = u.ID
	s.mu.Unlock()
	var store Store
	s.DB.First(&store, u.StoreID)
	c.JSON(http.StatusOK, gin.H{
		"token": tok,
		"user": gin.H{
			"id": u.ID, "username": u.Username, "name": u.Name,
			"role": u.Role, "store_id": u.StoreID, "store_name": store.Name,
		},
	})
}

// authRequired 解析 Bearer Token，把当前用户放入上下文
func (s *Server) authRequired(c *gin.Context) {
	h := c.GetHeader("Authorization")
	tok := strings.TrimPrefix(h, "Bearer ")
	if tok == "" || tok == h {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	s.mu.Lock()
	uid, ok := s.tokens[tok]
	s.mu.Unlock()
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "登录已失效，请重新登录"})
		return
	}
	var u User
	if err := s.DB.First(&u, uid).Error; err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "用户不存在"})
		return
	}
	c.Set("user", &u)
	c.Next()
}

func currentUser(c *gin.Context) *User {
	v, _ := c.Get("user")
	u, _ := v.(*User)
	return u
}

// requireManager 正式操作（提交/补件/撤回）仅店长可执行
func requireManager(c *gin.Context) (*User, bool) {
	u := currentUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return nil, false
	}
	if u.Role != RoleManager {
		c.JSON(http.StatusForbidden, gin.H{"error": "正式提交必须由店长完成，普通员工只能整理证据"})
		return nil, false
	}
	return u, true
}

// platformAuth 模拟平台回调的鉴权
func platformAuth(c *gin.Context) bool {
	if c.GetHeader("X-Platform-Token") != platformToken {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "平台接口令牌无效"})
		return false
	}
	return true
}
