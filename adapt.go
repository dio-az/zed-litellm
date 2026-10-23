package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	cacheAuto   = "auto"
	cache1h     = "1h"
	cacheBypass = "bypass"
)

var (
	cacheModes        = []string{cacheAuto, cache1h, cacheBypass}
	cacheControlKey   = []byte(`"cache_control"`)
	uncacheableBlocks = map[string]bool{"thinking": true, "redacted_thinking": true}

	// A random UUID, made once. A new value gives every running thread a new LiteLLM session.
	sessionNamespace = [16]byte{0x61, 0x5c, 0x6e, 0x71, 0xa4, 0xd3, 0x4f, 0x94, 0xa0, 0x44, 0x97, 0xef, 0x75, 0x6d, 0xca, 0xed}
)

type adapter struct {
	cache     string
	overrides map[string]modelConfig
}

// LiteLLM drops a top-level cache marker on the way to Bedrock, so the marker of a Claude request
// moves into the messages.
func (a adapter) adapt(body []byte) ([]byte, string) {
	req, err := parseObject(body)
	if err != nil {
		return body, ""
	}
	rawMessages, ok := req.get("messages")
	if !ok {
		return body, ""
	}
	messages, err := parseArray(rawMessages)
	if err != nil || len(messages) == 0 {
		return body, ""
	}

	session := sessionID(messages[0])
	model := stringValue(req.get("model"))

	mode := a.cache
	if m := a.overrides[model].Cache; m != "" {
		mode = m
	}

	if strings.HasPrefix(model, "claude") && moveCacheMarkerToMessages(&req, messages, mode) {
		return req.bytes(), session
	}
	return body, session
}

// Zed does not send its thread ID to Anthropic, and the first prompt is the only part of a thread
// that stays the same. Compaction keeps the newest prompts that fit in 80 KB, so a thread gets a
// new session when its later prompts fill the 80 KB. Two threads that start with the same prompt
// share a session. Without a prompt there is no session, so that unrelated threads stay apart.
func sessionID(firstMessage json.RawMessage) string {
	prompt := firstPrompt(firstMessage)
	if prompt == "" {
		return ""
	}
	h := sha1.New()
	h.Write(sessionNamespace[:])
	h.Write([]byte(prompt))
	var u [16]byte
	copy(u[:], h.Sum(nil))
	u[6] = u[6]&0x0f | 0x50 // version 5
	u[8] = u[8]&0x3f | 0x80 // RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:])
}

// Compaction adds later prompts to the first message, and cuts the oldest prompt from its end to
// fit 80 KB. No block marks where a prompt ends, so only its start identifies it: the first text
// block, with the mention links before it. Zed sends a mention as a link block, and can send the
// space between two mentions as a block of its own.
func firstPrompt(message json.RawMessage) string {
	msg, err := parseObject(message)
	if err != nil {
		return ""
	}
	content, ok := msg.get("content")
	if !ok {
		return ""
	}
	if isString(content) {
		return stringValue(content, true)
	}
	blocks, err := parseArray(content)
	if err != nil {
		return ""
	}
	var prompt []string
	for _, raw := range blocks {
		block, err := parseObject(raw)
		if err != nil {
			continue
		}
		text, ok := block.get("text")
		if !ok || !isString(text) {
			continue
		}
		s := stringValue(text, true)
		if strings.TrimSpace(s) == "" {
			continue
		}
		prompt = append(prompt, s)
		if !isMentionLink(s) {
			break
		}
	}
	return strings.Join(prompt, "\n")
}

func isMentionLink(text string) bool {
	return strings.HasPrefix(text, "[@") && strings.HasSuffix(text, ")")
}

// The marker on the last message writes the conversation to the cache. The previous request wrote
// at the previous user message, so a marker there makes this request read the cache. With the
// markers that Zed puts on tools and system, the request has 4 markers, the maximum of Anthropic.
func moveCacheMarkerToMessages(req *object, messages []json.RawMessage, mode string) bool {
	marker, ok := req.get("cache_control")
	if !ok {
		return false
	}

	previous := previousUserIndex(messages)
	targets := []int{len(messages) - 1}
	if previous >= 0 {
		targets = append(targets, previous)
	}
	ttl := chooseTTL(marker, messages, previous, mode)
	if ttl == cache1h {
		// Anthropic rejects a marker with a longer TTL after one with a shorter TTL, and tools and
		// system come before the messages. Zed gives them 1h already, so this rarely changes them.
		lengthenMarkers(req, "tools")
		lengthenMarkers(req, "system")
	}
	marker = withTTL(marker, ttl)

	marked := false
	for _, i := range targets {
		if updated, ok := markLastCacheableBlock(messages[i], marker); ok {
			messages[i] = updated
			marked = true
		}
	}
	if !marked {
		return false
	}

	req.del("cache_control")
	req.set("messages", arrayBytes(messages))
	return true
}

// "" means the provider default of 5 minutes.
func chooseTTL(marker json.RawMessage, messages []json.RawMessage, previous int, mode string) string {
	var zedTTL string
	if m, err := parseObject(marker); err == nil {
		zedTTL = stringValue(m.get("ttl"))
	}
	switch {
	case mode == cache1h:
		return cache1h
	case mode == cacheBypass, zedTTL != "", previous < 0:
		return zedTTL
	}

	// A 1h write costs 2 times the input price, and a 5m write 1.25 times. Each request writes only
	// the new content. After a pause of 5 to 60 minutes, a 1h cache still holds the conversation.
	// The next request then reads it for 0.1 times the price, and does not write it again for 1.25
	// times. With one such pause in 20 requests (a recorded Zed session had one in 31), 1h costs
	// less when the earlier conversation is at least 12 times the new content.
	var history, added int
	for i, m := range messages {
		if i <= previous {
			history += len(m)
		} else {
			added += len(m)
		}
	}
	if history >= 12*added {
		return cache1h
	}
	return ""
}

// An unchanged marker keeps its bytes, because the cache needs the request to stay the same, byte
// for byte.
func withTTL(marker json.RawMessage, ttl string) json.RawMessage {
	m, err := parseObject(marker)
	if err != nil || ttl == "" || stringValue(m.get("ttl")) == ttl {
		return marker
	}
	m.set("ttl", json.RawMessage(jsonString(ttl)))
	return m.bytes()
}

func previousUserIndex(messages []json.RawMessage) int {
	for i := len(messages) - 2; i >= 0; i-- {
		msg, err := parseObject(messages[i])
		if err == nil && stringValue(msg.get("role")) == "user" {
			return i
		}
	}
	return -1
}

func markLastCacheableBlock(message, marker json.RawMessage) (json.RawMessage, bool) {
	msg, err := parseObject(message)
	if err != nil {
		return nil, false
	}
	content, ok := msg.get("content")
	if !ok {
		return nil, false
	}

	if isString(content) {
		block := object{
			{key: "type", val: json.RawMessage(`"text"`)},
			{key: "text", val: content},
			{key: "cache_control", val: marker},
		}
		msg.set("content", arrayBytes([]json.RawMessage{block.bytes()}))
		return msg.bytes(), true
	}

	blocks, err := parseArray(content)
	if err != nil {
		return nil, false
	}
	for j := len(blocks) - 1; j >= 0; j-- {
		block, err := parseObject(blocks[j])
		if err != nil || uncacheableBlocks[stringValue(block.get("type"))] {
			continue
		}
		block.set("cache_control", marker)
		blocks[j] = block.bytes()
		msg.set("content", arrayBytes(blocks))
		return msg.bytes(), true
	}
	return nil, false
}

// A string, for example a plain system prompt, has no blocks to mark.
func lengthenMarkers(obj *object, key string) {
	raw, ok := obj.get(key)
	if !ok || !bytes.Contains(raw, cacheControlKey) {
		return
	}
	blocks, err := parseArray(raw)
	if err != nil {
		return
	}
	changed := false
	for i, b := range blocks {
		block, err := parseObject(b)
		if err != nil {
			continue
		}
		marker, ok := block.get("cache_control")
		if !ok {
			continue
		}
		if long := withTTL(marker, cache1h); !bytes.Equal(long, marker) {
			block.set("cache_control", long)
			blocks[i] = block.bytes()
			changed = true
		}
	}
	if changed {
		obj.set(key, arrayBytes(blocks))
	}
}
