package database

import (
	"testing"
	"time"
)

func TestParsePostgresPoolConfigDefaultsOverridesAndClamping(t *testing.T) {
	defaults := parsePostgresPoolConfig(func(string) string { return "" })
	if defaults.maxOpenConns != 50 || defaults.maxIdleConns != 25 || defaults.connMaxLifetime != 30*time.Minute || defaults.connMaxIdleTime != 10*time.Minute {
		t.Fatalf("defaults = %+v, want {50 25 30m 10m}", defaults)
	}

	customEnv := map[string]string{
		"CANVAS_DB_MAX_OPEN_CONNS":     "120",
		"CANVAS_DB_MAX_IDLE_CONNS":     "40",
		"CANVAS_DB_CONN_MAX_LIFETIME":  "45m",
		"CANVAS_DB_CONN_MAX_IDLE_TIME": "12m",
	}
	custom := parsePostgresPoolConfig(func(k string) string { return customEnv[k] })
	if custom.maxOpenConns != 120 || custom.maxIdleConns != 40 || custom.connMaxLifetime != 45*time.Minute || custom.connMaxIdleTime != 12*time.Minute {
		t.Fatalf("custom = %+v, want {120 40 45m 12m}", custom)
	}

	// Positive integer seconds fallback for lifetime & idle time (e.g. "1800" -> 30m, "600" -> 10m; "360" -> 6m) and explicit 0 idle conns.
	secondsEnv := map[string]string{
		"CANVAS_DB_MAX_OPEN_CONNS":     "80",
		"CANVAS_DB_MAX_IDLE_CONNS":     "0",
		"CANVAS_DB_CONN_MAX_LIFETIME":  "1800",
		"CANVAS_DB_CONN_MAX_IDLE_TIME": "360",
	}
	secondsCfg := parsePostgresPoolConfig(func(k string) string { return secondsEnv[k] })
	if secondsCfg.maxOpenConns != 80 || secondsCfg.maxIdleConns != 0 || secondsCfg.connMaxLifetime != 30*time.Minute || secondsCfg.connMaxIdleTime != 6*time.Minute {
		t.Fatalf("secondsCfg = %+v, want {80 0 30m 6m}", secondsCfg)
	}

	// Clamping: maxOpen > 500 -> 500; maxIdle > maxOpen -> maxOpen; maxIdleTime > maxLifetime -> maxLifetime.
	clampHighEnv := map[string]string{
		"CANVAS_DB_MAX_OPEN_CONNS":     "999",
		"CANVAS_DB_MAX_IDLE_CONNS":     "800",
		"CANVAS_DB_CONN_MAX_LIFETIME":  "5m",
		"CANVAS_DB_CONN_MAX_IDLE_TIME": "20m",
	}
	clampedHigh := parsePostgresPoolConfig(func(k string) string { return clampHighEnv[k] })
	if clampedHigh.maxOpenConns != 500 || clampedHigh.maxIdleConns != 500 || clampedHigh.connMaxLifetime != 5*time.Minute || clampedHigh.connMaxIdleTime != 5*time.Minute {
		t.Fatalf("clampedHigh = %+v, want {500 500 5m 5m}", clampedHigh)
	}

	// Small maxOpen clamps default maxIdle (25 -> 12).
	smallOpen := parsePostgresPoolConfig(func(k string) string {
		if k == "CANVAS_DB_MAX_OPEN_CONNS" {
			return "12"
		}
		return ""
	})
	if smallOpen.maxOpenConns != 12 || smallOpen.maxIdleConns != 12 {
		t.Fatalf("smallOpen = %+v, want maxOpen=12 maxIdle=12", smallOpen)
	}

	// Invalid negative or garbage values fall back to safe defaults.
	for name, env := range map[string]map[string]string{
		"negative": {
			"CANVAS_DB_MAX_OPEN_CONNS":     "-10",
			"CANVAS_DB_MAX_IDLE_CONNS":     "-5",
			"CANVAS_DB_CONN_MAX_LIFETIME":  "-10m",
			"CANVAS_DB_CONN_MAX_IDLE_TIME": "-2m",
		},
		"zero_open_and_durations": {
			"CANVAS_DB_MAX_OPEN_CONNS":     "0",
			"CANVAS_DB_CONN_MAX_LIFETIME":  "0s",
			"CANVAS_DB_CONN_MAX_IDLE_TIME": "0",
		},
		"garbage": {
			"CANVAS_DB_MAX_OPEN_CONNS":     "not-a-number",
			"CANVAS_DB_MAX_IDLE_CONNS":     "oops",
			"CANVAS_DB_CONN_MAX_LIFETIME":  "forever",
			"CANVAS_DB_CONN_MAX_IDLE_TIME": "bad",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := parsePostgresPoolConfig(func(k string) string { return env[k] })
			if got.maxOpenConns != 50 || got.maxIdleConns != 25 || got.connMaxLifetime != 30*time.Minute || got.connMaxIdleTime != 10*time.Minute {
				t.Fatalf("%s = %+v, want defaults {50 25 30m 10m}", name, got)
			}
		})
	}
}

func TestConfigurePoolSQLiteKeepsLocalLimits(t *testing.T) {
	t.Setenv("CANVAS_DB_MAX_OPEN_CONNS", "120")
	t.Setenv("CANVAS_DB_MAX_IDLE_CONNS", "60")
	db, err := Open(Config{Driver: "sqlite", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	if err := ConfigurePool(db); err != nil {
		t.Fatalf("ConfigurePool sqlite: %v", err)
	}
	if stats := sqlDB.Stats(); stats.MaxOpenConnections != 8 {
		t.Fatalf("sqlite MaxOpenConnections = %d, want 8", stats.MaxOpenConnections)
	}
}
