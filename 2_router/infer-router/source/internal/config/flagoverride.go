package config

import (
	"flag"
	"time"
)

// FlagPtrs holds the raw pointers returned by flag.XXX() calls.
// ApplyCLIOverrides uses these together with fs.Visit() to detect which
// flags were explicitly set on the command line.
type FlagPtrs struct {
	Mode              *string
	ListenAddr        *string
	AdminAddr         *string
	GRPCAddr          *string
	SchedulerAddr     *string
	Policy            *string
	LogLevel          *string
	LogFormat         *string
	LogDir            *string
	AdvertiseAddr     *string
	HeartbeatInterval *time.Duration
	HeartbeatTimeout  *time.Duration
	ShutdownGrace     *time.Duration
	EnableH2C         *bool
	BackendH2C        *bool
	Splitwise         *bool
}

// ApplyCLIOverrides overwrites cfg fields only for flags that were explicitly
// provided on the command line. This preserves the priority chain:
//
//	CLI flag  >  config file  >  defaults
func ApplyCLIOverrides(cfg *Config, fs *flag.FlagSet, ptrs *FlagPtrs) {
	explicit := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	if explicit["mode"] {
		cfg.Mode = Mode(*ptrs.Mode)
	}
	if explicit["listen"] {
		cfg.ListenAddr = *ptrs.ListenAddr
	}
	if explicit["admin-listen"] {
		cfg.AdminAddr = *ptrs.AdminAddr
	}
	if explicit["grpc"] {
		cfg.GRPCAddr = *ptrs.GRPCAddr
	}
	if explicit["scheduler-addr"] {
		cfg.SchedulerAddr = *ptrs.SchedulerAddr
	}
	if explicit["policy"] {
		cfg.Policy = *ptrs.Policy
	}
	if explicit["log-level"] {
		cfg.Log.Level = *ptrs.LogLevel
	}
	if explicit["log-format"] {
		cfg.Log.Format = *ptrs.LogFormat
	}
	if explicit["log-dir"] {
		cfg.Log.LogDir = *ptrs.LogDir
	}
	if explicit["advertise-addr"] {
		cfg.AdvertiseAddr = *ptrs.AdvertiseAddr
	}
	if explicit["heartbeat-interval"] {
		cfg.HeartbeatInterval = Dur(*ptrs.HeartbeatInterval)
	}
	if explicit["heartbeat-timeout"] {
		cfg.HeartbeatTimeout = Dur(*ptrs.HeartbeatTimeout)
	}
	if explicit["shutdown-grace"] {
		cfg.ShutdownGrace = Dur(*ptrs.ShutdownGrace)
	}
	if explicit["enable-h2c"] {
		cfg.EnableH2C = *ptrs.EnableH2C
	}
	if explicit["backend-h2c"] {
		cfg.BackendH2C = *ptrs.BackendH2C
	}
	if explicit["splitwise"] {
		cfg.Splitwise.Enabled = *ptrs.Splitwise
	}
}
