package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"

	"appeal/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Session 极简内存会话：token -> 用户快照。重启需重新登录（演示项目足够）。
type Session struct {
	UserID  uint
	StoreID uint
	Role    string
	Name    string
}

type Auth struct {
	mu     sync.RWMutex
	tokens map[string]Session
}

func NewAuth() *Auth { return &Auth{tokens: map[string]Session{}} }

func (a *Auth) Issue(s Session) string {
	tok := randomToken()
	a.mu.Lock()
	a.tokens[tok] = s
	a.mu.Unlock()
	return tok
}

func (a *Auth) Revoke(tok string) {
	a.mu.Lock()
	delete(a.tokens, tok)
	a.mu.Unlock()
}

func (a *Auth) get(tok string) (Session, bool) {
	a.mu.RLock()
	s, ok := a.tokens[tok]
	a.mu.RUnlock()
	return s, ok
}

func extractToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if tok, err := c.Cookie("session"); err == nil {
		return tok
	}
	return ""
}

// RequireAuth 解析会话，并保证用户仍属于该门店（账号被删后立即失效）。
func (a *Auth) RequireAuth(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		tok := extractToken(c)
		if tok == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized", "message": "请先登录"})
			return
		}
		sess, ok := a.get(tok)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized", "message": "登录已失效，请重新登录"})
			return
		}
		var u models.User
		if err := db.First(&u, sess.UserID).Error; err != nil || u.StoreID != sess.StoreID {
			a.Revoke(tok)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized", "message": "账号不可用"})
			return
		}
		c.Set("session", sess)
		c.Set("token", tok)
		c.Next()
	}
}

func currentSession(c *gin.Context) Session {
	v, _ := c.Get("session")
	return v.(Session)
}

// requireManager 正式提交/补件/撤回仅店长可用；员工整理证据不受此限制。
func requireManager(c *gin.Context) bool {
	sess := currentSession(c)
	if sess.Role != models.RoleManager {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "forbidden", "message": "只有店长可以执行正式提交、补件与撤回，请联系店长操作",
		})
		return false
	}
	return true
}
