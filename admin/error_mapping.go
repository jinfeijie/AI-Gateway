package admin

import (
	"net/http"

	"ai-gateway/model"
	"ai-gateway/store"

	"github.com/gin-gonic/gin"
)

func registerErrorMappingRoutes(r *gin.RouterGroup, s *store.Store) {
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
