package main

// Audit trail and undo points for the agent API. Every agent call becomes
// one record (time, call, arguments, result, the window in front) sent to
// keeper (cmd/keeper), which runs in its own container: the agent can be
// root in this one, so nothing it could erase is kept here. Before a call
// that can change things, keeper is asked for a snapshot of the home folder
// if the last one is older than SNAPSHOT_INTERVAL (before shell commands:
// always), so a mistake can be undone with scripts/undo.sh.
//
// Without KEEPER_URL (desktopd outside compose) records only go to the log.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type auditor struct {
	url      string        // keeper, e.g. http://keeper:8090; "" = log only
	interval time.Duration // max age of the snapshot before a mutating call
	queue    chan map[string]any
	client   *http.Client
}

func newAuditor(url string, interval time.Duration) *auditor {
	a := &auditor{url: strings.TrimRight(url, "/"), interval: interval, queue: make(chan map[string]any, 1024),
		client: &http.Client{Timeout: 30 * time.Second}}
	if a.url != "" {
		go a.send()
	}
	return a
}

// send posts records to keeper in order; if keeper is down they are logged
// here instead, so they still reach `docker compose logs`
func (a *auditor) send() {
	for rec := range a.queue {
		b, _ := json.Marshal(rec)
		var err error
		for try := 0; try < 3; try++ {
			var res *http.Response
			if res, err = a.client.Post(a.url+"/audit", "application/json", bytes.NewReader(b)); err == nil {
				res.Body.Close()
				break
			}
			time.Sleep(time.Second)
		}
		if err != nil {
			log.Printf("audit: keeper unreachable (%v), record: %s", err, b)
		}
	}
}

// snapshot asks keeper for a snapshot no older than maxAge; it returns the
// snapshot's id, or "" without keeper
func (a *auditor) snapshot(ctx context.Context, reason string, maxAge time.Duration) (string, error) {
	if a.url == "" {
		return "", nil
	}
	b, _ := json.Marshal(map[string]any{"reason": reason, "max_age": maxAge.Seconds()})
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.url+"/snapshot", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var r struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	json.NewDecoder(res.Body).Decode(&r)
	if r.Error != "" {
		log.Printf("snapshot: %s", r.Error)
	}
	return r.ID, nil
}

type notesKey struct{}

// notes are extra facts a handler adds to its call's audit record
type notes struct {
	mu sync.Mutex
	m  map[string]any
}

// note adds a fact to the request's audit record
func note(r *http.Request, k string, v any) {
	if n, ok := r.Context().Value(notesKey{}).(*notes); ok {
		n.mu.Lock()
		n.m[k] = v
		n.mu.Unlock()
	}
}

type recorder struct {
	http.ResponseWriter
	status int
	errBuf bytes.Buffer // the body of an error response
}

func (w *recorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *recorder) Write(p []byte) (int, error) {
	if w.status >= 400 && w.errBuf.Len() < 1024 {
		w.errBuf.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// audited records the call, after a snapshot if it may change things
func (s *server) audited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(io.LimitReader(r.Body, 2<<20))
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		rec := map[string]any{
			"ts": start.Format(time.RFC3339Nano), "source": "agent",
			"method": r.Method, "path": r.URL.Path,
		}
		if q := r.URL.RawQuery; q != "" {
			rec["query"] = q
		}
		for _, win := range s.x.windows() {
			if win.Active {
				rec["window"] = win.Title
			}
		}
		if len(body) > 0 {
			var v any
			if json.Unmarshal(body, &v) == nil {
				rec["body"] = trimStrings(v)
			}
		}
		n := &notes{m: map[string]any{}}
		r = r.WithContext(context.WithValue(r.Context(), notesKey{}, n))

		if r.Method == http.MethodPost && r.URL.Path != "/agent/clipboard" {
			maxAge := s.audit.interval
			if r.URL.Path == "/agent/exec" {
				maxAge = 0 // shell commands can do anything: always a fresh undo point
			}
			if id, err := s.audit.snapshot(r.Context(), "before "+strings.TrimPrefix(r.URL.Path, "/agent/"), maxAge); err != nil {
				log.Printf("snapshot before %s: %v", r.URL.Path, err)
			} else if id != "" {
				rec["snapshot_before"] = id
			}
		}

		rw := &recorder{ResponseWriter: w, status: http.StatusOK}
		next(rw, r)

		rec["status"] = rw.status
		rec["ms"] = time.Since(start).Milliseconds()
		if rw.errBuf.Len() > 0 {
			var e struct{ Error string }
			json.Unmarshal(rw.errBuf.Bytes(), &e)
			rec["error"] = e.Error
		}
		alerts := sensitive(string(body) + " " + r.URL.RawQuery)
		n.mu.Lock()
		alerts = append(alerts, toStrings(n.m["alerts"])...) // from the handler, e.g. a prompt injection on the page
		delete(n.m, "alerts")
		if len(n.m) > 0 {
			rec["notes"] = n.m
		}
		n.mu.Unlock()
		if len(alerts) > 0 {
			rec["alerts"] = alerts
		}
		if _, alert := rec["alerts"]; alert || r.Method == http.MethodPost {
			line, _ := json.Marshal(rec["body"])
			log.Printf("audit: %s %s %d %v %.200s", r.Method, r.URL.Path, rw.status, rec["alerts"], line)
		}
		if s.audit.url != "" {
			select {
			case s.audit.queue <- rec:
			default:
				log.Printf("audit: queue full, dropped a record of %s", r.URL.Path)
			}
		}
	}
}

// trimStrings shortens long strings (typed text, screenshots) so a record
// stays small
func trimStrings(v any) any {
	switch t := v.(type) {
	case string:
		if len(t) > 500 {
			return t[:500] + "…"
		}
	case map[string]any:
		for k, x := range t {
			t[k] = trimStrings(x)
		}
	case []any:
		for i, x := range t {
			t[i] = trimStrings(x)
		}
	}
	return v
}

func toStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case string:
		return []string{t}
	}
	return nil
}

// The browser's profile holds every login (cookies, saved passwords). The
// agent API never needs it, so a call that names it is worth a look.
var sensitivePatterns = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`/home/browser|net\.imput\.helium|\.config/chromium`), "names the browser profile (logins, cookies)"},
	{regexp.MustCompile(`/(Cookies|Login Data|Local State|Web Data)\b`), "names a browser credential file"},
}

func sensitive(s string) []string {
	var out []string
	for _, p := range sensitivePatterns {
		if p.re.MatchString(s) {
			out = append(out, p.what)
		}
	}
	return out
}

// Text written to steer an AI agent: tells it to drop its instructions, or
// to hand over credentials
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override)\b.{0,30}\b(previous|prior|above|earlier|all|your|system)\b.{0,20}\b(instructions?|prompts?|rules|directions)\b`),
	regexp.MustCompile(`(?i)\b(upload|send|post|paste|email|exfiltrate|share|submit|copy)\b.{0,60}\b(cookies?|session tokens?|passwords?|credentials|api keys?|auth tokens?)\b`),
	regexp.MustCompile(`(?i)\b(ai|llm|language model)\s+(agents?|assistants?|models?)\b.{0,40}\b(must|should|are required to|need to)\b`),
}

// injection reports whether page text looks like a prompt injection
func injection(text string) bool {
	for _, re := range injectionPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}
