package config

import "testing"

func TestResolvePBSStorage(t *testing.T) {
	c := &Config{Defaults: Defaults{PBSStorage: "pbs-main"}}
	if got := c.ResolvePBSStorage(Set{}); got != "pbs-main" {
		t.Errorf("default: got %q, want pbs-main", got)
	}
	if got := c.ResolvePBSStorage(Set{PBSStorage: "pbs-set"}); got != "pbs-set" {
		t.Errorf("override: got %q, want pbs-set", got)
	}
	empty := &Config{}
	if got := empty.ResolvePBSStorage(Set{}); got != "" {
		t.Errorf("unset: got %q, want empty", got)
	}
}
