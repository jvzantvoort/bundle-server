package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Server ServerConfig          `mapstructure:"server"`
	Pools  map[string]PoolConfig `mapstructure:"pools"`
}

type ServerConfig struct {
	Address            string `mapstructure:"address"`
	PublicURL          string `mapstructure:"public_url"`
	APIBasePath        string `mapstructure:"api_base_path"`
	HealthPath         string `mapstructure:"health_path"`
	OpenAPIPath        string `mapstructure:"openapi_path"`
	SwaggerPath        string `mapstructure:"swagger_path"`
	BearerToken        string `mapstructure:"bearer_token"`
	TaskWorkers        int    `mapstructure:"task_workers"`
	MaxManifestEntries int    `mapstructure:"max_manifest_entries"`
	MaxFileBytes       int64  `mapstructure:"max_file_bytes"`
	MaxBundleBytes     int64  `mapstructure:"max_bundle_bytes"`
	UploadTTLSeconds   int    `mapstructure:"upload_ttl_seconds"`
}

type PoolConfig struct {
	Root  string `mapstructure:"root"`
	Title string `mapstructure:"title"`
}

func Load(path string) (Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	setDefaults(v)

	if err := v.ReadInConfig(); err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.address", ":8080")
	v.SetDefault("server.api_base_path", "/api/v1")
	v.SetDefault("server.health_path", "/api/v1/health")
	v.SetDefault("server.openapi_path", "/openapi.yaml")
	v.SetDefault("server.swagger_path", "/swagger/")
	v.SetDefault("server.task_workers", 1)
	v.SetDefault("server.max_manifest_entries", 100000)
	v.SetDefault("server.max_file_bytes", int64(10<<30))
	v.SetDefault("server.max_bundle_bytes", int64(1<<40))
	v.SetDefault("server.upload_ttl_seconds", 86400)
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Server.Address) == "" {
		return fmt.Errorf("server.address is required")
	}
	if c.Server.TaskWorkers < 1 {
		return fmt.Errorf("server.task_workers must be at least 1")
	}
	if c.Server.MaxManifestEntries < 1 || c.Server.MaxFileBytes < 1 || c.Server.MaxBundleBytes < 1 || c.Server.UploadTTLSeconds < 1 {
		return fmt.Errorf("server limits must be positive")
	}
	for name, route := range map[string]string{
		"api_base_path": c.Server.APIPath(),
		"health_path":   c.Server.HealthURL(),
		"openapi_path":  c.Server.OpenAPIURL(),
		"swagger_path":  c.Server.SwaggerURL(),
	} {
		if !strings.HasPrefix(route, "/") || strings.ContainsAny(route, "?#") {
			return fmt.Errorf("server.%s must be an absolute URL path", name)
		}
	}
	for name, pool := range c.Pools {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(pool.Root) == "" {
			return fmt.Errorf("every pool needs a name and root")
		}
	}
	return nil
}

func (s ServerConfig) APIPath() string {
	return route(s.APIBasePath, "/api/v1", false)
}

func (s ServerConfig) HealthURL() string {
	return route(s.HealthPath, "/api/v1/health", false)
}

func (s ServerConfig) OpenAPIURL() string {
	return route(s.OpenAPIPath, "/openapi.yaml", false)
}

func (s ServerConfig) SwaggerURL() string {
	return route(s.SwaggerPath, "/swagger/", true)
}

func route(value, fallback string, trailingSlash bool) string {
	if strings.TrimSpace(value) == "" {
		value = fallback
	}
	value = "/" + strings.TrimLeft(value, "/")
	if trailingSlash {
		return strings.TrimRight(value, "/") + "/"
	}
	return strings.TrimRight(value, "/")
}
