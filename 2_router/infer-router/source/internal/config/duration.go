package config

import (
	"fmt"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration to support YAML unmarshalling from strings like "5s"
// and bare numbers (interpreted as seconds).
type Duration struct {
	Duration time.Duration
}

func (d Duration) MarshalYAML() (any, error) {
	return d.Duration.String(), nil
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	// Try bare number first (YAML int/float tags).
	var secs float64
	if node.Tag == "!!int" || node.Tag == "!!float" {
		if err := node.Decode(&secs); err == nil {
			d.Duration = time.Duration(secs * float64(time.Second))
			return nil
		}
	}

	// Otherwise treat as string, e.g. "5s", "100ms".
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("cannot parse duration: expected string like \"5s\" or number of seconds")
	}

	// A string that looks like a plain number (e.g. "10") — treat as seconds.
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		d.Duration = time.Duration(f * float64(time.Second))
		return nil
	}

	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// Dur is a convenience constructor.
func Dur(d time.Duration) Duration {
	return Duration{Duration: d}
}
