package main

import (
	"log"
	"os"
	"strings"
)

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
