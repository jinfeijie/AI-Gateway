package model

// Upstream 上游 API 配置
type Upstream struct {
	ID             string   `json:"id"`
	GroupID        string   `json:"group_id"`
	API            string   `json:"api"`
	APIKey         string   `json:"api_key"`
	Remark         string   `json:"remark"`
	Models         []string `json:"models,omitempty"`         // 支持的模型，空则继承分组
	MaxConcurrency int      `json:"max_concurrency,omitempty"` // 最大并发数，0 不限制
	Status         string   `json:"status"`                   // active / faulted / disabled
	FaultedAt      *int64   `json:"faulted_at,omitempty"`     // 故障时间戳
	FaultType      string   `json:"fault_type,omitempty"`     // auto / manual
	FaultReason    string   `json:"fault_reason,omitempty"`   // 故障原因
}

// FailoverRule 故障转移规则
type FailoverRule struct {
	Code      int    `json:"code"`                  // 触发的状态码
	Action    string `json:"action"`                // "offline" 下线 / "cooldown" 冷却
	CooldownS int    `json:"cooldown_s,omitempty"`  // cooldown 秒数，默认 60
	UseHeader string `json:"use_header,omitempty"`  // 从响应 header 读取冷却时长，如 "retry-after"
}

// Group 分组
type Group struct {
	ID             string             `json:"id"`
	Name           string             `json:"name"`
	APIKey         string             `json:"api_key"`
	Models         []string           `json:"models,omitempty"` // 分组支持的模型
	MaxConcurrency int                `json:"max_concurrency,omitempty"` // 分组最大并发数，0 不限制
	FailoverRules  []FailoverRule     `json:"failover_rules,omitempty"`
	HealthCheck    *HealthCheckConfig `json:"health_check,omitempty"`
}

// HealthCheckConfig 健康检查配置
type HealthCheckConfig struct {
	Enabled   bool              `json:"enabled"`
	IntervalS int               `json:"interval_s"`
	Retries   int               `json:"retries"`
	TimeoutS  int               `json:"timeout_s"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      string            `json:"body,omitempty"`
}

// ErrorMapping 错误码映射
type ErrorMapping struct {
	SourceCode int    `json:"source_code"`
	TargetCode int    `json:"target_code"`
	Message    string `json:"message,omitempty"`
}

// ModelPricing 模型价格（美元/百万 token）
type ModelPricing struct {
	Model      string  `json:"model"`
	Input      float64 `json:"input"`       // $/M input tokens
	Output     float64 `json:"output"`      // $/M output tokens
	CacheRead  float64 `json:"cache_read"`  // $/M cache read tokens
	CacheWrite float64 `json:"cache_write"` // $/M cache write tokens
}

// Config 总配置
type Config struct {
	ListenAddr      string            `json:"listen_addr"`
	AdminToken      string            `json:"admin_token"`
	AdminUser       string            `json:"admin_user"`
	AdminPassword   string            `json:"admin_password"`
	Groups          []Group           `json:"groups"`
	Upstreams       []Upstream        `json:"upstreams"`
	HealthCheck     HealthCheckConfig `json:"health_check"`
	ErrorMappings   []ErrorMapping    `json:"error_mappings"`
	StripFields       []string          `json:"strip_fields,omitempty"`
	ProbeModels       []string          `json:"probe_models,omitempty"`
	DefaultProbeModel string            `json:"default_probe_model,omitempty"`
	ModelPricing      []ModelPricing    `json:"model_pricing,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		ListenAddr:    ":8080",
		AdminToken:    "",
		AdminUser:     "admin",
		AdminPassword: "admin",
		Groups:        []Group{},
		Upstreams:     []Upstream{},
		HealthCheck: HealthCheckConfig{
			Enabled:   false,
			IntervalS: 60,
			Retries:   3,
			TimeoutS:  10,
		},
		ErrorMappings:     []ErrorMapping{},
		DefaultProbeModel: "claude-sonnet-4-6",
		ProbeModels: []string{
			"claude-sonnet-4-6",
			"claude-haiku-4-5-20251001",
			"claude-opus-4-6",
			"claude-sonnet-4-20250514",
		},
		ModelPricing: DefaultModelPricing(),
	}
}

// DefaultModelPricing 默认模型价格
func DefaultModelPricing() []ModelPricing {
	return []ModelPricing{
		{Model: "claude-opus-4-6", Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
		{Model: "claude-sonnet-4-6", Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
		{Model: "claude-sonnet-4-20250514", Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
		{Model: "claude-haiku-4-5-20251001", Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
	}
}

// DefaultFailoverRules 默认故障转移规则
func DefaultFailoverRules() []FailoverRule {
	return []FailoverRule{
		{Code: 404, Action: "offline"},
		{Code: 429, Action: "cooldown", CooldownS: 60, UseHeader: "retry-after"},
		{Code: 500, Action: "offline"},
		{Code: 502, Action: "offline"},
		{Code: 503, Action: "offline"},
		{Code: 504, Action: "offline"},
	}
}
