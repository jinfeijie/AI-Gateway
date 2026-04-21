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
	mu       sync.RWMutex
	path     string
	cfg      model.Config
	keyIndex map[string]int // API Key → Groups 数组下标
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
	// 迁移旧 api_key 到 api_keys 数组
	dirty := false
	for i := range s.cfg.Groups {
		if s.cfg.Groups[i].APIKey != "" {
			// 旧的单 key 迁移到新的多 key 数组
			s.cfg.Groups[i].APIKeys = append(s.cfg.Groups[i].APIKeys, model.GroupAPIKey{
				ID:     uuid.New().String(),
				Key:    s.cfg.Groups[i].APIKey,
				Remark: "默认",
			})
			s.cfg.Groups[i].APIKey = ""
			dirty = true
		}
		if len(s.cfg.Groups[i].APIKeys) == 0 {
			// 没有任何 key，自动生成一个
			s.cfg.Groups[i].APIKeys = []model.GroupAPIKey{{
				ID:     uuid.New().String(),
				Key:    "sk-" + uuid.New().String(),
				Remark: "默认",
			}}
			dirty = true
		}
	}
	if dirty {
		s.save()
	}
	s.rebuildIndex()
	return s, nil
}

// rebuildIndex 重建 API Key → Group 索引
func (s *Store) rebuildIndex() {
	idx := make(map[string]int, len(s.cfg.Groups)*2)
	for i, g := range s.cfg.Groups {
		for _, ak := range g.APIKeys {
			idx[ak.Key] = i
		}
	}
	s.keyIndex = idx
}

// FindGroupByKey 通过 API Key 查找分组，返回配置快照和匹配的分组（O(1) 查找）
func (s *Store) FindGroupByKey(key string) (model.Config, *model.Group) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i, ok := s.keyIndex[key]; ok && i < len(s.cfg.Groups) {
		cfg := s.cfg
		return cfg, &cfg.Groups[i]
	}
	return s.cfg, nil
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
	s.rebuildIndex()
	return s.save()
}
