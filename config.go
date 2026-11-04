package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Station struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	Group string `json:"group,omitempty"`
}

type Config struct {
	Volume        int       `json:"volume"`
	VolumeStep    int       `json:"volume_step"`
	Autoplay      bool      `json:"autoplay"`
	Notifications bool      `json:"notifications"`
	LastStation   string    `json:"last_station"`
	Stations      []Station `json:"stations"`
}

func DefaultConfig() *Config {
	return &Config{
		Volume:        70,
		VolumeStep:    5,
		Notifications: true,
		Stations: []Station{
			{
				ID:   "radio-paradise",
				Name: "Radio Paradise",
				URL:  "https://stream.radioparadise.com/mp3-128",
			},
			{
				ID:    "kiss-fm-armenia",
				Name:  "Kiss FM Armenia",
				URL:   "https://de.auroramedia.am/kissfm.mp3",
				Group: "Favorites",
			},
		},
	}
}

func (c *Config) Validate() error {
	if c.Volume < 0 || c.Volume > 100 {
		return fmt.Errorf("volume %d out of range 0-100", c.Volume)
	}
	if c.VolumeStep <= 0 {
		return fmt.Errorf("volume_step must be positive, got %d", c.VolumeStep)
	}

	seen := make(map[string]bool, len(c.Stations))
	for i, st := range c.Stations {
		id := strings.TrimSpace(st.ID)
		if id == "" {
			return fmt.Errorf("station %d: empty id", i+1)
		}
		if seen[id] {
			return fmt.Errorf("station %d: duplicate id %q", i+1, st.ID)
		}
		seen[id] = true

		if strings.TrimSpace(st.Name) == "" {
			return fmt.Errorf("station %q: empty name", st.ID)
		}

		u, err := url.Parse(st.URL)
		if err != nil || !isHTTP(u.Scheme) || u.Host == "" {
			return fmt.Errorf("station %q: url %q must be an absolute http(s) URL", st.ID, st.URL)
		}
	}
	return nil
}

func isHTTP(scheme string) bool {
	return strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https")
}

func ConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "priyomnik", "config.json"), nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		cfg := DefaultConfig()
		if err := Save(path, cfg); err != nil {
			return nil, fmt.Errorf("create default config: %w", err)
		}
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

func Save(path string, cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		tmp.Close()
		return fmt.Errorf("encode config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync config directory: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
