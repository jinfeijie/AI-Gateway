package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"ai-gateway/model"

	"github.com/google/uuid"
)

type Store struct {
	mu   sync.RWMutex
	path string
	cfg  model.Config
}

func New(path string) (*Store, error) {
	s := &Store{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.cfg = model.DefaultConfig()
			return s, s.save()
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &s.cfg); err != nil {
		return nil, err
	}
	// 补充新增字段的默认值
	defaults := model.DefaultConfig()
	if s.cfg.AdminUser == "" {
		s.cfg.AdminUser = defaults.AdminUser
	}
	if s.cfg.AdminPassword == "" {
		s.cfg.AdminPassword = defaults.AdminPassword
	}
	if len(s.cfg.ModelPricing) == 0 {
		s.cfg.ModelPricing = defaults.ModelPricing
	}
	// 为已有分组补齐 API Key
	dirty := false
	for i := range s.cfg.Groups {
		if s.cfg.Groups[i].APIKey == "" {
			s.cfg.Groups[i].APIKey = "sk-" + uuid.New().String()
			dirty = true
		}
	}
	if dirty {
		s.save()
	}
	return s, nil
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

func (s *Store) Get() model.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Store) Update(fn func(*model.Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	return s.save()
}
