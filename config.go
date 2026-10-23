package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The file is JSON because JSON is the only human-readable format in the Go standard library, and
// the relay has no dependencies.
type config struct {
	Listen    string                 `json:"listen,omitempty"`
	Upstream  string                 `json:"upstream"`
	Cache     string                 `json:"cache,omitempty"`
	Overrides map[string]modelConfig `json:"overrides,omitempty"`

	// Only the -debug flag sets it, so that a config file never leaves recording on.
	debug bool
}

// A nil or empty field keeps the value from LiteLLM, or the global cache mode. An empty effort list
// is different from nil: it turns off effort.
type modelConfig struct {
	Cache          string   `json:"cache,omitempty"`
	Name           string   `json:"name,omitempty"`
	MaxInputTokens *int64   `json:"max_input_tokens,omitempty"`
	MaxTokens      *int64   `json:"max_tokens,omitempty"`
	Images         *bool    `json:"images,omitempty"`
	Thinking       *bool    `json:"thinking,omitempty"`
	Effort         []string `json:"effort,omitempty"`
}

var zedEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

func defaultConfig() config {
	return config{
		Listen:    "127.0.0.1:8787",
		Cache:     cacheAuto,
		Overrides: map[string]modelConfig{},
	}
}

func (cfg config) newAdapter() adapter {
	return adapter{cache: cfg.Cache, overrides: cfg.Overrides}
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "zed-litellm", "config.json")
}

func shortPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return filepath.Join("~", rest)
	}
	return path
}

// The XDG spec puts logs in the state folder.
func recordDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "zed-litellm", "records")
}

// The returned function sets only the flags that the command line gives, so that the file keeps all
// other values. Overrides do not fit on a command line, so they have no flag.
func bindConfigFlags(fs *flag.FlagSet) func(*config) {
	d := defaultConfig()
	listen := fs.String("listen", d.Listen, "address to listen on")
	upstream := fs.String("upstream", "", "LiteLLM base URL (required, here or in the config file)")
	cache := fs.String("cache", d.Cache, "TTL of the conversation cache: auto, 1h or bypass")
	debug := fs.Bool("debug", false, "log each request, and record it to "+shortPath(recordDir()))

	return func(cfg *config) {
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "listen":
				cfg.Listen = *listen
			case "upstream":
				cfg.Upstream = *upstream
			case "cache":
				cfg.Cache = *cache
			case "debug":
				cfg.debug = *debug
			}
		})
	}
}

func loadConfig(path string, applyFlags func(*config)) (config, error) {
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return cfg, err
	default:
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	}

	applyFlags(&cfg)
	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (cfg config) validate() error {
	if cfg.Upstream == "" {
		return errors.New("no upstream: pass -upstream <LiteLLM URL>, or set upstream in the config file")
	}
	if _, err := parseUpstream(cfg.Upstream); err != nil {
		return err
	}

	if !slices.Contains(cacheModes, cfg.Cache) {
		return fmt.Errorf("cache %q is not one of %v", cfg.Cache, cacheModes)
	}
	for id, mc := range cfg.Overrides {
		if mc.Cache != "" && !slices.Contains(cacheModes, mc.Cache) {
			return fmt.Errorf("overrides.%s: cache %q is not one of %v", id, mc.Cache, cacheModes)
		}
		for _, level := range mc.Effort {
			if !slices.Contains(zedEffortLevels, level) {
				return fmt.Errorf("overrides.%s: effort %q is not one of %v", id, level, zedEffortLevels)
			}
		}
	}
	return nil
}

func parseUpstream(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid upstream %q", s)
	}
	return u, nil
}

func initConfig(path string, applyFlags func(*config)) error {
	merged := defaultConfig()
	applyFlags(&merged)
	if err := merged.validate(); err != nil {
		return err
	}
	var cfg config
	applyFlags(&cfg)
	data, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
