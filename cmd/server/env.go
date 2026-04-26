package main

import (
	"log"
	"os"
	"strconv"
	"strings"
)

// envInt parses an int environment variable, falling back to
// defaultValue when the value is missing or unparseable. Negative
// values are rejected so callers can rely on a sane minimum without
// extra clamping at every call site.
func envInt(name string, defaultValue int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return defaultValue
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Printf("warning: %s=%q is not a valid non-negative integer; using default %d", name, v, defaultValue)
		return defaultValue
	}
	return n
}

// envBool parses a boolean environment variable. Missing or unparseable
// values fall back to defaultValue. Accepted truthy values: "true", "1",
// "yes", "on" (case-insensitive); accepted falsy: "false", "0", "no", "off".
func envBool(name string, defaultValue bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return defaultValue
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		log.Printf("warning: %s=%q is not a valid boolean; using default %v", name, v, defaultValue)
		return defaultValue
	}
}
