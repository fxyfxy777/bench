package logger

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Config holds logging configuration.
type Config struct {
	Level       string        // control-plane log level (default "info")
	AccessLevel string        // request-path log level (default "info")
	Format      string        // "json" or "console"
	SlowRequest time.Duration // slow request warning threshold (default 30s)

	// File output — empty LogDir means stderr-only (dev mode).
	LogDir     string // directory for log files
	MaxSizeMB  int    // max size per log file before rotation in MB (default 500)
	MaxBackups int    // max rotated files to keep (default 5)
	MaxAgeDays int    // max age of rotated files in days (default 30)
	Compress   bool   // gzip compress rotated files

	// Access log sampling — controls how many log lines per second are emitted.
	// Both zero → disable sampling (emit all logs). Useful for low-QPS GPU workloads.
	SampleInitial    int // first N logs per second always emitted (default 100)
	SampleThereafter int // then 1-in-N per second (default 1000)

	// Independent audit log rotation (request_audit.log).
	AuditMaxSizeMB  int  // default: 1000
	AuditMaxBackups int  // default: 10
	AuditMaxAgeDays int  // default: 90
	AuditCompress   bool // default: true
}

// Bundle holds two independent loggers (control + access), per-module level
// controls, and resources that must be flushed on shutdown.
//
// Root  — control-plane: Tee(stderr, router.log) synchronous, no sampling.
// Access — request-path: BufferedWriteSyncer(access.log) + Sampler, truly async.
// Audit — forensics: BufferedWriteSyncer(request_audit.log), non-sampled, never dropped.
type Bundle struct {
	Root        *zap.Logger     // control-plane logger
	Access      *zap.Logger     // request-path logger (async, sampled)
	Level       zap.AtomicLevel // control-plane level (runtime-adjustable)
	AccessLevel zap.AtomicLevel // request-path level (runtime-adjustable)
	SlowRequest time.Duration

	accessBuf *zapcore.BufferedWriteSyncer // for flush on Close
	auditBuf  *zapcore.BufferedWriteSyncer // independent audit log (request_audit.log)
	closers   []io.Closer                 // lumberjack file handles

	mu     sync.RWMutex
	levels map[string]zap.AtomicLevel // per-module level overrides
}

// NewBundle creates a dual-logger bundle with independent control and access loggers.
//
// When LogDir is set (production): control → Tee(stderr, router.log), access → Buffered(access.log).
// When LogDir is empty (dev mode): control → sync stderr, access → Buffered(stderr).
// The two loggers are NEVER Tee'd together — access is truly async.
func NewBundle(cfg Config) (*Bundle, error) {
	controlLevel := zap.NewAtomicLevelAt(parseLevel(cfg.Level))
	accessLevel := zap.NewAtomicLevelAt(parseLevel(cfg.AccessLevel))
	slowReq := cfg.SlowRequest
	if slowReq <= 0 {
		slowReq = 30 * time.Second
	}

	// Resolve sampling defaults: both zero → no sampling (all logs emitted).
	sampleInitial := cfg.SampleInitial
	sampleThereafter := cfg.SampleThereafter
	if sampleInitial == 0 && sampleThereafter == 0 {
		// Explicit "no sampling" — leave both at 0 so buildAccessCore skips the sampler.
	} else {
		if sampleInitial <= 0 {
			sampleInitial = 100
		}
		if sampleThereafter <= 0 {
			sampleThereafter = 1000
		}
	}

	enc := newEncoder(cfg.Format)
	b := &Bundle{
		Level:       controlLevel,
		AccessLevel: accessLevel,
		SlowRequest: slowReq,
		levels:      make(map[string]zap.AtomicLevel),
	}

	if cfg.LogDir != "" {
		if err := b.buildFileMode(enc, controlLevel, accessLevel, sampleInitial, sampleThereafter, cfg); err != nil {
			return nil, err
		}
	} else {
		b.buildStderrMode(enc, controlLevel, accessLevel, sampleInitial, sampleThereafter)
	}

	return b, nil
}

// buildFileMode sets up file-backed loggers with lumberjack rotation.
// Control: Tee(sync stderr, sync router.log). Access: Buffered(access.log) + optional Sampler.
func (b *Bundle) buildFileMode(
	enc zapcore.Encoder,
	controlLevel, accessLevel zap.AtomicLevel,
	sampleInitial, sampleThereafter int,
	cfg Config,
) error {
	maxSize := cfg.MaxSizeMB
	if maxSize <= 0 {
		maxSize = 500
	}
	maxBackups := cfg.MaxBackups
	if maxBackups <= 0 {
		maxBackups = 5
	}
	maxAge := cfg.MaxAgeDays
	if maxAge <= 0 {
		maxAge = 30
	}

	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return err
	}

	// Control: Tee(stderr, router.log) — synchronous, guaranteed output.
	controlFile := &lumberjack.Logger{
		Filename:   filepath.Join(cfg.LogDir, "router.log"),
		MaxSize:    maxSize,
		MaxBackups: maxBackups,
		MaxAge:     maxAge,
		Compress:   cfg.Compress,
	}
	b.closers = append(b.closers, controlFile)

	stderrCore := zapcore.NewCore(enc, zapcore.Lock(os.Stderr), controlLevel)
	fileCore := zapcore.NewCore(enc, zapcore.AddSync(controlFile), controlLevel)
	controlTee := zapcore.NewTee(stderrCore, fileCore)
	b.Root = zap.New(controlTee, zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel))

	// Access: Buffered(access.log) + Sampler — truly async, no stderr, no Tee with control.
	accessFile := &lumberjack.Logger{
		Filename:   filepath.Join(cfg.LogDir, "access.log"),
		MaxSize:    maxSize,
		MaxBackups: maxBackups,
		MaxAge:     maxAge,
		Compress:   cfg.Compress,
	}
	b.closers = append(b.closers, accessFile)

	b.accessBuf = &zapcore.BufferedWriteSyncer{
		WS:            zapcore.AddSync(accessFile),
		Size:          256 * 1024,
		FlushInterval: time.Second,
	}
	accessCore := b.buildAccessCore(enc, b.accessBuf, accessLevel, sampleInitial, sampleThereafter)
	b.Access = zap.New(accessCore, zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel))

	// Audit: Buffered(request_audit.log) — non-sampled, independent from access.log.
	auditMaxSize := cfg.AuditMaxSizeMB
	if auditMaxSize <= 0 {
		auditMaxSize = 1000
	}
	auditMaxBackups := cfg.AuditMaxBackups
	if auditMaxBackups <= 0 {
		auditMaxBackups = 10
	}
	auditMaxAge := cfg.AuditMaxAgeDays
	if auditMaxAge <= 0 {
		auditMaxAge = 90
	}
	auditCompress := cfg.AuditCompress
	// Default to true if all audit fields are zero (not explicitly configured).
	if cfg.AuditMaxSizeMB == 0 && cfg.AuditMaxBackups == 0 && cfg.AuditMaxAgeDays == 0 {
		auditCompress = true
	}
	auditFile := &lumberjack.Logger{
		Filename:   filepath.Join(cfg.LogDir, "request_audit.log"),
		MaxSize:    auditMaxSize,
		MaxBackups: auditMaxBackups,
		MaxAge:     auditMaxAge,
		Compress:   auditCompress,
	}
	b.closers = append(b.closers, auditFile)
	b.auditBuf = &zapcore.BufferedWriteSyncer{
		WS:            zapcore.AddSync(auditFile),
		Size:          256 * 1024,
		FlushInterval: time.Second,
	}

	return nil
}

// buildStderrMode sets up stderr-only loggers (dev/test mode).
// Control: sync stderr. Access: Buffered(stderr) + optional Sampler — still async, still independent.
func (b *Bundle) buildStderrMode(enc zapcore.Encoder, controlLevel, accessLevel zap.AtomicLevel, sampleInitial, sampleThereafter int) {
	controlCore := zapcore.NewCore(enc, zapcore.Lock(os.Stderr), controlLevel)
	b.Root = zap.New(controlCore, zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel))

	b.accessBuf = &zapcore.BufferedWriteSyncer{
		WS:            zapcore.Lock(os.Stderr),
		Size:          256 * 1024,
		FlushInterval: time.Second,
	}
	accessCore := b.buildAccessCore(enc, b.accessBuf, accessLevel, sampleInitial, sampleThereafter)
	b.Access = zap.New(accessCore, zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel))
}

// buildAccessCore creates the access-path core with optional sampling.
// When sampleInitial and sampleThereafter are both 0, sampling is disabled entirely.
func (b *Bundle) buildAccessCore(enc zapcore.Encoder, ws zapcore.WriteSyncer, level zap.AtomicLevel, sampleInitial, sampleThereafter int) zapcore.Core {
	core := zapcore.NewCore(enc, ws, level)
	if sampleInitial == 0 && sampleThereafter == 0 {
		return core // no sampling — emit every log line
	}
	return zapcore.NewSamplerWithOptions(core, time.Second, sampleInitial, sampleThereafter)
}

// Sub creates a named control-plane sub-logger with per-module level control.
// The returned logger's effective level is controlled by its own AtomicLevel,
// adjustable at runtime via SetModuleLevel.
func (b *Bundle) Sub(name string) *zap.Logger {
	modLevel := b.registerModuleLevel(name, b.Level.Level())
	wrapped := &moduleCore{Core: b.Root.Core(), level: modLevel}
	return zap.New(wrapped, zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel)).Named(name)
}

// SubAccess creates a named access-path sub-logger with per-module level control.
// Derived from the Access logger — writes to access.log (or buffered stderr in dev mode).
func (b *Bundle) SubAccess(name string) *zap.Logger {
	modLevel := b.registerModuleLevel(name, b.AccessLevel.Level())
	wrapped := &moduleCore{Core: b.Access.Core(), level: modLevel}
	return zap.New(wrapped, zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel)).Named(name)
}

// SubAudit creates a non-sampled audit logger that writes to request_audit.log
// (in file mode) or buffered stderr (in dev mode). Bypasses any sampling core
// to ensure audit lines are NEVER dropped regardless of load — critical for
// forensic self-proof.
//
// The returned logger uses JSON encoding with caller info enabled.
func (b *Bundle) SubAudit(name string) *zap.Logger {
	// Prefer dedicated audit buffer; fall back to access buffer for dev mode.
	ws := b.auditBuf
	if ws == nil {
		ws = b.accessBuf
	}
	if ws == nil {
		// Fallback: no buffered writer (should not happen in normal usage).
		return b.Root.Named(name)
	}
	// Build a core directly on the audit writer, skipping the sampler.
	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	core := zapcore.NewCore(enc, ws, b.AccessLevel)
	return zap.New(core, zap.AddCaller()).Named(name)
}

// registerModuleLevel returns an existing module level or creates a new one.
func (b *Bundle) registerModuleLevel(name string, defaultLevel zapcore.Level) zap.AtomicLevel {
	b.mu.Lock()
	defer b.mu.Unlock()
	if lvl, exists := b.levels[name]; exists {
		return lvl
	}
	lvl := zap.NewAtomicLevelAt(defaultLevel)
	b.levels[name] = lvl
	return lvl
}

// ModuleLevel returns the AtomicLevel for a named module.
// Returns the control-plane level if the module doesn't exist.
func (b *Bundle) ModuleLevel(name string) zap.AtomicLevel {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if lvl, ok := b.levels[name]; ok {
		return lvl
	}
	return b.Level
}

// SetModuleLevel adjusts the level for a specific module at runtime.
// Returns false if the module hasn't been registered via Sub/SubAccess.
func (b *Bundle) SetModuleLevel(name string, lvl zapcore.Level) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if al, ok := b.levels[name]; ok {
		al.SetLevel(lvl)
		return true
	}
	return false
}

// Modules returns the registered module names and their current levels.
func (b *Bundle) Modules() map[string]string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	m := make(map[string]string, len(b.levels))
	for k, v := range b.levels {
		m[k] = v.Level().String()
	}
	return m
}

// Close flushes the buffered access writer, syncs both loggers, and closes
// file handles. Call this in main() defer.
func (b *Bundle) Close() error {
	var errs []error

	if b.accessBuf != nil {
		if err := b.accessBuf.Stop(); err != nil {
			errs = append(errs, err)
		}
	}
	if b.auditBuf != nil {
		if err := b.auditBuf.Stop(); err != nil {
			errs = append(errs, err)
		}
	}

	// Sync may return "bad file descriptor" or "inappropriate ioctl" on stderr;
	// these are harmless and platform-specific (common on macOS/Linux pipes).
	if err := ignoreSyncErr(b.Root.Sync()); err != nil {
		errs = append(errs, err)
	}
	if b.Access != nil {
		if err := ignoreSyncErr(b.Access.Sync()); err != nil {
			errs = append(errs, err)
		}
	}

	for _, c := range b.closers {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// ignoreSyncErr filters out harmless stderr sync errors that occur on
// macOS ("bad file descriptor") and Linux pipes ("inappropriate ioctl").
func ignoreSyncErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "bad file descriptor") ||
		strings.Contains(msg, "inappropriate ioctl") ||
		strings.Contains(msg, "invalid argument") {
		return nil
	}
	return err
}

// New creates a *zap.Logger based on the given level and format.
// Retained for backward compatibility with existing callers and tests.
func New(level, format string) (*zap.Logger, error) {
	lvl := parseLevel(level)

	var cfg zap.Config
	if format == "json" {
		cfg = zap.NewProductionConfig()
	} else {
		cfg = zap.NewDevelopmentConfig()
	}
	cfg.Level = zap.NewAtomicLevelAt(lvl)

	return cfg.Build()
}

func parseLevel(s string) zapcore.Level {
	var lvl zapcore.Level
	if err := lvl.UnmarshalText([]byte(s)); err != nil {
		return zapcore.InfoLevel
	}
	return lvl
}

func newEncoder(format string) zapcore.Encoder {
	if format == "json" {
		return zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	}
	return zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
}

// moduleCore wraps a zapcore.Core to apply a per-module level filter.
// This enables Sub/SubAccess loggers to have independent, runtime-adjustable levels.
type moduleCore struct {
	zapcore.Core
	level zap.AtomicLevel
}

func (c *moduleCore) Enabled(lvl zapcore.Level) bool {
	return c.level.Enabled(lvl)
}

func (c *moduleCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if !c.level.Enabled(ent.Level) {
		return ce
	}
	return c.Core.Check(ent, ce)
}

func (c *moduleCore) With(fields []zapcore.Field) zapcore.Core {
	return &moduleCore{Core: c.Core.With(fields), level: c.level}
}
