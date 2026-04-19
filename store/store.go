package store

import (
	"encoding/json"
	"log"
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
		// 配置文件损坏，备份后用默认配置重建
		backupPath := path + ".corrupt"
		_ = os.WriteFile(backupPath, data, 0644)
		log.Printf("config file corrupted, backed up to %s, reinitializing with defaults", backupPath)
		s.cfg = model.DefaultConfig()
		return s, s.save()
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
	// 原子写入：先写临时文件再重命名，防止写入中断导致文件损坏
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
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
