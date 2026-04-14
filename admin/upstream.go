package admin

import (
	"net/http"
	"strconv"
	"time"

	"ai-gateway/model"
	"ai-gateway/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func registerUpstreamRoutes(r *gin.RouterGroup, s *store.Store, bq BalancerQuerier) {
	r.GET("/upstreams", func(c *gin.Context) {
		groupID := c.Query("group_id")
		upstreams := s.Get().Upstreams
		if groupID != "" {
			filtered := make([]model.Upstream, 0)
			for _, u := range upstreams {
				if u.GroupID == groupID {
					filtered = append(filtered, u)
				}
			}
			upstreams = filtered
		}

		// 附加运行时状态
		cooldowns := bq.CooldownInfo()
		type UpstreamDTO struct {
			model.Upstream
			RuntimeStatus string `json:"runtime_status"`
			CooldownUntil *int64 `json:"cooldown_until,omitempty"`
		}
		result := make([]UpstreamDTO, 0, len(upstreams))
		for _, u := range upstreams {
			dto := UpstreamDTO{Upstream: u}
			if until, ok := cooldowns[u.ID]; ok {
				dto.RuntimeStatus = "cooldown"
				ts := until.Unix()
				dto.CooldownUntil = &ts
			} else {
				switch u.Status {
				case "faulted":
					dto.RuntimeStatus = "faulted"
				case "half_open":
					dto.RuntimeStatus = "half_open"
				case "disabled":
					dto.RuntimeStatus = "disabled"
				default:
					dto.RuntimeStatus = "normal"
				}
			}
			result = append(result, dto)
		}
		c.JSON(http.StatusOK, gin.H{"data": result})
	})

	r.POST("/upstreams", func(c *gin.Context) {
		var req struct {
			GroupID        string   `json:"group_id" binding:"required"`
			API            string   `json:"api" binding:"required"`
			APIKey         string   `json:"api_key" binding:"required"`
			Remark         string   `json:"remark"`
			Models         []string `json:"models"`
			MaxConcurrency int      `json:"max_concurrency"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// 验证分组存在
		cfg := s.Get()
		var groupExists bool
		for _, g := range cfg.Groups {
			if g.ID == req.GroupID {
				groupExists = true
				break
			}
		}
		if !groupExists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "group not found"})
			return
		}
		u := model.Upstream{
			ID:             uuid.New().String(),
			GroupID:        req.GroupID,
			API:            req.API,
			APIKey:         req.APIKey,
			Models:         req.Models,
			MaxConcurrency: req.MaxConcurrency,
			Remark:         req.Remark,
			Status:         "active",
		}
		if err := s.Update(func(cfg *model.Config) {
			cfg.Upstreams = append(cfg.Upstreams, u)
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": u})
	})

	r.PUT("/upstreams/:id", func(c *gin.Context) {
		id := c.Param("id")
		var req struct {
			API            string    `json:"api"`
			APIKey         string    `json:"api_key"`
			Remark         *string   `json:"remark"`           // 指针区分未传和传空字符串
			Models         *[]string `json:"models"`           // 指针区分未传和空数组
			MaxConcurrency *int      `json:"max_concurrency"`  // 指针区分未传和传 0
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Upstreams {
				if cfg.Upstreams[i].ID == id {
					if req.API != "" {
						cfg.Upstreams[i].API = req.API
					}
					if req.APIKey != "" {
						cfg.Upstreams[i].APIKey = req.APIKey
					}
					if req.Remark != nil {
						cfg.Upstreams[i].Remark = *req.Remark
					}
					if req.Models != nil {
						cfg.Upstreams[i].Models = *req.Models
					}
					if req.MaxConcurrency != nil {
						cfg.Upstreams[i].MaxConcurrency = *req.MaxConcurrency
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
			c.JSON(http.StatusNotFound, gin.H{"error": "upstream not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})

	r.DELETE("/upstreams/:id", func(c *gin.Context) {
		id := c.Param("id")
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i, u := range cfg.Upstreams {
				if u.ID == id {
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
			c.JSON(http.StatusNotFound, gin.H{"error": "upstream not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})

	r.POST("/upstreams/:id/fault", func(c *gin.Context) {
		id := c.Param("id")
		now := time.Now().Unix()
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Upstreams {
				if cfg.Upstreams[i].ID == id {
					cfg.Upstreams[i].Status = "faulted"
					cfg.Upstreams[i].FaultedAt = &now
					cfg.Upstreams[i].FaultType = "manual"
					cfg.Upstreams[i].FaultReason = "手动标记"
					found = true
					return
				}
			}
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "upstream not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})

	r.POST("/upstreams/:id/recover", func(c *gin.Context) {
		id := c.Param("id")
		var found bool
		if err := s.Update(func(cfg *model.Config) {
			for i := range cfg.Upstreams {
				if cfg.Upstreams[i].ID == id {
					cfg.Upstreams[i].Status = "active"
					cfg.Upstreams[i].FaultedAt = nil
					cfg.Upstreams[i].FaultType = ""
					cfg.Upstreams[i].FaultReason = ""
					found = true
					return
				}
			}
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "upstream not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
	})

	// 会话亲和列表
	r.GET("/sessions", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": bq.SessionInfo()})
	})

	// 请求日志
	r.GET("/request-logs", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": bq.RequestLogs()})
	})

	// 单条日志详情（含完整 body）
	r.GET("/request-logs/:idx", func(c *gin.Context) {
		idx, err := strconv.Atoi(c.Param("idx"))
		if err != nil || idx < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid index"})
			return
		}
		entry := bq.RequestLogDetail(idx)
		if entry == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": entry})
	})

	// 统计数据
	r.GET("/stats", func(c *gin.Context) {
		cfg := s.Get()
		c.JSON(http.StatusOK, gin.H{"data": bq.DailyStats(cfg.ModelPricing)})
	})

	// 模型价格表
	r.GET("/pricing", func(c *gin.Context) {
		cfg := s.Get()
		c.JSON(http.StatusOK, gin.H{"data": cfg.ModelPricing})
	})

	// 当前并发数
	r.GET("/concurrency", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": bq.LoadInfo()})
	})
}
