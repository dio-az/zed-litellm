package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

var testAdapter = defaultConfig().newAdapter()

func TestSessionIDIsAStandardUUIDv5(t *testing.T) {
	// Python made these values: uuid.uuid5(UUID("615c6e71-a4d3-4f94-a044-97ef756dcaed"), prompt).
	cases := map[string]string{
		"Testing again LiteLLM caching": "98d6507d-a19d-57ff-9b1f-b1bad47ce2f2",
		"first":                         "534ee319-045c-5872-a417-0baef385852d",
	}
	for prompt, want := range cases {
		msg := `{"role":"user","content":[{"type":"text","text":` + jsonString(prompt) + `}]}`
		if got := sessionID(json.RawMessage(msg)); got != want {
			t.Errorf("sessionID(%q) = %s, want %s", prompt, got, want)
		}
	}
}

func TestNoSessionWithoutAPrompt(t *testing.T) {
	msg := `{"role":"user","content":[{"type":"image","source":{}}]}`
	if got := sessionID(json.RawMessage(msg)); got != "" {
		t.Errorf("sessionID = %q; threads without a prompt would share one session", got)
	}
}

// The request looks like one from Zed: prefixTTL on the markers of tools and system, and marker at
// the top level.
func conversation(turns, turnSize, tailSize int, marker, prefixTTL string) string {
	var msgs []string
	for i := range turns {
		role := []string{"user", "assistant"}[i%2]
		msgs = append(msgs, `{"role":"`+role+`","content":[{"type":"text","text":"`+strings.Repeat("x", turnSize)+`"}]}`)
	}
	msgs = append(msgs, `{"role":"user","content":[{"type":"text","text":"`+strings.Repeat("y", tailSize)+`"}]}`)
	prefix := `{"type":"ephemeral"` + prefixTTL + `}`
	return `{"model":"claude-opus-5-5","tools":[{"name":"t","cache_control":` + prefix + `}],` +
		`"system":[{"type":"text","text":"s","cache_control":` + prefix + `}],` +
		`"messages":[` + strings.Join(msgs, ",") + `],"cache_control":` + marker + `}`
}

func TestCacheTTL(t *testing.T) {
	const (
		zedMarker = `{"type":"ephemeral"}`
		zedPrefix = `,"ttl":"1h"`
		long      = `{"type":"ephemeral","ttl":"1h"}`
		short     = `{"type":"ephemeral"}`
	)
	cases := []struct {
		name, mode, body, want string
	}{
		{"auto: long history, small turn", cacheAuto, conversation(20, 1000, 500, zedMarker, zedPrefix), long},
		{"auto: new content too large", cacheAuto, conversation(20, 1000, 5000, zedMarker, zedPrefix), short},
		{"auto: first message", cacheAuto, conversation(0, 0, 10, zedMarker, zedPrefix), short},
		{"auto: Zed's TTL wins", cacheAuto, conversation(20, 1000, 500, `{"type":"ephemeral","ttl":"5m"}`, zedPrefix), `{"type":"ephemeral","ttl":"5m"}`},
		{"auto: lengthens a short prefix", cacheAuto, conversation(20, 1000, 500, zedMarker, ""), long},
		{"bypass: keeps Zed's default for a long history", cacheBypass, conversation(20, 1000, 500, zedMarker, zedPrefix), short},
		{"bypass: keeps Zed's TTL", cacheBypass, conversation(20, 1000, 500, long, zedPrefix), long},
		{"1h: replaces Zed's TTL", cache1h, conversation(20, 1000, 500, `{"type":"ephemeral","ttl":"5m"}`, zedPrefix), long},
		{"1h: even for a short history", cache1h, conversation(1, 10, 500, zedMarker, zedPrefix), long},
		{"1h: with short prefix markers", cache1h, conversation(20, 1000, 500, zedMarker, ""), long},
		{"auto: string system mentioning markers", cacheAuto,
			strings.Replace(conversation(20, 1000, 500, zedMarker, zedPrefix),
				`"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}]`,
				`"system":"talks about \"cache_control\""`, 1), long},
	}
	for _, tc := range cases {
		a := adapter{cache: tc.mode}
		got, _ := a.adapt([]byte(tc.body))
		var req struct {
			Messages []struct {
				Content []struct {
					CacheControl json.RawMessage `json:"cache_control"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(got, &req); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var markers []string
		for _, m := range req.Messages {
			for _, c := range m.Content {
				if c.CacheControl != nil {
					markers = append(markers, string(c.CacheControl))
				}
			}
		}
		for _, m := range markers {
			if m != tc.want {
				t.Errorf("%s: markers %v, want each %s", tc.name, markers, tc.want)
				break
			}
		}
		if len(markers) == 0 {
			t.Errorf("%s: no markers in %s", tc.name, got)
		}
		if tc.want == long && !ttlsNeverGrow(t, got) {
			t.Errorf("%s: a longer TTL follows a shorter one: %s", tc.name, got)
		}
	}
}

// Anthropic rejects a marker with a longer TTL than an earlier marker, in the order tools, system,
// messages.
func ttlsNeverGrow(t *testing.T, body []byte) bool {
	var req struct {
		Tools    []map[string]json.RawMessage `json:"tools"`
		System   json.RawMessage              `json:"system"`
		Messages []struct {
			Content []map[string]json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	blocks := req.Tools
	var system []map[string]json.RawMessage
	if json.Unmarshal(req.System, &system) == nil {
		blocks = append(blocks, system...)
	}
	for _, m := range req.Messages {
		blocks = append(blocks, m.Content...)
	}
	longest := 2 // 1h
	for _, b := range blocks {
		cc, ok := b["cache_control"]
		if !ok {
			continue
		}
		ttl := 1
		if strings.Contains(string(cc), `"1h"`) {
			ttl = 2
		}
		if ttl > longest {
			return false
		}
		longest = ttl
	}
	return true
}

func userMessage(blocks ...string) json.RawMessage {
	content := make([]string, len(blocks))
	for i, b := range blocks {
		content[i] = `{"type":"text","text":` + jsonString(b) + `}`
	}
	return json.RawMessage(`{"role":"user","content":[` + strings.Join(content, ",") + `]}`)
}

// The blocks follow UserMessage::to_request in Zed's crates/agent/src/thread.rs.
func mentionPrompt(name, text string) []string {
	return []string{
		"[@" + name + "](zed:///agent/skill?name=" + name + ")", text,
		"<context>\nThe following items were attached by the user.", "<skills>\n" + name + " content", "</context>",
	}
}

func TestSessionIDSurvivesCompaction(t *testing.T) {
	first := mentionPrompt("skill", "go")
	cases := map[string]struct{ before, after []string }{
		"plain prompts":               {[]string{"first"}, []string{"first", "second", "summary"}},
		"first prompt with a mention": {first, append(slices.Clone(first), "second", "summary")},
		"later prompt with a mention": {
			[]string{"first"},
			append(append([]string{"first"}, mentionPrompt("other", "second")...), "summary"),
		},
		// Zed cut the attachments of the oldest prompt to fit 80 KB, as in a recorded thread.
		"attachments cut": {
			first,
			append(append(slices.Clone(first[:4:4]), mentionPrompt("other", "second")...), "summary"),
		},
		// An image after the text did not fit, so Zed cut everything from the image on.
		"context cut": {
			first,
			append(append(slices.Clone(first[:2:2]), mentionPrompt("other", "second")...), "summary"),
		},
	}
	for name, c := range cases {
		if sessionID(userMessage(c.before...)) != sessionID(userMessage(c.after...)) {
			t.Errorf("%s: session ID changed after compaction", name)
		}
	}
}

func TestThreadsThatStartWithTheSameMentionGetDifferentSessions(t *testing.T) {
	if sessionID(userMessage(mentionPrompt("skill", "fix the bug")...)) == sessionID(userMessage(mentionPrompt("skill", "write docs")...)) {
		t.Error("two threads with different prompts after the same mention share a session")
	}
	if sessionID(userMessage(mentionPrompt("skill", "go")...)) == sessionID(userMessage(mentionPrompt("other", "go")...)) {
		t.Error("two threads with different mentions share a session")
	}
	twoMentions := func(text string) json.RawMessage {
		return userMessage("[@a](zed:///a)", " ", "[@b](zed:///b)", text)
	}
	if sessionID(twoMentions("fix the bug")) == sessionID(twoMentions("write docs")) {
		t.Error("two threads with different prompts after the same two mentions share a session")
	}
}

func TestClaudeMovesMarkerToLastAndPreviousUser(t *testing.T) {
	in := `{"model":"claude-sonnet-5-5","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"a"}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"text","text":"b"}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"r"},{"type":"text","text":"c"}]}` +
		`],"cache_control":{"type":"ephemeral"}}`
	want := `{"model":"claude-sonnet-5-5","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"text","text":"b"}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"r"},{"type":"text","text":"c","cache_control":{"type":"ephemeral"}}]}` +
		`]}`
	got, _ := testAdapter.adapt([]byte(in))
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestClaudeKeepsTTLAndConvertsStringContent(t *testing.T) {
	in := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}],"cache_control":{"type":"ephemeral","ttl":"1h"}}`
	want := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	got, _ := testAdapter.adapt([]byte(in))
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestClaudeWithoutTopLevelMarkerIsUnchanged(t *testing.T) {
	in := `{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}]}`
	got, session := testAdapter.adapt([]byte(in))
	if string(got) != in || session == "" {
		t.Errorf("got %s (session %q), want the input with a session", got, session)
	}
}

func TestOtherModelsKeepOriginalBytes(t *testing.T) {
	for _, mode := range []string{cacheAuto, cache1h, cacheBypass} {
		in := []byte(`{"model":"gemini-4-argon", "tools":[{"name":"t","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
			`"messages":[{"role":"user","content":"hi"}],"cache_control":{"type":"ephemeral"}}`)
		got, session := adapter{cache: mode}.adapt(in)
		if !bytes.Equal(got, in) || session == "" {
			t.Errorf("%s: got %s (session %q)", mode, got, session)
		}
	}
}

func TestCacheOffRemovesEveryMarker(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "claude-opus-5-5"} {
		in := `{"model":"` + model + `","tools":[{"name":"t","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
			`"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
			`"messages":[{"role":"user","content":"hi"}],"cache_control":{"type":"ephemeral"}}`
		want := `{"model":"` + model + `","tools":[{"name":"t"}],"system":[{"type":"text","text":"s"}],` +
			`"messages":[{"role":"user","content":"hi"}]}`
		got, session := adapter{cache: cacheAuto, overrides: map[string]modelConfig{model: {Cache: cacheOff}}}.adapt([]byte(in))
		if string(got) != want || session == "" {
			t.Errorf("%s: got  %s (session %q)\nwant %s", model, got, session, want)
		}
	}
}

func TestInvalidBodyPassesThroughWithoutSession(t *testing.T) {
	for _, in := range []string{"", "not json", "{}", `{"messages":[]}`, `[1,2]`} {
		got, session := testAdapter.adapt([]byte(in))
		if string(got) != in || session != "" {
			t.Errorf("adapt(%q) = %q, %q", in, got, session)
		}
	}
}

func TestUntouchedValuesKeepTheirBytes(t *testing.T) {
	// Outside the edited blocks, escapes and number formats must not change.
	in := `{"model":"claude-sonnet-5-5","max_tokens":1.0e5,"tools":[{"name":"é\/x"}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"a"}]}],"cache_control":{"type":"ephemeral"}}`
	got, _ := testAdapter.adapt([]byte(in))
	for _, raw := range []string{`1.0e5`, `"é\/x"`} {
		if !strings.Contains(string(got), raw) {
			t.Errorf("%s was rewritten: %s", raw, got)
		}
	}
}

func BenchmarkAdaptClaude(b *testing.B) {
	var msgs []string
	for i := range 40 {
		role := []string{"user", "assistant"}[i%2]
		msgs = append(msgs, `{"role":"`+role+`","content":[{"type":"text","text":"`+strings.Repeat("x", 7000)+`"}]}`)
	}
	body := []byte(`{"model":"claude-sonnet-5-5","messages":[` + strings.Join(msgs, ",") + `],"cache_control":{"type":"ephemeral"}}`)
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		testAdapter.adapt(body)
	}
}
