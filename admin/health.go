package admin

import (
	"net/http"

	"ai-gateway/health"
	"ai-gateway/model"
	"ai-gateway/store"

	"github.com/gin-gonic/gin"
)

func registerHealthCheckRoutes(r *gin.RouterGroup, s *store.Store, checker *health.Checker) {
	r.GET("/health-check", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": s.Get().HealthCheck})
	})

	// 健康检查历史
	r.GET("/health-check/history", func(c *gin.Context) {
		upstreamID := c.Query("upstream_id")
		c.JSON(http.StatusOK, gin.H{"data": checker.History(upstreamID)})
	})

	r.PUT("/health-check", func(c *gin.Context) {
		var req model.HealthCheckConfig
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := s.Update(func(cfg *model.Config) {
			cfg.HealthCheck = req
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})

	// 测活模型配置
	r.GET("/probe-models", func(c *gin.Context) {
		cfg := s.Get()
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"models":              cfg.ProbeModels,
			"default_model":      cfg.DefaultProbeModel,
			"openai_models":       cfg.OpenAIProbeModels,
			"default_openai_model": cfg.DefaultOpenAIProbeModel,
		}})
	})

	r.PUT("/probe-models", func(c *gin.Context) {
		var req struct {
			Models             []string `json:"models"`
			DefaultModel       string   `json:"default_model"`
			OpenAIModels       []string `json:"openai_models"`
			DefaultOpenAIModel string   `json:"default_openai_model"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := s.Update(func(cfg *model.Config) {
			cfg.ProbeModels = req.Models
			cfg.DefaultProbeModel = req.DefaultModel
			cfg.OpenAIProbeModels = req.OpenAIModels
			cfg.DefaultOpenAIProbeModel = req.DefaultOpenAIModel
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})

	// 单个上游探活
	r.POST("/upstreams/:id/probe", func(c *gin.Context) {
		id := c.Param("id")
		var req struct {
			Model string `json:"model"`
		}
		c.ShouldBindJSON(&req)

		cfg := s.Get()
		var upstream *model.Upstream
		for _, u := range cfg.Upstreams {
			if u.ID == id {
				upstream = &u
				break
			}
		}
		if upstream == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "upstream not found"})
			return
		}
		result := checker.Probe(upstream.API, upstream.APIKey, req.Model, upstream.Protocol, cfg.HealthCheck)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"id":          id,
			"healthy":     result.Healthy,
			"status_code": result.StatusCode,
			"body":        result.Body,
			"reply":       result.Reply,
			"error":       result.Error,
		}})
	})
}
