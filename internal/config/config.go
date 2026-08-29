// Package config loads q3ctl's non-secret runtime configuration.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
)

type Config struct {
	Listen        string `json:"listen"`
	RCONAddr      string `json:"rcon_addr"`
	AdminUser     string `json:"admin_user"`
	StateFile     string `json:"state_file"`
	AuditFile     string `json:"audit_file"`
	GameLogFile   string `json:"game_log_file"`
	GameDataPath  string `json:"game_data_path"`
	AdminPassword string `json:"-"`
	RCONPassword  string `json:"-"`
}

func Load(path string) (Config, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(bytes, &c); err != nil {
		return Config{}, err
	}
	c.AdminPassword = os.Getenv("Q3CTL_ADMIN_PASSWORD")
	c.RCONPassword = os.Getenv("Q3CTL_RCON_PASSWORD")
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8088"
	}
	if c.RCONAddr == "" {
		c.RCONAddr = "127.0.0.1:27960"
	}
	if c.StateFile == "" {
		c.StateFile = "/var/lib/q3ctl/state.json"
	}
	if c.AuditFile == "" {
		c.AuditFile = "/var/log/q3ctl/audit.jsonl"
	}
	if c.GameLogFile == "" {
		c.GameLogFile = "/var/log/q3ctl/game.log"
	}
	if c.GameDataPath == "" {
		c.GameDataPath = "/usr/lib/ioquake3/baseq3"
	}
	if c.AdminUser == "" || c.AdminPassword == "" || c.RCONPassword == "" {
		return Config{}, errors.New("admin_user and Q3CTL_ADMIN_PASSWORD/Q3CTL_RCON_PASSWORD required")
	}
	if !strings.HasPrefix(c.Listen, "127.0.0.1:") && !strings.HasPrefix(c.Listen, "[::1]:") {
		return Config{}, errors.New("only loopback listeners allowed")
	}
	return c, nil
}
