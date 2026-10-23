package main

import (
	"bufio"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedEvent struct {
	Event  string          `json:"event"`
	URL    string          `json:"url"`
	Status int             `json:"status"`
	Header http.Header     `json:"headers"`
	Body   json.RawMessage `json:"body"`
}

// The requests run at the same time. The key is the file name without .jsonl, and each exchange
// starts at its original_request.
func recordExchanges(t *testing.T, upstream http.HandlerFunc, requests ...*http.Request) map[string][][]recordedEvent {
	t.Helper()
	server := httptest.NewServer(upstream)
	defer server.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".local", "state", "zed-litellm", "records")
	cfg, err := loadConfig(filepath.Join(home, "missing.json"), flagsFrom(t, "-upstream", server.URL, "-debug"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, req := range requests {
		wg.Go(func() { h.ServeHTTP(httptest.NewRecorder(), req) })
	}
	wg.Wait()

	paths, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	files := map[string][][]recordedEvent{}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var exchanges [][]recordedEvent
		scanner := bufio.NewScanner(f)
		scanner.Buffer(nil, 64<<20)
		for scanner.Scan() {
			var ev recordedEvent
			if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
				t.Fatalf("line is not JSON: %v", err)
			}
			if ev.Event == originalRequest || len(exchanges) == 0 {
				exchanges = append(exchanges, nil)
			}
			exchanges[len(exchanges)-1] = append(exchanges[len(exchanges)-1], ev)
		}
		if err := scanner.Err(); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f.Close()
		files[strings.TrimSuffix(filepath.Base(path), ".jsonl")] = exchanges
	}
	return files
}

func events(exchange []recordedEvent) string {
	var names []string
	for _, ev := range exchange {
		names = append(names, ev.Event)
	}
	return strings.Join(names, ",")
}

func TestRecordingLogsModifiedStepsOnlyWhenTheyChange(t *testing.T) {
	const stream = "event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n"
	upstream := func(w http.ResponseWriter, r *http.Request) {
		// For /model/info, the Go client asks for gzip itself and decodes it.
		if r.URL.Path == "/model/info" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		if r.Header.Get("Accept-Encoding") != "" {
			t.Errorf("upstream got Accept-Encoding %q; the recording would be compressed", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	}
	post := func(body string, header ...string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("X-Api-Key", "sk-secret")
		req.Header.Set("Accept-Encoding", "gzip")
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		return req
	}
	untouched := `{"model":"gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`
	models := httptest.NewRequest(http.MethodGet, "/v1/models", nil)

	const claudeSession = "534ee319-045c-5872-a417-0baef385852d" // UUIDv5 of "first"
	claude := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"first"}],"cache_control":{"type":"ephemeral"}}`
	files := recordExchanges(t, upstream,
		post(claude),
		post(untouched, litellmSessionHeader, "from-client-123"),
		post(untouched, zedSessionHeader, "../escape"),
		models)

	want := map[string][]string{
		claudeSession:     {"original_request,modified_request,original_response"},
		"from-client-123": {"original_request,original_response"},
		noSession: { // In sorted order.
			"original_request,modified_request,original_response,modified_response",
			"original_request,original_response",
		},
	}
	if len(files) != len(want) {
		t.Errorf("got files %v, want %d", slices.Collect(maps.Keys(files)), len(want))
	}
	for file, wantExchanges := range want {
		var got []string
		for _, exchange := range files[file] {
			got = append(got, events(exchange))
		}
		// Exchanges that ran at the same time can be in either order.
		slices.Sort(got)
		if !slices.Equal(got, wantExchanges) {
			t.Errorf("%s.jsonl exchanges = %v, want %v", file, got, wantExchanges)
		}
	}

	claudeExchange := files[claudeSession][0]
	original, modified, response := claudeExchange[0], claudeExchange[1], claudeExchange[2]
	if string(original.Body) != claude {
		t.Errorf("original_request body = %s, want Zed's body", original.Body)
	}
	if original.Header.Get("X-Api-Key") != "[redacted]" || original.Header.Get("Accept-Encoding") != "gzip" {
		t.Errorf("original_request headers = %v, want the key redacted and Zed's Accept-Encoding", original.Header)
	}
	wantForwarded := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]}]}`
	if string(modified.Body) != wantForwarded || modified.Header.Get(zedSessionHeader) == "" {
		t.Errorf("modified_request = %s %v, want the moved marker and the session header", modified.Body, modified.Header)
	}
	var body string
	if err := json.Unmarshal(response.Body, &body); err != nil || body != stream || response.Status != http.StatusOK {
		t.Errorf("original_response = %d %s, want 200 and the stream as a string", response.Status, response.Body)
	}

	for _, exchange := range files[noSession] {
		if len(exchange) != 4 {
			continue
		}
		if modelInfo, list := exchange[1], exchange[3]; !strings.HasSuffix(modelInfo.URL, "/model/info") ||
			string(list.Body) != `{"data":[],"has_more":false}` {
			t.Errorf("models: modified_request %s, modified_response %s", modelInfo.URL, list.Body)
		}
	}
}

func TestConcurrentExchangesOfASessionDoNotMix(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for range 3 {
			_, _ = w.Write([]byte("data: {}\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(time.Millisecond)
		}
	}
	var requests []*http.Request
	for range 20 {
		body := `{"model":"gpt-6-luna","messages":[{"role":"user","content":"same thread"}]}`
		requests = append(requests, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	}
	files := recordExchanges(t, upstream, requests...)
	if len(files) != 1 {
		t.Fatalf("got files %v, want one session", slices.Collect(maps.Keys(files)))
	}
	for _, exchanges := range files {
		if len(exchanges) != 20 {
			t.Errorf("got %d exchanges, want 20", len(exchanges))
		}
		for _, exchange := range exchanges {
			if events(exchange) != "original_request,modified_request,original_response" {
				t.Errorf("exchange lines mixed: %s", events(exchange))
			}
		}
	}
}

func TestNoRecordingWithoutDebug(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := loadConfig(filepath.Join(home, "missing.json"), flagsFrom(t, "-upstream", upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
		t.Errorf("the relay wrote to %s/.local without -debug", home)
	}
}
