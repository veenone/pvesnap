package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

type GuestType string

const (
	QEMU GuestType = "qemu"
	LXC  GuestType = "lxc"
)

type Node struct {
	Name      string `yaml:"name"`
	Endpoint  string `yaml:"endpoint"`
	APIToken  string `yaml:"api_token"`
	VerifyTLS bool   `yaml:"verify_tls"`
}

type Guest struct {
	Node  string    `yaml:"node"`
	VMID  int       `yaml:"vmid"`
	Type  GuestType `yaml:"type"`
	Role  string    `yaml:"role,omitempty"`
}

type Set struct {
	Name        string  `yaml:"name"`
	Description string  `yaml:"description,omitempty"`
	Guests      []Guest `yaml:"guests"`
	PBSStorage  string  `yaml:"pbs_storage,omitempty"`
}

type Defaults struct {
	ParallelismPerNode int           `yaml:"parallelism_per_node"`
	TaskPollInterval  time.Duration `yaml:"task_poll_interval"`
	TaskTimeout       time.Duration `yaml:"task_timeout"`
	PBSStorage        string        `yaml:"pbs_storage"`
}

type Config struct {
	Nodes    []Node   `yaml:"nodes"`
	Sets     []Set    `yaml:"sets"`
	Defaults Defaults `yaml:"defaults"`
}

func DefaultPath() string {
	if p := os.Getenv("PVESNAP_CONFIG"); p != "" {
		return p
	}
	if h, err := os.UserConfigDir(); err == nil {
		return filepath.Join(h, "pvesnap", "config.yaml")
	}
	return "config.yaml"
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Defaults.ParallelismPerNode == 0 {
		c.Defaults.ParallelismPerNode = 2
	}
	if c.Defaults.TaskPollInterval == 0 {
		c.Defaults.TaskPollInterval = 2 * time.Second
	}
	if c.Defaults.TaskTimeout == 0 {
		c.Defaults.TaskTimeout = 30 * time.Minute
	}
}

func (c *Config) validate() error {
	if len(c.Nodes) == 0 {
		return fmt.Errorf("config: at least one node required")
	}
	seenNode := map[string]bool{}
	for _, n := range c.Nodes {
		if n.Name == "" || n.Endpoint == "" || n.APIToken == "" {
			return fmt.Errorf("config: node %q missing name/endpoint/api_token", n.Name)
		}
		if seenNode[n.Name] {
			return fmt.Errorf("config: duplicate node name %q", n.Name)
		}
		seenNode[n.Name] = true
	}
	seenSet := map[string]bool{}
	for _, s := range c.Sets {
		if s.Name == "" {
			return fmt.Errorf("config: set with empty name")
		}
		if seenSet[s.Name] {
			return fmt.Errorf("config: duplicate set name %q", s.Name)
		}
		seenSet[s.Name] = true
		if len(s.Guests) == 0 {
			return fmt.Errorf("config: set %q has no guests", s.Name)
		}
		for i, g := range s.Guests {
			if !seenNode[g.Node] {
				return fmt.Errorf("config: set %q guest[%d] references unknown node %q", s.Name, i, g.Node)
			}
			if g.VMID <= 0 {
				return fmt.Errorf("config: set %q guest[%d] invalid vmid", s.Name, i)
			}
			if g.Type != QEMU && g.Type != LXC {
				return fmt.Errorf("config: set %q guest[%d] type must be qemu or lxc", s.Name, i)
			}
		}
	}
	return nil
}

func (c *Config) FindNode(name string) (Node, bool) {
	for _, n := range c.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return Node{}, false
}

// ResolvePBSStorage returns the PBS storage id for a set: the set's override if
// set, otherwise the global default. Empty string means "not configured".
func (c *Config) ResolvePBSStorage(s Set) string {
	if s.PBSStorage != "" {
		return s.PBSStorage
	}
	return c.Defaults.PBSStorage
}

func (c *Config) FindSet(name string) (Set, bool) {
	for _, s := range c.Sets {
		if s.Name == name {
			return s, true
		}
	}
	return Set{}, false
}

var snapNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{1,39}$`)

// NormalizeSnapName converts a user-provided label to a Proxmox-safe snapshot
// name. Proxmox requires [A-Za-z][A-Za-z0-9_-]{1,39}.
func NormalizeSnapName(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("snapshot name is empty")
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	// first char must be a letter
	if !((out[0] >= 'A' && out[0] <= 'Z') || (out[0] >= 'a' && out[0] <= 'z')) {
		out = append([]byte{'s'}, out...)
	}
	if len(out) > 40 {
		out = out[:40]
	}
	res := string(out)
	if !snapNameRe.MatchString(res) {
		return "", fmt.Errorf("snapshot name %q cannot be normalized to a valid Proxmox name", s)
	}
	return res, nil
}
