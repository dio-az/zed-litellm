package main

import (
	"encoding/json"
	"io"
	"log"
	"maps"
	"net/http"
	"slices"
)

// The /v1/models endpoint of LiteLLM has no capabilities. Without them, Zed offers no thinking, no
// effort and no image input. /model/info has a flag for each capability.

type modelInfoResponse struct {
	Data []modelInfoEntry `json:"data"`
}

type modelInfoEntry struct {
	ModelName string           `json:"model_name"`
	ModelInfo litellmModelInfo `json:"model_info"`
}

type litellmModelInfo struct {
	Mode              string   `json:"mode"`
	MaxInputTokens    *int64   `json:"max_input_tokens"`
	MaxOutputTokens   *int64   `json:"max_output_tokens"`
	MaxTokens         *int64   `json:"max_tokens"`
	SupportsVision    bool     `json:"supports_vision"`
	SupportsReasoning bool     `json:"supports_reasoning"`
	SupportsLow       *bool    `json:"supports_low_reasoning_effort"`
	SupportsXHigh     bool     `json:"supports_xhigh_reasoning_effort"`
	SupportsMax       bool     `json:"supports_max_reasoning_effort"`
	EffortLevels      []string `json:"reasoning_effort_levels"`
}

// Only the fields that Zed reads. See from_listed in crates/anthropic of Zed.
type modelList struct {
	Data    []anthropicModel `json:"data"`
	HasMore bool             `json:"has_more"`
}

type anthropicModel struct {
	Type           string       `json:"type"`
	ID             string       `json:"id"`
	DisplayName    string       `json:"display_name"`
	MaxInputTokens int64        `json:"max_input_tokens"`
	MaxTokens      int64        `json:"max_tokens"`
	Capabilities   capabilities `json:"capabilities"`
}

type capabilities struct {
	ImageInput supported `json:"image_input"`
	Thinking   thinking  `json:"thinking"`
	Effort     effort    `json:"effort"`
}

type supported struct {
	Supported bool `json:"supported"`
}

type thinking struct {
	Supported bool `json:"supported"`
	Types     struct {
		Adaptive supported `json:"adaptive"`
	} `json:"types"`
}

type effort struct {
	Supported bool      `json:"supported"`
	Low       supported `json:"low"`
	Medium    supported `json:"medium"`
	High      supported `json:"high"`
	XHigh     supported `json:"xhigh"`
	Max       supported `json:"max"`
}

func (h *handler) serveModels(w http.ResponseWriter, r *http.Request, rec *recording) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		h.upstream.JoinPath("model/info").String(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header = r.Header.Clone() // Zed's API key
	// Go decodes gzip only if Go asked for it, so the Accept-Encoding of Zed must go.
	req.Header.Del("Accept-Encoding")
	rec.request(modifiedRequest, req, nil)

	resp, err := h.client.Do(req)
	if err != nil {
		writeGatewayError(w, r, err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeGatewayError(w, r, err)
		return
	}
	rec.response(originalResponse, resp.StatusCode, resp.Header, body)
	// Zed gets the LiteLLM error unchanged, and the recording already has it.
	setResponseEvent(w, "")

	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}

	var info modelInfoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		log.Printf("models: %v", err)
		writeError(w, http.StatusBadGateway, "relay cannot read LiteLLM /model/info: "+err.Error())
		return
	}
	setResponseEvent(w, modifiedResponse)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelList{Data: buildModelList(info.Data, h.cfg.Overrides)})
}

// A Go map has no order, so the sort keeps the model picker the same between requests.
func buildModelList(entries []modelInfoEntry, overrides map[string]modelConfig) []anthropicModel {
	// /model/info lists each deployment, so a load-balanced model comes more than once.
	groups := map[string][]litellmModelInfo{}
	for _, e := range entries {
		if e.ModelInfo.Mode == "chat" {
			groups[e.ModelName] = append(groups[e.ModelName], e.ModelInfo)
		}
	}
	for id := range overrides {
		if _, ok := groups[id]; !ok {
			log.Printf("overrides: %q is not a chat model in LiteLLM /model/info", id)
		}
	}

	out := []anthropicModel{}
	for _, id := range slices.Sorted(maps.Keys(groups)) {
		if m, ok := toAnthropicModel(id, mergeDeployments(groups[id]), overrides[id]); ok {
			out = append(out, m)
		} else {
			log.Printf("models: %q has no token limits; set max_input_tokens and max_tokens", id)
		}
	}
	return out
}

// LiteLLM can route a request to any deployment of a model, so the model gets only what every
// deployment supports. A deployment without a token limit does not lower the limit.
func mergeDeployments(infos []litellmModelInfo) litellmModelInfo {
	merged := litellmModelInfo{Mode: "chat", SupportsVision: true, SupportsReasoning: true}
	for i, info := range infos {
		merged.MaxInputTokens = smaller(merged.MaxInputTokens, info.MaxInputTokens)
		merged.MaxOutputTokens = smaller(merged.MaxOutputTokens, first(info.MaxOutputTokens, info.MaxTokens))
		merged.SupportsVision = merged.SupportsVision && info.SupportsVision
		merged.SupportsReasoning = merged.SupportsReasoning && info.SupportsReasoning
		levels := litellmEffortLevels(info)
		if i == 0 {
			merged.EffortLevels = slices.Clone(levels)
		} else {
			merged.EffortLevels = slices.DeleteFunc(merged.EffortLevels, func(level string) bool {
				return !slices.Contains(levels, level)
			})
		}
	}
	return merged
}

func smaller(a, b *int64) *int64 {
	if a == nil || (b != nil && *b < *a) {
		return b
	}
	return a
}

// Zed hides a model without token limits, so the second result is false for such a model.
func toAnthropicModel(id string, info litellmModelInfo, mc modelConfig) (anthropicModel, bool) {
	maxIn := first(mc.MaxInputTokens, info.MaxInputTokens)
	maxOut := first(mc.MaxTokens, info.MaxOutputTokens, info.MaxTokens)
	if maxIn == nil || maxOut == nil {
		return anthropicModel{}, false
	}

	reasoning := *first(mc.Thinking, &info.SupportsReasoning)
	levels := mc.Effort
	if levels == nil {
		levels = litellmEffortLevels(info)
	}
	// LiteLLM also knows none and minimal. Zed has no setting for them.
	levels = slices.DeleteFunc(slices.Clone(levels), func(level string) bool {
		return !reasoning || !slices.Contains(zedEffortLevels, level)
	})

	m := anthropicModel{
		Type:           "model",
		ID:             id,
		DisplayName:    id,
		MaxInputTokens: *maxIn,
		MaxTokens:      *maxOut,
	}
	if mc.Name != "" {
		m.DisplayName = mc.Name
	}
	m.Capabilities.ImageInput.Supported = *first(mc.Images, &info.SupportsVision)
	m.Capabilities.Thinking.Supported = reasoning
	// Every reasoning model gets adaptive thinking. LiteLLM turns adaptive thinking and
	// output_config.effort into the reasoning setting of each provider.
	m.Capabilities.Thinking.Types.Adaptive.Supported = reasoning
	m.Capabilities.Effort = effort{
		Supported: len(levels) > 0,
		Low:       supported{slices.Contains(levels, "low")},
		Medium:    supported{slices.Contains(levels, "medium")},
		High:      supported{slices.Contains(levels, "high")},
		XHigh:     supported{slices.Contains(levels, "xhigh")},
		Max:       supported{slices.Contains(levels, "max")},
	}
	return m, true
}

// The same rules as reasoning_effort_capability in LiteLLM, so that Zed offers only the levels that
// LiteLLM accepts.
func litellmEffortLevels(info litellmModelInfo) []string {
	if info.EffortLevels != nil {
		return info.EffortLevels
	}
	levels := []string{"medium", "high"}
	if info.SupportsLow == nil || *info.SupportsLow {
		levels = append(levels, "low")
	}
	if info.SupportsXHigh {
		levels = append(levels, "xhigh")
	}
	if info.SupportsMax {
		levels = append(levels, "max")
	}
	return levels
}

func first[T any](values ...*T) *T {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}
