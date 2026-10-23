// Command zed-litellm is a local relay between Zed and a LiteLLM proxy. It fixes the prompt cache
// for Claude on Bedrock, serves a model list with capabilities, and groups each Zed thread into one
// LiteLLM session.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	// LiteLLM groups requests by any x-<vendor>-session-id header.
	zedSessionHeader     = "X-Zed-Session-Id"
	litellmSessionHeader = "X-LiteLLM-Session-Id"
	maxRequestBody       = 64 << 20
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	path := configPath()
	applyFlags := bindConfigFlags(flag.CommandLine)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags] [init]\n\n"+
			"  init  write a config file with the given flags, then exit\n\n"+
			"Flags replace the matching values from %s.\n\n",
			filepath.Base(os.Args[0]), shortPath(path))
		flag.PrintDefaults()
	}
	flag.Parse()

	switch flag.Arg(0) {
	case "":
	case "init":
		if err := initConfig(path, applyFlags); err != nil {
			log.Fatalf("init: %v", err)
		}
		fmt.Println("wrote", path)
		return
	default:
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := loadConfig(path, applyFlags)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	h, err := newHandler(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if !cfg.debug {
		log.SetOutput(io.Discard)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout, because a response streams for as long as the model runs.
	}
	log.Printf("listening on http://%s -> %s", cfg.Listen, h.upstream)
	err = srv.ListenAndServe()
	// Without -debug the log goes nowhere, but a stop must still print its cause, for example a
	// port in use.
	log.SetOutput(os.Stderr)
	log.Fatal(err)
}

type handler struct {
	cfg      config
	upstream *url.URL
	adapter  adapter
	proxy    *httputil.ReverseProxy
	client   *http.Client
	recorder *recorder
}

func newHandler(cfg config) (*handler, error) {
	upstream, err := parseUpstream(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	var dir string
	if cfg.debug {
		dir = recordDir()
	}
	recorder, err := newRecorder(dir)
	if err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 16
	// Compressed responses go through unchanged, so that the relay never holds a stream back to
	// decompress it.
	transport.DisableCompression = true

	return &handler{
		cfg:      cfg,
		upstream: upstream,
		adapter:  cfg.newAdapter(),
		client: &http.Client{
			Timeout: 30 * time.Second,
			// Go keeps X-Api-Key on a redirect to another host, so a redirect could send Zed's key
			// there. Zed gets the redirect instead, as on the proxied requests.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		recorder: recorder,
		proxy: &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(upstream)
			},
			Transport:     transport,
			FlushInterval: -1, // So that Zed shows thinking and text as they arrive.
			ErrorHandler:  writeGatewayError,
		},
	}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := h.recorder.start()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK, rec: rec, event: originalResponse}
	defer func() {
		sw.recordResponse()
		// The session header is set by now, because adaptRequest runs before this deferred call.
		session := r.Header.Get(zedSessionHeader)
		if session == "" {
			session = r.Header.Get(litellmSessionHeader)
		}
		rec.finish(session)
	}()

	messages := r.Method == http.MethodPost && r.URL.Path == "/v1/messages"
	if rec != nil || messages {
		body, err := io.ReadAll(http.MaxBytesReader(sw, r.Body, maxRequestBody))
		if err != nil {
			status := http.StatusBadRequest
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				status = http.StatusRequestEntityTooLarge
			}
			writeError(sw, status, "relay cannot read the request body: "+err.Error())
			return
		}
		rec.request(originalRequest, r, body)
		if messages {
			body = h.adaptRequest(r, body, rec)
		}
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	}
	if rec != nil {
		// Without Accept-Encoding, LiteLLM answers uncompressed, so that the recording is readable.
		r.Header.Del("Accept-Encoding")
	}

	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		h.serveModels(sw, r, rec)
	} else {
		h.proxy.ServeHTTP(sw, r)
	}

	if h.cfg.debug {
		log.Printf("%s %s %d %s %s", r.Method, r.URL.Path, sw.status,
			time.Since(start).Round(time.Millisecond), r.Header.Get(zedSessionHeader))
	}
}

func (h *handler) adaptRequest(r *http.Request, body []byte, rec *recording) []byte {
	out, session := h.adapter.adapt(body)
	modified := !bytes.Equal(out, body)
	if session != "" && r.Header.Get(zedSessionHeader) == "" && r.Header.Get(litellmSessionHeader) == "" {
		r.Header.Set(zedSessionHeader, session)
		modified = true
	}
	if modified {
		rec.request(modifiedRequest, r, out)
	}
	return out
}

func writeGatewayError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("upstream %s %s: %v", r.Method, r.URL.Path, err)
	writeError(w, http.StatusBadGateway, "relay cannot reach the gateway: "+err.Error())
}

// The Anthropic error format makes Zed show the message.
func writeError(w http.ResponseWriter, status int, message string) {
	setResponseEvent(w, modifiedResponse)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":`+jsonString(message)+`}}`)
}

// The Unwrap method gives the reverse proxy the Flusher of the original writer. Without it,
// streaming stops.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	header      http.Header
	rec         *recording
	event       string // "" does not record the response.
	body        bytes.Buffer
}

// A caller passes modifiedResponse when it changed or generated the response, and "" when the
// recording already has the response.
func setResponseEvent(w http.ResponseWriter, event string) {
	if sw, ok := w.(*statusWriter); ok {
		sw.event = event
	}
}

func (s *statusWriter) WriteHeader(code int) {
	// A 1xx response is informational. The final status comes after it.
	if !s.wroteHeader && code >= 200 {
		s.status = code
		s.wroteHeader = true
		if s.rec != nil {
			s.header = s.Header().Clone()
		}
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	if s.rec != nil {
		s.body.Write(b)
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) recordResponse() {
	if s.event != "" {
		s.rec.response(s.event, s.status, s.header, s.body.Bytes())
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
