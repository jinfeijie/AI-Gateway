package main

import (
	"flag"
	"log"

	"ai-gateway/admin"
	"ai-gateway/health"
	"ai-gateway/proxy"
	"ai-gateway/registry"
	"ai-gateway/store"
	"ai-gateway/web"

	"github.com/gin-gonic/gin"
)

func main() {
	dataPath := flag.String("data", "data/config.json", "config data file path")
	flag.Parse()

	s, err := store.New(*dataPath)
	if err != nil {
		log.Fatalf("failed to load store: %v", err)
	}

	cfg := s.Get()
	addr := cfg.ListenAddr
	if addr == "" {
		addr = ":8080"
	}

	r := gin.Default()
	r.RedirectTrailingSlash = false

	// Health checker
	checker := health.NewChecker(s)

	// Proxy
	h := proxy.NewHandler(s, *dataPath)
	h.RegisterRoutes(r)

	// 注入冷却设置器后再启动健康检查
	checker.SetCooldownSetter(h.Balancer())
	checker.Start()
	defer checker.Stop()

	// Admin API
	adminGroup := r.Group("/admin")
	admin.Register(adminGroup, s, checker, h)

	// Registry API（注册中心，独立鉴权）
	registryGroup := r.Group("/registry")
	registry.Register(registryGroup, s)

	// Web UI
	web.RegisterRoutes(r)

	log.Printf("AI Gateway listening on %s", addr)
	log.Printf("Admin UI: http://localhost%s/ui", addr)
	if err := r.Run(addr); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
