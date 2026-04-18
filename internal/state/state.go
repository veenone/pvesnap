package state

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/veenone/pvesnap/internal/config"
	"gopkg.in/yaml.v3"
)

type GuestStatus string

const (
	StatusOK      GuestStatus = "ok"
	StatusFailed  GuestStatus = "failed"
	StatusPending GuestStatus = "pending"
)

type GuestRecord struct {
	Node     string           `yaml:"node"`
	VMID     int              `yaml:"vmid"`
	Type     config.GuestType `yaml:"type"`
	Snapname string           `yaml:"snapname"`
	Status   GuestStatus      `yaml:"status"`
	Error    string           `yaml:"error,omitempty"`
}

type Snapshot struct {
	Set         string        `yaml:"set"`
	Name        string        `yaml:"name"`
	Description string        `yaml:"description,omitempty"`
	CreatedAt   time.Time     `yaml:"created_at"`
	Guests      []GuestRecord `yaml:"guests"`
}

type Store struct {
	Snapshots []Snapshot `yaml:"snapshots"`
}

func DefaultPath() string {
	if p := os.Getenv("PVESNAP_STATE"); p != "" {
		return p
	}
	if h, err := os.UserConfigDir(); err == nil {
		return filepath.Join(h, "pvesnap", "state.yaml")
	}
	return "state.yaml"
}

func Load(path string) (*Store, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Store{}, nil
		}
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	var s Store
	if err := yaml.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	return &s, nil
}

// Save writes the store atomically (temp file + rename).
func (s *Store) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	b, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

func (s *Store) Find(setName, name string) (*Snapshot, int) {
	for i := range s.Snapshots {
		if s.Snapshots[i].Set == setName && s.Snapshots[i].Name == name {
			return &s.Snapshots[i], i
		}
	}
	return nil, -1
}

func (s *Store) Upsert(snap Snapshot) {
	if existing, idx := s.Find(snap.Set, snap.Name); existing != nil {
		s.Snapshots[idx] = snap
		return
	}
	s.Snapshots = append(s.Snapshots, snap)
}

func (s *Store) Remove(setName, name string) bool {
	for i := range s.Snapshots {
		if s.Snapshots[i].Set == setName && s.Snapshots[i].Name == name {
			s.Snapshots = append(s.Snapshots[:i], s.Snapshots[i+1:]...)
			return true
		}
	}
	return false
}
