package admin

import (
	"net/http"

	"ai-gateway/model"
	"ai-gateway/store"

	"github.com/gin-gonic/gin"
)

func registerErrorMappingRoutes(r *gin.RouterGroup, s *store.Store) {
	// 全局错误码映射
	r.GET("/error-mappings", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": s.Get().ErrorMappings})
	})

	r.PUT("/error-mappings", func(c *gin.Context) {
		var req []model.ErrorMapping
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := s.Update(func(cfg *model.Config) {
			cfg.ErrorMappings = req
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})

	// 分组级错误码映射
	r.GET("/groups/:id/error-mappings", func(c *gin.Context) {
		id := c.Param("id")
		cfg := s.Get()
		for _, g := range cfg.Groups {
			if g.ID == id {
				c.JSON(http.StatusOK, gin.H{"data": g.ErrorMappings})
				return
			}
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "group not found"})
	})

	r.PUT("/groups/:id/error-mappings", func(c *gin.Context) {
		id := c.Param("id")
		var req []model.ErrorMapping
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Groups {
				if cfg.Groups[i].ID == id {
					cfg.Groups[i].ErrorMappings = req
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

	// 分组级字段剔除
	r.GET("/groups/:id/strip-fields", func(c *gin.Context) {
		id := c.Param("id")
		cfg := s.Get()
		for _, g := range cfg.Groups {
			if g.ID == id {
				c.JSON(http.StatusOK, gin.H{"data": g.StripFields})
				return
			}
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "group not found"})
	})

	r.PUT("/groups/:id/strip-fields", func(c *gin.Context) {
		id := c.Param("id")
		var req []string
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Groups {
				if cfg.Groups[i].ID == id {
					cfg.Groups[i].StripFields = req
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

	r.GET("/strip-fields", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": s.Get().StripFields})
	})

	r.PUT("/strip-fields", func(c *gin.Context) {
		var req []string
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := s.Update(func(cfg *model.Config) {
			cfg.StripFields = req
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})
}
