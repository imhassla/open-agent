// Package config loads runtime configuration, primarily the OpenRouter key.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	OpenRouterKey string
}

// Load resolves the OpenRouter key from (first match wins):
// an already-set env var, a .env in the working directory, or
// ~/.config/open-agent/.env. Values already in the environment are
// never overwritten by a .env file.
func Load() (*Config, error) {
	loadDotEnv(".env")
	if home, err := os.UserHomeDir(); err == nil {
		loadDotEnv(filepath.Join(home, ".config", "open-agent", ".env"))
	}

	key := firstNonEmpty(os.Getenv("OPENROUTER_KEY"), os.Getenv("OPENROUTER_API_KEY"))
	if key == "" {
		return nil, fmt.Errorf("OPENROUTER_KEY not found in env or .env")
	}
	return &Config{OpenRouterKey: key}, nil
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
