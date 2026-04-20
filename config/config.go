package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const Path = ".codeindex/config.json"

type Config struct {
	FTS   FTSConfig   `json:"fts"`
	Index IndexConfig `json:"index"`
}

type FTSConfig struct {
	Include       []string `json:"include"`
	ExcludeDirs   []string `json:"excludeDirs"`
	MaxFileSizeKB int      `json:"maxFileSizeKB"`
}

type IndexConfig struct {
	Include []string `json:"include"`
}

func Default() *Config {
	return &Config{
		FTS: FTSConfig{
			Include:       []string{"*.go"},
			ExcludeDirs:   []string{"vendor", ".git", "node_modules", "testdata", ".codeindex"},
			MaxFileSizeKB: 50,
		},
		Index: IndexConfig{
			Include: []string{"*.go"},
		},
	}
}

func Load() (*Config, error) {
	data, err := os.ReadFile(Path)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path, data, 0644)
}

func (c *Config) MatchesFTS(path string) bool {
	name := filepath.Base(path)
	for _, pattern := range c.FTS.Include {
		if ok, _ := filepath.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

func (c *Config) IsExcludedDir(name string) bool {
	for _, d := range c.FTS.ExcludeDirs {
		if d == name {
			return true
		}
	}
	return false
}
