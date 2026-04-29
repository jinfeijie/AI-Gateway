package registry

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"ai-gateway/model"
	"ai-gateway/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Register 注册注册中心路由（不走 Admin 鉴权）
func Register(r *gin.RouterGroup, s *store.Store) {
	r.Use(func(c *gin.Context) {
		cfg := s.Get()
		if cfg.RegistryToken == "" {
			c.Next()
			return
		}
		auth := c.GetHeader("Authorization")
		if auth == "Bearer "+cfg.RegistryToken {
			c.Next()
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		c.Abort()
	})

	r.POST("/register", registerHandler(s))
	r.POST("/heartbeat", heartbeatHandler(s))
	r.POST("/deregister", deregisterHandler(s))
}

func registerHandler(s *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			IP     string   `json:"ip" binding:"required"`
			Port   int      `json:"port" binding:"required"`
			APIKey string   `json:"api_key" binding:"required"`
			Group  string   `json:"group" binding:"required"`
			Models []string `json:"models"`
			Status string   `json:"status"`
			Weight *int     `json:"weight"`
			Remark string   `json:"remark"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.Status == "" {
			req.Status = "active"
		}

		apiURL := fmt.Sprintf("http://%s:%d", req.IP, req.Port)
		now := time.Now().Unix()

		var instanceID string

		if err := s.Update(func(cfg *model.Config) {
			// 查找分组：优先按 registry_key 匹配，其次按名称
			groupID := ""
			for _, g := range cfg.Groups {
				if g.RegistryKey != "" && g.RegistryKey == req.Group {
					groupID = g.ID
					break
				}
			}
			if groupID == "" {
				for _, g := range cfg.Groups {
					if g.Name == req.Group {
						groupID = g.ID
						break
					}
				}
			}
			if groupID == "" {
				groupID = uuid.New().String()
				cfg.Groups = append(cfg.Groups, model.Group{
					ID:          groupID,
					Name:        req.Group,
					RegistryKey: req.Group,
				})
			}

			// 按 ip+port+group 去重
			for i := range cfg.Upstreams {
				u := &cfg.Upstreams[i]
				if u.API == apiURL && u.GroupID == groupID {
					instanceID = u.ID
					// 禁止覆盖检查
					if u.NoOverride {
						// 仅更新心跳时间
						u.HeartbeatAt = &now
						return
					}
					// 更新已有上游
					u.APIKey = req.APIKey
					u.Models = req.Models
					u.Status = req.Status
					u.Weight = req.Weight
					u.Remark = req.Remark
					u.Source = "registry"
					u.HeartbeatAt = &now
					return
				}
			}

			// 新建上游
			instanceID = uuid.New().String()
			cfg.Upstreams = append(cfg.Upstreams, model.Upstream{
				ID:          instanceID,
				GroupID:     groupID,
				API:         apiURL,
				APIKey:      req.APIKey,
				Models:      req.Models,
				Status:      req.Status,
				Weight:      req.Weight,
				Remark:      req.Remark,
				Source:      "registry",
				HeartbeatAt: &now,
			})
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"instance_id": instanceID,
		})
	}
}

func heartbeatHandler(s *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			InstanceID string `json:"instance_id" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		now := time.Now().Unix()
		var found bool

		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Upstreams {
				if cfg.Upstreams[i].ID == req.InstanceID {
					cfg.Upstreams[i].HeartbeatAt = &now
					// 心跳超时被标记故障的，自动恢复
					if cfg.Upstreams[i].Source == "registry" &&
						cfg.Upstreams[i].Status == "faulted" &&
						strings.Contains(cfg.Upstreams[i].FaultReason, "heartbeat") {
						cfg.Upstreams[i].Status = "active"
						cfg.Upstreams[i].FaultedAt = nil
						cfg.Upstreams[i].FaultType = ""
						cfg.Upstreams[i].FaultReason = ""
					}
					found = true
					return
				}
			}
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	}
}

func deregisterHandler(s *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			InstanceID string `json:"instance_id" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i, u := range cfg.Upstreams {
				if u.ID == req.InstanceID {
					cfg.Upstreams = append(cfg.Upstreams[:i], cfg.Upstreams[i+1:]...)
					found = true
					return
				}
			}
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	}
}
