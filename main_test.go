package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionHeaderGoesUpstreamUnlessTheClientSentOne(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer upstream.Close()

	cfg := defaultConfig()
	cfg.Upstream = upstream.URL
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	send := func(header, value string) {
		body := `{"model":"gpt-6-luna","messages":[{"role":"user","content":"Testing again LiteLLM caching"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		if header != "" {
			req.Header.Set(header, value)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	send("", "")
	if v := got.Get(zedSessionHeader); v != "98d6507d-a19d-57ff-9b1f-b1bad47ce2f2" {
		t.Errorf("%s = %q, want the thread's UUID", zedSessionHeader, v)
	}
	send(zedSessionHeader, "from-zed-1234")
	if v := got.Get(zedSessionHeader); v != "from-zed-1234" {
		t.Errorf("%s = %q; the relay replaced Zed's own ID", zedSessionHeader, v)
	}
	send(litellmSessionHeader, "from-client")
	if v := got.Get(zedSessionHeader); v != "" {
		t.Errorf("%s = %q; it would compete with the client's %s", zedSessionHeader, v, litellmSessionHeader)
	}
}
