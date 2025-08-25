package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type ServerConf struct {
	Addr          string `yaml:"addr"`
	PublicBaseURL string `yaml:"public_base_url"`
}

type DBConf struct {
	Driver      string `yaml:"driver"` // "sqlite" or "postgres"
	SQLitePath  string `yaml:"sqlite_path"`
	PostgresDSN string `yaml:"postgres_dsn"`
}

type RekorImagesConf struct {
	FetchEnabled bool   `yaml:"fetch_enabled"`
	BaseURL      string `yaml:"base_url"`
	AuthHeader   string `yaml:"auth_header"`
	PathTemplate string `yaml:"path_template"` // NEW: e.g. "/img/{agent_uid}/{uuid}.jpeg?api_key=${REKOR_API_KEY}"
}

type RekorShareDefaults struct {
	Plate      bool `yaml:"plate"`
	Timestamp  bool `yaml:"timestamp"`
	CameraName bool `yaml:"camera_name"`
	Confidence bool `yaml:"confidence"`
	GPS        bool `yaml:"gps"`
	Images     bool `yaml:"images"`
}

type RekorProviderConf struct {
	Enabled       bool               `yaml:"enabled"`
	Token         string             `yaml:"token"`
	ShareDefaults RekorShareDefaults `yaml:"share_defaults"`
	IndexFields   []string           `yaml:"index_fields"`
	Images        RekorImagesConf    `yaml:"images"`
}

type ProvidersConf struct {
	Rekor RekorProviderConf `yaml:"rekor"`
}

type Config struct {
	Server    ServerConf    `yaml:"server"`
	Database  DBConf        `yaml:"database"`
	Providers ProvidersConf `yaml:"providers"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
