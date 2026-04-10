package admin

import (
	"net/http"

	"ai-gateway/model"
	"ai-gateway/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func registerGroupRoutes(r *gin.RouterGroup, s *store.Store) {
	r.GET("/groups", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": s.Get().Groups})
	})

	r.POST("/groups", func(c *gin.Context) {
		var req struct {
			Name          string                  `json:"name" binding:"required"`
			Models        []string                `json:"models"`
			FailoverRules []model.FailoverRule     `json:"failover_rules"`
			HealthCheck   *model.HealthCheckConfig `json:"health_check"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		g := model.Group{
			ID:            uuid.New().String(),
			Name:          req.Name,
			APIKey:        "sk-" + uuid.New().String(),
			Models:        req.Models,
			FailoverRules: req.FailoverRules,
			HealthCheck:   req.HealthCheck,
		}
		if err := s.Update(func(cfg *model.Config) {
			cfg.Groups = append(cfg.Groups, g)
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": g})
	})

	r.PUT("/groups/:id", func(c *gin.Context) {
		id := c.Param("id")
		var req struct {
			Name           string                  `json:"name" binding:"required"`
			Models         []string                `json:"models"`
			MaxConcurrency *int                    `json:"max_concurrency"`
			FailoverRules  []model.FailoverRule     `json:"failover_rules"`
			HealthCheck    *model.HealthCheckConfig `json:"health_check"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Groups {
				if cfg.Groups[i].ID == id {
					cfg.Groups[i].Name = req.Name
					cfg.Groups[i].Models = req.Models
					cfg.Groups[i].FailoverRules = req.FailoverRules
					cfg.Groups[i].HealthCheck = req.HealthCheck
					if req.MaxConcurrency != nil {
						cfg.Groups[i].MaxConcurrency = *req.MaxConcurrency
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
			c.JSON(http.StatusNotFound, gin.H{"error": "group not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})

	// 重新生成分组 API Key
	r.POST("/groups/:id/regen-key", func(c *gin.Context) {
		id := c.Param("id")
		newKey := "sk-" + uuid.New().String()
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Groups {
				if cfg.Groups[i].ID == id {
					cfg.Groups[i].APIKey = newKey
					found = true
					return
				}
			}
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "group not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"api_key": newKey}})
	})

	r.DELETE("/groups/:id", func(c *gin.Context) {
		id := c.Param("id")
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i, g := range cfg.Groups {
				if g.ID == id {
					cfg.Groups = append(cfg.Groups[:i], cfg.Groups[i+1:]...)
					found = true
					upstreams := cfg.Upstreams[:0]
					for _, u := range cfg.Upstreams {
						if u.GroupID != id {
							upstreams = append(upstreams, u)
						}
					}
					cfg.Upstreams = upstreams
					return
				}
			}
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "group not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})
}
