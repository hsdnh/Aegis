package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Project    string            `yaml:"project"`
	Schedule   string            `yaml:"schedule"`
	Collectors CollectorsConfig  `yaml:"collectors"`
	Rules      []RuleConfig      `yaml:"rules"`
	AI         AIConfig          `yaml:"ai"`
	Alerts     AlertsConfig      `yaml:"alerts"`
	Storage    StorageConfig     `yaml:"storage"`
	Shadow     []ShadowCheckConfig `yaml:"shadow,omitempty"`
	Probes     []SyntheticProbeConfig `yaml:"probes,omitempty"`
	SourcePath string            `yaml:"source_path,omitempty"` // for L0 scan + instrument
}

type StorageConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"` // SQLite file path, default "./data/aiops.db"
}

type ShadowCheckConfig struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"` // "sql_compare", "cross_check"
	Description string `yaml:"description"`
	Severity    string `yaml:"severity"`
	Query       string `yaml:"query,omitempty"`
	Expect      string `yaml:"expect,omitempty"`
	SourceQuery string `yaml:"source_query,omitempty"`
	TargetQuery string `yaml:"target_query,omitempty"`
	CompareMode string `yaml:"compare_mode,omitempty"`
}

type SyntheticProbeConfig struct {
	Name         string `yaml:"name"`
	Type         string `yaml:"type"` // "http", "redis_roundtrip"
	URL          string `yaml:"url,omitempty"`
	Method       string `yaml:"method,omitempty"`
	ExpectStatus int    `yaml:"expect_status,omitempty"`
	ExpectBody   string `yaml:"expect_body,omitempty"`
	RedisAddr    string `yaml:"redis_addr,omitempty"`
	TimeoutSec   int    `yaml:"timeout_sec,omitempty"`
}

type CollectorsConfig struct {
	Redis  []RedisConfig  `yaml:"redis,omitempty"`
	MySQL  []MySQLConfig  `yaml:"mysql,omitempty"`
	HTTP   []HTTPConfig   `yaml:"http,omitempty"`
	Log    []LogConfig    `yaml:"log,omitempty"`
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	Checks   []struct {
		KeyPattern string  `yaml:"key_pattern"`
		Threshold  float64 `yaml:"threshold"`
		Alert      string  `yaml:"alert"`
	} `yaml:"checks"`
}

type MySQLConfig struct {
	DSN    string `yaml:"dsn"`
	Checks []struct {
		Query     string  `yaml:"query"`
		Name      string  `yaml:"name"`
		Threshold float64 `yaml:"threshold"`
		Alert     string  `yaml:"alert"`
	} `yaml:"checks"`
}

type HTTPConfig struct {
	URL     string `yaml:"url"`
	Auth    string `yaml:"auth"`
	Timeout int    `yaml:"timeout"`
	Checks  []struct {
		JSONPath  string  `yaml:"json_path"`
		Name      string  `yaml:"name"`
		Threshold float64 `yaml:"threshold"`
		Alert     string  `yaml:"alert"`
	} `yaml:"checks"`
}

type LogConfig struct {
	Source        string   `yaml:"source"`
	Unit          string   `yaml:"unit"`
	FilePath      string   `yaml:"file_path"`
	Container     string   `yaml:"container"`
	ErrorPatterns []string `yaml:"error_patterns"`
	Minutes       int      `yaml:"minutes"`
}

type RuleConfig struct {
	Name       string `yaml:"name"`
	MetricName string `yaml:"metric_name"`
	Operator   string `yaml:"operator"`
	Threshold  float64 `yaml:"threshold"`
	Severity   string `yaml:"severity"`
	Message    string `yaml:"message"`
	Labels     map[string]string `yaml:"labels,omitempty"`
}

type AIConfig struct {
	Provider     string `yaml:"provider"` // "claude", "openai"
	APIKey       string `yaml:"api_key"`
	Model        string `yaml:"model"`
	SystemPrompt string `yaml:"system_prompt"`
	Enabled      bool   `yaml:"enabled"`
}

type AlertsConfig struct {
	Console  *struct{} `yaml:"console,omitempty"`
	Bark     *struct {
		ServerURL string   `yaml:"server_url"`
		Keys      []string `yaml:"keys"`
	} `yaml:"bark,omitempty"`
	Telegram *struct {
		Token   string  `yaml:"token"`
		ChatIDs []int64 `yaml:"chat_ids"`
	} `yaml:"telegram,omitempty"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	// Expand environment variables
	expanded := os.ExpandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Project == "" {
		return nil, fmt.Errorf("project name is required")
	}
	if cfg.Schedule == "" {
		cfg.Schedule = "*/30 * * * *"
	}

	return &cfg, nil
}
