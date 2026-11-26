package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Port     int            `yaml:"port"`
	Debug    bool           `yaml:"-"`
	TLS      TLSConfig      `yaml:"tls"`
	Postgres PostgresConfig `yaml:"postgres"`
	Redis    RedisConfig    `yaml:"redis"`
	Renewal  RenewalConfig  `yaml:"renewal"`
	Catalog  CatalogConfig  `yaml:"catalog"`
}

type CatalogConfig struct {
	File     string `yaml:"file"`
	CostToBu int32  `yaml:"cost_to_bu"`
}

type TLSConfig struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
}

type PostgresConfig struct {
	URL string `yaml:"url"`
}

type RedisConfig struct {
	URL string `yaml:"url"`
}

// RenewalConfig configures the in-process budget renewal sweep and
// reconcile loop. Zero values fall back to the renewal package's defaults
// (30s sweep interval, 10m reconcile interval, 100 budgets per pass).
type RenewalConfig struct {
	// Interval is the sweep cadence. Budget periods are day-to-month scale,
	// so a renewal landing an interval after the boundary is fine — it
	// fails closed (briefly denying traffic that should now be allowed),
	// the safe direction.
	Interval time.Duration `yaml:"interval"`
	// ReconcileInterval is the cadence of the self-heal that rebuilds
	// balances and flags in Redis from the usage ledger. It bounds how
	// long drift between the ledger and the Redis counters (or a Redis
	// data loss) can persist: startup reconcile aside, a lost or drifted
	// key is repaired within one reconcile interval.
	ReconcileInterval time.Duration `yaml:"reconcileInterval"`
	// BatchSize caps the budgets processed per sweep or reconcile pass;
	// the next pass picks up whatever is left.
	BatchSize int `yaml:"batchSize"`
}

func Load(dir string) (*Config, error) {
	cfg := &Config{
		Port: 8080,
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("listing config files in %s: %w", dir, err)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no config files found in %s", dir)
	}

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("reading config %s: %w", f, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config %s: %w", f, err)
		}
		log.Printf("loaded config from %s", f)
	}

	return cfg, nil
}
