package config

import (
	"slices"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load([]string{"-dev"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":8080" || cfg.LogLevel != "info" || cfg.LogFormat != "text" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if !slices.Equal(cfg.STUNURLs, []string{"stun:stun.l.google.com:19302"}) {
		t.Errorf("STUNURLs = %v", cfg.STUNURLs)
	}
}

func TestLoadFlags(t *testing.T) {
	cfg, err := Load([]string{
		"-addr", ":9000",
		"-log-level", "debug",
		"-log-format", "json",
		"-stun", " stun:a.example:3478 , stun:b.example:3478 ,",
		"-shutdown-timeout", "3s",
		"-tenants", "tenants.json",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9000" || cfg.LogLevel != "debug" || cfg.LogFormat != "json" || cfg.ShutdownTimeout != 3*time.Second || cfg.TenantsFile != "tenants.json" {
		t.Errorf("flags not applied: %+v", cfg)
	}
	if !slices.Equal(cfg.STUNURLs, []string{"stun:a.example:3478", "stun:b.example:3478"}) {
		t.Errorf("STUNURLs = %v", cfg.STUNURLs)
	}
}

func TestLoadEmptySTUNDisablesIt(t *testing.T) {
	cfg, err := Load([]string{"-dev", "-stun", ""})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.STUNURLs) != 0 {
		t.Errorf("STUNURLs = %v, want empty", cfg.STUNURLs)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	tests := map[string]func(*Config){
		"empty addr":       func(c *Config) { c.Addr = "" },
		"bad log level":    func(c *Config) { c.LogLevel = "verbose" },
		"bad log format":   func(c *Config) { c.LogFormat = "xml" },
		"bad stun url":     func(c *Config) { c.STUNURLs = []string{"http://example.com"} },
		"zero shutdown":    func(c *Config) { c.ShutdownTimeout = 0 },
		"negative timeout": func(c *Config) { c.ShutdownTimeout = -time.Second },
		"no tenants":       func(c *Config) { c.Dev = false },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			cfg.Dev = true
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("Validate returned nil, want error")
			}
		})
	}
}

func TestLoadUnknownFlag(t *testing.T) {
	if _, err := Load([]string{"-nope"}); err == nil {
		t.Error("Load returned nil, want error for unknown flag")
	}
}
