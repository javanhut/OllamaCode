package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type KVStore struct {
	mu   sync.RWMutex
	path string
	data map[string]interface{}
}

func NewKVStore(path string) (*KVStore, error) {
	s := &KVStore{
		path: path,
		data: make(map[string]interface{}),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *KVStore) load() error {
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		return nil
	}
	file, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(file, &s.data)
}

func (s *KVStore) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	file, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, file, 0644)
}

func (s *KVStore) Set(key string, value interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return s.save()
}

func (s *KVStore) Get(key string) (interface{}, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	val, ok := s.data[key]
	return val, ok
}

func (s *KVStore) GetFullData() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

func (s *KVStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return s.save()
}
