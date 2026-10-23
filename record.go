package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// A recording holds prompts and code, so only the user can read the files, and these headers become
// [redacted].
var redactedHeaders = []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "Cookie"}

const (
	originalRequest  = "original_request"
	modifiedRequest  = "modified_request"
	originalResponse = "original_response"
	modifiedResponse = "modified_response"
)

const noSession = "no-session"

// A session ID becomes a file name, so it can have only these characters. LiteLLM groups an
// x-<vendor>-session-id only if it has these characters too, so no grouped session loses its file.
var plainSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// All lines of an exchange go to the file together when the exchange ends. So two requests of one
// session that run at the same time do not mix their lines.
type recorder struct {
	mu  sync.Mutex
	dir string
}

func newRecorder(dir string) (*recorder, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	log.Printf("recording requests and responses to %s", dir)
	return &recorder{dir: dir}, nil
}

// A nil recording records nothing.
type recording struct {
	recorder *recorder
	lines    bytes.Buffer
}

type recordEvent struct {
	Time   string      `json:"time"`
	Event  string      `json:"event"`
	Method string      `json:"method,omitempty"`
	URL    string      `json:"url,omitempty"`
	Status int         `json:"status,omitempty"`
	Header http.Header `json:"headers,omitempty"`
	Body   any         `json:"body,omitempty"`
}

func (r *recorder) start() *recording {
	if r == nil {
		return nil
	}
	return &recording{recorder: r}
}

func (rec *recording) request(event string, r *http.Request, body []byte) {
	rec.write(recordEvent{Event: event, Method: r.Method, URL: r.URL.String(), Header: r.Header, Body: body})
}

func (rec *recording) response(event string, status int, header http.Header, body []byte) {
	rec.write(recordEvent{Event: event, Status: status, Header: header, Body: body})
}

func (rec *recording) write(ev recordEvent) {
	if rec == nil {
		return
	}
	ev.Time = time.Now().Format(time.RFC3339Nano)
	ev.Header = redact(ev.Header)
	if raw, ok := ev.Body.([]byte); ok {
		ev.Body = bodyValue(raw)
	}
	line, err := json.Marshal(ev)
	if err != nil {
		log.Printf("record: %v", err)
		return
	}
	rec.lines.Write(line)
	rec.lines.WriteByte('\n')
}

func (rec *recording) finish(session string) {
	if rec == nil || rec.lines.Len() == 0 {
		return
	}
	if !plainSessionID.MatchString(session) {
		session = noSession
	}
	r := rec.recorder
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(r.dir, session+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("record: %v", err)
		return
	}
	if _, err := f.Write(rec.lines.Bytes()); err != nil {
		log.Printf("record: %v", err)
	}
	if err := f.Close(); err != nil {
		log.Printf("record: %v", err)
	}
}

// A JSON body stays JSON, so that jq can query it. Any other body, for example an event stream,
// becomes a string.
func bodyValue(body []byte) any {
	switch {
	case len(body) == 0:
		return nil
	case json.Valid(body):
		return json.RawMessage(body)
	default:
		return string(body)
	}
}

func redact(header http.Header) http.Header {
	if header == nil {
		return nil
	}
	h := header.Clone()
	for _, key := range redactedHeaders {
		if h.Get(key) != "" {
			h.Set(key, "[redacted]")
		}
	}
	return h
}
