package config

import (
	"log/slog"
	"slices"
	"strings"
)

// EnvPrefix starts the names of all environment variables of StackBelay.
const EnvPrefix = "STACKBELAY_"

// LegacyEnvPrefix started the variable names before StackVault was renamed
// to StackBelay (issue #44). Setups from that time still set them, so they
// are read as a fallback; the log warns about every one that was used.
const LegacyEnvPrefix = "STACKVAULT_"

// Env reads environment variables and falls back to the legacy name:
// Get("STACKBELAY_X") returns $STACKBELAY_X, or $STACKVAULT_X if the new
// variable is empty or unset. Like a decorator around
// Environment.GetEnvironmentVariable in C#.
type Env struct {
	getenv func(string) string
	legacy []string
}

// NewEnv wraps getenv (usually os.Getenv).
func NewEnv(getenv func(string) string) *Env {
	return &Env{getenv: getenv}
}

// Get returns the value of name, see Env. Names without EnvPrefix are read
// as they are.
func (e *Env) Get(name string) string {
	if v := e.getenv(name); v != "" {
		return v
	}
	rest, ok := strings.CutPrefix(name, EnvPrefix)
	if !ok {
		return ""
	}
	old := LegacyEnvPrefix + rest
	v := e.getenv(old)
	if v != "" && !slices.Contains(e.legacy, old) {
		e.legacy = append(e.legacy, old)
	}
	return v
}

// Legacy returns the legacy names that Get has used so far, in the order
// of their first use.
func (e *Env) Legacy() []string {
	return slices.Clone(e.legacy)
}

// WarnLegacy logs a warning for every legacy name that Get has used.
func (e *Env) WarnLegacy(log *slog.Logger) {
	for _, old := range e.legacy {
		log.Warn("deprecated environment variable, rename it (StackVault was renamed to StackBelay)",
			"name", old, "new_name", EnvPrefix+strings.TrimPrefix(old, LegacyEnvPrefix))
	}
}
