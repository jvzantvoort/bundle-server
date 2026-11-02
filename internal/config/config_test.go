package config

import "testing"

func TestValidateSchedules(t *testing.T) {
	cfg := validConfig()
	cfg.Schedules = []ScheduleConfig{{Name: "cache", Action: "cache_metadata", Interval: "15m", Pools: []string{"default"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsUnknownSchedulePool(t *testing.T) {
	cfg := validConfig()
	cfg.Schedules = []ScheduleConfig{{Name: "check", Action: "verify", Interval: "1h", Pools: []string{"missing"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted a schedule with an unknown pool")
	}
}

func validConfig() Config {
	return Config{
		Server: ServerConfig{
			Address: ":8080", TaskWorkers: 1, MaxManifestEntries: 1,
			MaxFileBytes: 1, MaxBundleBytes: 1, UploadTTLSeconds: 1,
		},
		Pools: map[string]PoolConfig{"default": {Root: "/tmp/bundles"}},
	}
}
