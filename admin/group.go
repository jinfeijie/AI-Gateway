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
			Name           string                  `json:"name" binding:"required"`
			Protocols      []string                `json:"protocols"`
			Models         []string                `json:"models"`
			ModelMapping   map[string]string       `json:"model_mapping"`
			FailoverRules  []model.FailoverRule     `json:"failover_rules"`
			HealthCheck    *model.HealthCheckConfig `json:"health_check"`
			LogMode        string                  `json:"log_mode"`
			LogSampleRate  *int                    `json:"log_sample_rate"`
			AllowStream    *bool                   `json:"allow_stream"`
			AllowNonStream *bool                   `json:"allow_non_stream"`
			NoCache        *bool                   `json:"no_cache"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		g := model.Group{
			ID:   uuid.New().String(),
			Name: req.Name,
			APIKeys: []model.GroupAPIKey{{
				ID:     uuid.New().String(),
				Key:    "sk-" + uuid.New().String(),
				Remark: "默认",
			}},
			Protocols:      req.Protocols,
			Models:         req.Models,
			ModelMapping:   req.ModelMapping,
			FailoverRules:  req.FailoverRules,
			HealthCheck:    req.HealthCheck,
			LogMode:        req.LogMode,
			AllowStream:    req.AllowStream,
			AllowNonStream: req.AllowNonStream,
		}
		if req.NoCache != nil {
			g.NoCache = *req.NoCache
		}
		if req.LogSampleRate != nil {
			g.LogSampleRate = *req.LogSampleRate
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
			Protocols      []string                `json:"protocols"`
			Models         []string                `json:"models"`
			ModelMapping   map[string]string       `json:"model_mapping"`
			MaxConcurrency *int                    `json:"max_concurrency"`
			FailoverRules  []model.FailoverRule     `json:"failover_rules"`
			HealthCheck    *model.HealthCheckConfig `json:"health_check"`
			LogMode        *string                 `json:"log_mode"`
			LogSampleRate  *int                    `json:"log_sample_rate"`
			AllowStream    *bool                   `json:"allow_stream"`
			AllowNonStream *bool                   `json:"allow_non_stream"`
			NoCache        *bool                   `json:"no_cache"`
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
					if req.Protocols != nil {
						cfg.Groups[i].Protocols = req.Protocols
					}
					if req.Models != nil {
						cfg.Groups[i].Models = req.Models
					}
					if req.ModelMapping != nil {
						cfg.Groups[i].ModelMapping = req.ModelMapping
					}
					if req.FailoverRules != nil {
						cfg.Groups[i].FailoverRules = req.FailoverRules
					}
					if req.HealthCheck != nil {
						cfg.Groups[i].HealthCheck = req.HealthCheck
					}
					if req.MaxConcurrency != nil {
						cfg.Groups[i].MaxConcurrency = *req.MaxConcurrency
					}
					if req.LogMode != nil {
						cfg.Groups[i].LogMode = *req.LogMode
					}
					if req.LogSampleRate != nil {
						cfg.Groups[i].LogSampleRate = *req.LogSampleRate
					}
					if req.AllowStream != nil {
						cfg.Groups[i].AllowStream = req.AllowStream
					}
					if req.AllowNonStream != nil {
						cfg.Groups[i].AllowNonStream = req.AllowNonStream
					}
					if req.NoCache != nil {
						cfg.Groups[i].NoCache = *req.NoCache
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

	// 重新生成分组 API Key（兼容旧接口，重新生成第一个 key）
	r.POST("/groups/:id/regen-key", func(c *gin.Context) {
		id := c.Param("id")
		newKey := "sk-" + uuid.New().String()
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Groups {
				if cfg.Groups[i].ID == id {
					if len(cfg.Groups[i].APIKeys) > 0 {
						cfg.Groups[i].APIKeys[0].Key = newKey
					} else {
						cfg.Groups[i].APIKeys = []model.GroupAPIKey{{
							ID: uuid.New().String(), Key: newKey, Remark: "默认",
						}}
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
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"api_key": newKey}})
	})

	// 添加 API Key
	r.POST("/groups/:id/keys", func(c *gin.Context) {
		id := c.Param("id")
		var req struct {
			Remark string `json:"remark"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		newKey := model.GroupAPIKey{
			ID:     uuid.New().String(),
			Key:    "sk-" + uuid.New().String(),
			Remark: req.Remark,
		}
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Groups {
				if cfg.Groups[i].ID == id {
					cfg.Groups[i].APIKeys = append(cfg.Groups[i].APIKeys, newKey)
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
		c.JSON(http.StatusOK, gin.H{"data": newKey})
	})

	// 删除 API Key
	r.DELETE("/groups/:id/keys/:keyId", func(c *gin.Context) {
		groupID := c.Param("id")
		keyID := c.Param("keyId")
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Groups {
				if cfg.Groups[i].ID == groupID {
					keys := cfg.Groups[i].APIKeys
					if len(keys) <= 1 {
						return // 至少保留一个 key
					}
					for j, k := range keys {
						if k.ID == keyID {
							cfg.Groups[i].APIKeys = append(keys[:j], keys[j+1:]...)
							found = true
							return
						}
					}
					return
				}
			}
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !found {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key not found or is the last key"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})

	// 重新生成指定 API Key
	r.POST("/groups/:id/keys/:keyId/regen", func(c *gin.Context) {
		groupID := c.Param("id")
		keyID := c.Param("keyId")
		newKey := "sk-" + uuid.New().String()
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Groups {
				if cfg.Groups[i].ID == groupID {
					for j := range cfg.Groups[i].APIKeys {
						if cfg.Groups[i].APIKeys[j].ID == keyID {
							cfg.Groups[i].APIKeys[j].Key = newKey
							found = true
							return
						}
					}
					return
				}
			}
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"api_key": newKey}})
	})

	// 修改 API Key 备注
	r.PUT("/groups/:id/keys/:keyId", func(c *gin.Context) {
		groupID := c.Param("id")
		keyID := c.Param("keyId")
		var req struct {
			Remark string `json:"remark"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Groups {
				if cfg.Groups[i].ID == groupID {
					for j := range cfg.Groups[i].APIKeys {
						if cfg.Groups[i].APIKeys[j].ID == keyID {
							cfg.Groups[i].APIKeys[j].Remark = req.Remark
							found = true
							return
						}
					}
					return
				}
			}
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
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
