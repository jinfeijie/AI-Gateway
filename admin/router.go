package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"ai-gateway/health"
	"ai-gateway/model"
	"ai-gateway/proxy"
	"ai-gateway/store"

	"github.com/gin-gonic/gin"
)

// BalancerQuerier 查询负载均衡运行时状态的接口（避免循环导入 proxy 包）
type BalancerQuerier interface {
	CooldownInfo() map[string]time.Time
	SessionInfo() []any
	RequestLogs() []any
	RequestLogDetail(idx int) *proxy.RequestLog
	DailyStats(pricing []model.ModelPricing) any // 聚合统计
	LoadInfo() map[string]int64               // 上游当前并发数
}

// sessionToken 根据用户名密码生成确定性 token，重启后仍然有效
func sessionToken(user, pass string) string {
	mac := hmac.New(sha256.New, []byte("ai-gateway-session"))
	mac.Write([]byte(user + ":" + pass))
	return hex.EncodeToString(mac.Sum(nil))
}

func Register(r *gin.RouterGroup, s *store.Store, checker *health.Checker, bq BalancerQuerier) {
	// 认证中间件（跳过 login）
	r.Use(func(c *gin.Context) {
		if strings.HasSuffix(c.FullPath(), "/login") {
			c.Next()
			return
		}

		cfg := s.Get()

		// 方式1: Authorization header (API 调用)
		if cfg.AdminToken != "" {
			auth := c.GetHeader("Authorization")
			if auth == "Bearer "+cfg.AdminToken {
				c.Next()
				return
			}
		}

		// 方式2: Cookie session (浏览器)
		if token, err := c.Cookie("session"); err == nil {
			if token == sessionToken(cfg.AdminUser, cfg.AdminPassword) {
				c.Next()
				return
			}
		}

		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		c.Abort()
	})

	// 登录接口
	r.POST("/login", func(c *gin.Context) {
		var req struct {
			Username string `json:"username" binding:"required"`
			Password string `json:"password" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		cfg := s.Get()
		if req.Username != cfg.AdminUser || req.Password != cfg.AdminPassword {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		token := sessionToken(req.Username, req.Password)
		c.SetCookie("session", token, 86400, "/", "", false, true)
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})

	registerGroupRoutes(r, s)
	registerUpstreamRoutes(r, s, bq)
	registerHealthCheckRoutes(r, s, checker)
	registerErrorMappingRoutes(r, s)
}
