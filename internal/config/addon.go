package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// OptionsPath is where the Home Assistant Supervisor writes an add-on's
// configuration.
const OptionsPath = "/data/options.json"

// optionAliases covers the few option names that do not follow the general
// rule. Everything else maps by uppercasing: "camera_source" sets
// CAMERA_SOURCE, so the add-on schema and the environment stay in step without
// a translation table to maintain.
var optionAliases = map[string]string{
	"auth_mode":     "WEB_AUTH_MODE",
	"archive_path":  "ARCHIVE_DIR",
	"language":      "WEB_DEFAULT_LANGUAGE",
	"entity_prefix": "HA_ENTITY_PREFIX",
}

// LoadAddonOptions reads the Supervisor's options file and applies it to the
// environment, so the rest of the configuration is parsed exactly as it is for
// a plain container.
//
// Values already present in the environment win, which keeps the container's
// own environment authoritative and makes the add-on options a default layer
// rather than an override. A missing file is not an error: that is simply the
// normal, non-add-on case.
func LoadAddonOptions(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	var options map[string]any
	if err := json.Unmarshal(raw, &options); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}

	for key, value := range options {
		name := envNameFor(key)
		if name == "" {
			continue
		}
		if _, alreadySet := os.LookupEnv(name); alreadySet {
			continue
		}
		text, ok := optionString(value)
		if !ok {
			continue
		}
		// An empty option means "not configured"; setting it would shadow a
		// default that is more useful than the empty string.
		if strings.TrimSpace(text) == "" {
			continue
		}
		if err := os.Setenv(name, text); err != nil {
			return fmt.Errorf("applying option %q: %w", key, err)
		}
	}
	return nil
}

// envNameFor maps an option key to its environment variable.
func envNameFor(key string) string {
	if alias, ok := optionAliases[key]; ok {
		return alias
	}
	upper := strings.ToUpper(key)
	for _, r := range upper {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return ""
		}
	}
	return upper
}

// optionString renders a JSON value the way the environment expects it.
// Nested objects and arrays are skipped: the schema is deliberately flat.
func optionString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		// JSON has no integers; render whole numbers without a decimal point so
		// durations and counts parse cleanly.
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10), true
		}
		return strconv.FormatFloat(v, 'f', -1, 64), true
	default:
		return "", false
	}
}
