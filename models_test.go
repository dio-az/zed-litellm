package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A real /model/info response with only the fields that the relay reads: a Claude chat model
// (Bedrock), a GPT chat model (Azure), a Gemini chat model (Vertex) and an embedding model.
func fixtureModelInfo(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("model_info.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func flagsFrom(t *testing.T, args ...string) func(*config) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	apply := bindConfigFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return apply
}

func serveModelList(t *testing.T, overrides map[string]modelConfig) modelList {
	t.Helper()
	body := fixtureModelInfo(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/info" || r.Header.Get("X-Api-Key") != "sk-test" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	file, _ := json.Marshal(map[string]any{"upstream": upstream.URL, "overrides": overrides})
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path, flagsFrom(t))
	if err != nil {
		t.Fatal(err)
	}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models?limit=1000", nil)
	req.Header.Set("X-Api-Key", "sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var list modelList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func TestModelListIsSortedAndNamedByOverrides(t *testing.T) {
	list := serveModelList(t, map[string]modelConfig{"gpt-6.1-sol": {Name: "GPT-6.1 Sol"}, "not-in-litellm": {}})
	var got []string
	for _, m := range list.Data {
		got = append(got, m.ID+"="+m.DisplayName)
	}
	want := []string{"claude-opus-5-5=claude-opus-5-5", "gemini-3.8-flash=gemini-3.8-flash", "gpt-6.1-sol=GPT-6.1 Sol"}
	if !slices.Equal(got, want) {
		t.Errorf("models = %v, want %v", got, want)
	}
}

func TestOverridesChangeCapabilities(t *testing.T) {
	list := serveModelList(t, map[string]modelConfig{
		"gemini-3.8-flash": {MaxTokens: new(int64(64000)), Images: new(false), Effort: []string{"low", "xhigh"}},
		"gpt-6.1-sol":      {Thinking: new(false)},
	})
	if len(list.Data) != 3 {
		t.Fatalf("got %d models, want all 3 chat models", len(list.Data))
	}
	byID := map[string]anthropicModel{}
	for _, m := range list.Data {
		byID[m.ID] = m
	}
	flash, sol := byID["gemini-3.8-flash"], byID["gpt-6.1-sol"]

	if flash.MaxTokens != 64000 || flash.MaxInputTokens != 1048576 {
		t.Errorf("flash tokens = %d/%d, want the override and LiteLLM's input limit", flash.MaxInputTokens, flash.MaxTokens)
	}
	c := flash.Capabilities
	if c.ImageInput.Supported || !c.Thinking.Supported {
		t.Errorf("flash images/thinking = %v/%v, want false/true", c.ImageInput.Supported, c.Thinking.Supported)
	}
	if !c.Effort.Low.Supported || !c.Effort.XHigh.Supported || c.Effort.Medium.Supported || c.Effort.High.Supported {
		t.Errorf("flash effort = %+v, want exactly low and xhigh", c.Effort)
	}

	if c := sol.Capabilities; c.Thinking.Supported || c.Thinking.Types.Adaptive.Supported || c.Effort.Supported {
		t.Errorf("sol with thinking off = %+v, want no thinking and no effort", c)
	}
}

func TestOverridesListAModelWithoutTokenLimits(t *testing.T) {
	entries := []modelInfoEntry{{ModelName: "custom", ModelInfo: litellmModelInfo{Mode: "chat"}}}
	got := buildModelList(entries, map[string]modelConfig{"custom": {MaxInputTokens: new(int64(200000)), MaxTokens: new(int64(8000))}})
	if len(got) != 1 || got[0].MaxInputTokens != 200000 || got[0].MaxTokens != 8000 {
		t.Errorf("got %+v, want custom with the configured limits", got)
	}
}

func TestEffortLevelsFollowLiteLLMRules(t *testing.T) {
	cases := map[string]struct {
		info litellmModelInfo
		want []string
	}{
		"no flags":           {litellmModelInfo{}, []string{"low", "medium", "high"}},
		"low refused":        {litellmModelInfo{SupportsLow: new(false)}, []string{"medium", "high"}},
		"xhigh and max":      {litellmModelInfo{SupportsXHigh: true, SupportsMax: true}, zedEffortLevels},
		"declared wins":      {litellmModelInfo{SupportsXHigh: true, EffortLevels: []string{"low", "high", "max"}}, []string{"low", "high", "max"}},
		"declared nothing":   {litellmModelInfo{SupportsXHigh: true, EffortLevels: []string{}}, nil},
		"unknown levels cut": {litellmModelInfo{EffortLevels: []string{"minimal", "none", "high"}}, []string{"high"}},
	}
	for name, tc := range cases {
		tc.info.SupportsReasoning = true
		tc.info.MaxInputTokens, tc.info.MaxTokens = new(int64(1000)), new(int64(1000))
		m, _ := toAnthropicModel("m", tc.info, modelConfig{})
		e := m.Capabilities.Effort
		var got []string
		for i, s := range []supported{e.Low, e.Medium, e.High, e.XHigh, e.Max} {
			if s.Supported {
				got = append(got, zedEffortLevels[i])
			}
		}
		if !slices.Equal(got, tc.want) || e.Supported != (len(tc.want) > 0) {
			t.Errorf("%s: effort %v (supported %v), want %v", name, got, e.Supported, tc.want)
		}
	}
}

func TestModelListHasOnlyChatModels(t *testing.T) {
	if list := serveModelList(t, nil); len(list.Data) != 3 {
		t.Errorf("got %d models, want the 3 chat models without the embedding model", len(list.Data))
	}
}

func TestModelCapabilitiesComeFromLiteLLM(t *testing.T) {
	type want struct {
		maxIn, maxOut int64
		xhigh, max    bool
	}
	cases := map[string]want{
		// For Claude, the values are the same as in the /v1/models of Anthropic.
		"claude-opus-5-5": {1000000, 128000, true, true},
		"gpt-6.1-sol":     {1050000, 128000, true, true},
		// LiteLLM refuses xhigh for Gemini.
		"gemini-3.8-flash": {1048576, 65536, false, false},
	}
	for _, m := range serveModelList(t, nil).Data {
		w, ok := cases[m.ID]
		if !ok {
			continue
		}
		c := m.Capabilities
		if m.MaxInputTokens != w.maxIn || m.MaxTokens != w.maxOut {
			t.Errorf("%s tokens = %d/%d, want %d/%d", m.ID, m.MaxInputTokens, m.MaxTokens, w.maxIn, w.maxOut)
		}
		if !c.ImageInput.Supported || !c.Thinking.Types.Adaptive.Supported || !c.Effort.High.Supported {
			t.Errorf("%s lacks images, adaptive thinking or high effort: %+v", m.ID, c)
		}
		if c.Effort.XHigh.Supported != w.xhigh || c.Effort.Max.Supported != w.max {
			t.Errorf("%s xhigh/max = %v/%v, want %v/%v", m.ID, c.Effort.XHigh.Supported, c.Effort.Max.Supported, w.xhigh, w.max)
		}
	}
}

func TestModelInfoRedirectDoesNotGetTheKey(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect target got the request, with X-Api-Key %q", r.Header.Get("X-Api-Key"))
	}))
	defer other.Close()
	upstream := httptest.NewServer(http.RedirectHandler(other.URL+"/login", http.StatusFound))
	defer upstream.Close()

	cfg, err := loadConfig(filepath.Join(t.TempDir(), "missing.json"), flagsFrom(t, "-upstream", upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Api-Key", "sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Errorf("status %d, want the redirect status for Zed", rec.Code)
	}
}

func TestDeploymentOrderDoesNotChangeCapabilities(t *testing.T) {
	wide := litellmModelInfo{Mode: "chat", MaxInputTokens: new(int64(1000000)), MaxOutputTokens: new(int64(128000)),
		SupportsVision: true, SupportsReasoning: true, SupportsXHigh: true, SupportsMax: true}
	narrow := litellmModelInfo{Mode: "chat", MaxInputTokens: new(int64(200000)), MaxTokens: new(int64(64000)),
		SupportsReasoning: true, EffortLevels: []string{"medium", "high"}}
	want := anthropicModel{Type: "model", ID: "m", DisplayName: "m", MaxInputTokens: 200000, MaxTokens: 64000}
	want.Capabilities.Thinking.Supported = true
	want.Capabilities.Thinking.Types.Adaptive.Supported = true
	want.Capabilities.Effort = effort{Supported: true, Medium: supported{true}, High: supported{true}}

	for _, order := range [][]litellmModelInfo{{wide, narrow}, {narrow, wide}} {
		entries := []modelInfoEntry{{"m", order[0]}, {"m", order[1]}}
		if got := buildModelList(entries, nil); len(got) != 1 || got[0] != want {
			t.Errorf("got %+v, want only what both deployments support: %+v", got, want)
		}
	}
}

func TestModelWithoutTokenLimitsIsDropped(t *testing.T) {
	entries := []modelInfoEntry{{ModelName: "tts", ModelInfo: litellmModelInfo{Mode: "chat"}}}
	if got := buildModelList(entries, nil); len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

func TestInitWritesLoadableConfigOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zed-litellm", "config.json")
	if err := initConfig(path, flagsFrom(t, "-upstream", "https://litellm.example.com", "-cache", "1h", "-debug")); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path, flagsFrom(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream != "https://litellm.example.com" || cfg.Cache != cache1h || cfg.Listen != defaultConfig().Listen {
		t.Errorf("init wrote %+v", cfg)
	}
	if cfg.debug {
		t.Error("init saved -debug, so the service would record forever")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(saved)); !slices.Equal(keys, []string{"cache", "upstream"}) {
		t.Errorf("init saved %v, want only the given flags, so new defaults reach old configs", keys)
	}
	if err := initConfig(path, flagsFrom(t)); err == nil {
		t.Error("init overwrote an existing config")
	}
}

func TestConfigRejectsTypos(t *testing.T) {
	for _, file := range []string{
		`{"no_cache_model": ["x"]}`,
		`{"overrides": {"m": {"max_output_tokens": 1}}}`,
		`{"overrides": {"m": {"effort": ["hihg"]}}}`,
		`{"overrides": {"m": {"cache": "off"}}}`,
		`{"cache": "5m"}`,
		`{"models": []}`,
		`{"verbose": true}`,
		`{"debug": true}`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path, flagsFrom(t)); err == nil {
			t.Errorf("%s was accepted", file)
		}
	}
}

func TestCacheModeComesFromOverrideThenGlobal(t *testing.T) {
	cases := map[string]struct {
		file string
		args []string
		want string // The conversation marker of a long Claude thread.
	}{
		"defaults":              {`{}`, nil, `{"type":"ephemeral","ttl":"1h"}`},
		"global bypass":         {`{}`, []string{"-cache", "bypass"}, `{"type":"ephemeral"}`},
		"override beats global": {`{"cache": "bypass", "overrides": {"claude-opus-5-5": {"cache": "1h"}}}`, nil, `{"type":"ephemeral","ttl":"1h"}`},
		"override for another":  {`{"cache": "bypass", "overrides": {"gpt-6.1-sol": {"cache": "1h"}}}`, nil, `{"type":"ephemeral"}`},
	}
	for name, tc := range cases {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(tc.file), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig(path, flagsFrom(t, append([]string{"-upstream", "http://litellm:4000"}, tc.args...)...))
		if err != nil {
			t.Fatal(err)
		}
		out, _ := cfg.newAdapter().adapt([]byte(conversation(20, 1000, 500, `{"type":"ephemeral"}`, `,"ttl":"1h"`)))
		last := out[bytes.LastIndex(out, []byte(`"cache_control":`))+len(`"cache_control":`):]
		if got := string(last[:bytes.IndexByte(last, '}')+1]); got != tc.want {
			t.Errorf("%s: marker %s, want %s", name, got, tc.want)
		}
	}
}

func TestGivenFlagsReplaceTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	file := `{"listen": "127.0.0.1:9000", "upstream": "http://from-file:4000", "cache": "bypass"}`
	if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path, flagsFrom(t, "-upstream", "https://from-flag.example.com", "-cache", "1h", "-debug"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream != "https://from-flag.example.com" || cfg.Cache != cache1h || !cfg.debug {
		t.Errorf("flags did not replace the file: %+v", cfg)
	}
	if cfg.Listen != "127.0.0.1:9000" {
		t.Errorf("listen = %s; a flag that was not given replaced the file", cfg.Listen)
	}
	if _, err := loadConfig(path, flagsFrom(t, "-upstream", "not a url")); err == nil {
		t.Error("an invalid -upstream was accepted")
	}
}

func TestUpstreamIsRequired(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	if _, err := loadConfig(missing, flagsFrom(t)); err == nil {
		t.Error("the relay started without an upstream")
	}
	if err := initConfig(missing, flagsFrom(t)); err == nil {
		t.Error("init wrote a config without an upstream")
	}
}
