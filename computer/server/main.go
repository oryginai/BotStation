// desktopd streams the container's X display to a browser and takes mouse
// and keyboard input back. A token-protected control API turns viewing on
// and off and hands out short-lived, single-use viewer tokens. Everything is
// served over TLS 1.3 (HTTPS/WSS) unless INSECURE_HTTP=true.
//
// Control API (all need "Authorization: Bearer $CONTROL_TOKEN"):
//
//	GET  /api/status    {"enabled", "viewers", "width", "height"}
//	POST /api/enable    allow viewers
//	POST /api/disable   stop streaming, disconnect every viewer
//	POST /api/sessions  {"view_only": bool} -> {"token", "expires_at", "view_url", ...}
//
// Viewer:  GET /stream (WebSocket; token in the subprotocol, see stream.go)
//
//	GET /view#token=...  the desktop alone, full window (token never reaches a server log)
//	GET /client.js       the stream client used by /view, importable by other pages
//
// Agent:   /agent/... screenshots, input, windows, clipboard, apps (see agent.go)
//
// Health:  GET /healthz (no auth)
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

//go:embed web
var web embed.FS

const (
	subprotocol = "desktop.v1"
	tokenPrefix = "token."
)

type config struct {
	listen         string
	controlToken   string
	display        string
	fbPath         string
	fps            int
	quality        int
	motionQuality  int
	maxViewers     int
	tokenTTL       time.Duration
	allowedOrigins []string
	frameAncestors string
	publicURL      string
	insecureHTTP   bool
	tlsCert        string
	tlsKey         string
	tlsHosts       []string
	tlsDir         string
	enabled        bool
	clipToDesktop  bool // viewers may paste into the desktop
	clipToViewer   bool // text copied in the desktop is sent to viewers
	agentAPI       bool
	agentToken     string // agent API only; CONTROL_TOKEN works there too
	agentExec      bool   // POST /agent/exec runs shell commands
	browserAuto    bool   // /agent/browser/... (helium.sh opens the debugging port)
	humanInput     bool   // agent input moves and types at a person's pace
}

func loadConfig() config {
	c := config{
		listen:         env("LISTEN", ":8443"),
		controlToken:   os.Getenv("CONTROL_TOKEN"),
		display:        env("DISPLAY", ":1"),
		fbPath:         env("FRAMEBUFFER", "/tmp/xvfb/Xvfb_screen0"),
		fps:            envInt("STREAM_FPS", 25),
		quality:        envInt("STREAM_QUALITY", 85),
		motionQuality:  envInt("STREAM_MOTION_QUALITY", 45),
		maxViewers:     envInt("MAX_VIEWERS", 2),
		tokenTTL:       time.Duration(envInt("VIEWER_TOKEN_TTL", 60)) * time.Second,
		allowedOrigins: strings.Split(env("ALLOWED_ORIGINS", "*"), ","),
		frameAncestors: env("FRAME_ANCESTORS", "'none'"),
		publicURL:      strings.TrimRight(os.Getenv("PUBLIC_URL"), "/"),
		insecureHTTP:   os.Getenv("INSECURE_HTTP") == "true",
		tlsCert:        os.Getenv("TLS_CERT"),
		tlsKey:         os.Getenv("TLS_KEY"),
		tlsHosts:       strings.Split(env("TLS_HOSTS", "localhost,127.0.0.1"), ","),
		tlsDir:         env("TLS_DIR", "/var/lib/desktopd"),
		enabled:        env("STREAM_ENABLED", "false") == "true",
		agentAPI:       env("AGENT_API", "true") == "true",
		agentToken:     os.Getenv("AGENT_TOKEN"),
		agentExec:      env("AGENT_EXEC", "false") == "true",
		browserAuto:    env("BROWSER_AUTOMATION", "true") == "true",
		humanInput:     env("AGENT_HUMAN_INPUT", "true") == "true",
	}
	if c.agentToken != "" && len(c.agentToken) < 32 {
		log.Fatal("AGENT_TOKEN must be at least 32 characters (try: openssl rand -hex 32)")
	}
	switch clip := env("CLIPBOARD", "both"); clip {
	case "both", "to-desktop", "from-desktop", "off":
		c.clipToDesktop = clip == "both" || clip == "to-desktop"
		c.clipToViewer = clip == "both" || clip == "from-desktop"
	default:
		log.Fatalf("CLIPBOARD must be both, to-desktop, from-desktop or off (got %q)", clip)
	}
	switch {
	case c.controlToken == "":
		c.controlToken = randomToken()
		log.Printf("CONTROL_TOKEN not set, generated one for this run: %s", c.controlToken)
	case len(c.controlToken) < 32:
		log.Fatal("CONTROL_TOKEN must be at least 32 characters (try: openssl rand -hex 32)")
	}
	return c
}

type session struct {
	expires  time.Time
	viewOnly bool
}

type server struct {
	cfg   config
	fb    *framebuffer
	x     *xconn
	br    *browser
	fails *limiter

	mu      sync.Mutex
	enabled bool
	tokens  map[string]session
	viewers map[*viewer]struct{}
	cursor  []byte // last cursor message, sent to new viewers
}

func main() {
	cfg := loadConfig()

	fb, err := openFramebuffer(cfg.fbPath, 30*time.Second)
	if err != nil {
		log.Fatalf("framebuffer: %v", err)
	}
	s := &server{
		cfg:     cfg,
		fb:      fb,
		br:      newBrowser(cdpAddr),
		fails:   newLimiter(10, time.Minute),
		enabled: cfg.enabled,
		tokens:  map[string]session{},
		viewers: map[*viewer]struct{}{},
	}
	s.x, err = openX(cfg.display, s.broadcastCursor, s.broadcastClipboard)
	if err != nil {
		log.Fatalf("X display %s: %v", cfg.display, err)
	}
	s.x.human = cfg.humanInput
	go s.trackPointer()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /api/status", s.auth(s.handleStatus))
	mux.HandleFunc("POST /api/enable", s.auth(s.handleEnable))
	mux.HandleFunc("POST /api/disable", s.auth(s.handleDisable))
	mux.HandleFunc("POST /api/sessions", s.auth(s.handleSession))
	mux.HandleFunc("GET /stream", s.handleStream)
	mux.HandleFunc("GET /view", s.static("web/view.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /view.js", s.static("web/view.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /client.js", s.static("web/client.js", "text/javascript; charset=utf-8"))
	if cfg.agentAPI {
		s.agentRoutes(mux)
	}

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           s.headers(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	if cfg.insecureHTTP {
		log.Printf("desktopd on http://%s (%dx%d, streaming %s) - UNENCRYPTED, only behind a TLS proxy",
			cfg.listen, fb.width, fb.height, onOff(s.enabled))
		log.Fatal(srv.ListenAndServe())
	}
	cert, err := loadOrCreateCert(cfg)
	if err != nil {
		log.Fatalf("TLS: %v", err)
	}
	srv.TLSConfig = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}
	log.Printf("desktopd on https://%s (%dx%d, streaming %s, agent API %s, exec %s, browser automation %s)",
		cfg.listen, fb.width, fb.height, onOff(s.enabled), onOff(cfg.agentAPI), onOff(cfg.agentExec), onOff(cfg.browserAuto))
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

// --- control API ---

func (s *server) status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"enabled": s.enabled,
		"viewers": len(s.viewers),
		"width":   s.fb.width,
		"height":  s.fb.height,
	}
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.status())
}

func (s *server) handleEnable(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.enabled = true
	s.mu.Unlock()
	log.Print("streaming enabled")
	writeJSON(w, http.StatusOK, s.status())
}

func (s *server) handleDisable(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.enabled = false
	clear(s.tokens)
	for v := range s.viewers {
		v.close(websocket.StatusGoingAway, "streaming disabled")
		delete(s.viewers, v)
	}
	s.mu.Unlock()
	log.Print("streaming disabled")
	writeJSON(w, http.StatusOK, s.status())
}

func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ViewOnly bool `json:"view_only"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "streaming is disabled"})
		return
	}
	now := time.Now()
	for t, sess := range s.tokens {
		if now.After(sess.expires) {
			delete(s.tokens, t)
		}
	}
	tok := randomToken()
	sess := session{expires: now.Add(s.cfg.tokenTTL), viewOnly: req.ViewOnly}
	s.tokens[tok] = sess
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":       tok,
		"expires_at":  sess.expires.UTC().Format(time.RFC3339),
		"view_only":   sess.viewOnly,
		"stream_path": "/stream",
		"view_url":    s.baseURL(r) + "/view#token=" + tok,
	})
}

// --- viewer ---

func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.fails.blocked(ip) {
		http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
		return
	}
	// The token comes in the WebSocket subprotocol list, never in the URL,
	// so it does not end up in proxy or access logs
	var tok string
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if p = strings.TrimSpace(p); strings.HasPrefix(p, tokenPrefix) {
			tok = strings.TrimPrefix(p, tokenPrefix)
		}
	}

	// Tokens are single use: taken out of the map before anything else
	s.mu.Lock()
	sess, ok := s.tokens[tok]
	delete(s.tokens, tok)
	switch {
	case !s.enabled:
		s.mu.Unlock()
		http.Error(w, "streaming is disabled", http.StatusConflict)
		return
	case tok == "" || !ok || time.Now().After(sess.expires):
		s.mu.Unlock()
		s.fails.fail(ip)
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return
	case len(s.viewers) >= s.cfg.maxViewers:
		s.mu.Unlock()
		http.Error(w, "too many viewers", http.StatusServiceUnavailable)
		return
	}
	s.mu.Unlock()

	opts := &websocket.AcceptOptions{
		Subprotocols:    []string{subprotocol},
		CompressionMode: websocket.CompressionDisabled, // JPEG does not compress further
	}
	if s.anyOrigin() {
		opts.InsecureSkipVerify = true
	} else {
		opts.OriginPatterns = s.cfg.allowedOrigins
	}
	ws, err := websocket.Accept(w, r, opts)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20) // room for clipboard text

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := &viewer{ws: ws, cancel: cancel, viewOnly: sess.viewOnly}

	s.mu.Lock()
	if !s.enabled { // disabled while we were upgrading
		s.mu.Unlock()
		ws.Close(websocket.StatusGoingAway, "streaming disabled")
		return
	}
	s.viewers[v] = struct{}{}
	cursor := s.cursor
	s.mu.Unlock()
	log.Printf("viewer connected from %s (view_only=%v)", ip, v.viewOnly)

	defer func() {
		s.mu.Lock()
		delete(s.viewers, v)
		s.mu.Unlock()
		if !v.viewOnly {
			s.x.releaseAll()
		}
		ws.CloseNow()
		log.Printf("viewer from %s disconnected", ip)
	}()

	s.serveViewer(ctx, v, cursor)
}

func (s *server) broadcastCursor(msg []byte) {
	s.mu.Lock()
	s.cursor = msg
	vs := make([]*viewer, 0, len(s.viewers))
	for v := range s.viewers {
		vs = append(vs, v)
	}
	s.mu.Unlock()
	for _, v := range vs {
		v.send(websocket.MessageText, msg)
	}
}

// trackPointer sends the pointer's position to every viewer whenever it
// moves, so viewers see it move when the agent (not they) moves it.
func (s *server) trackPointer() {
	tick := time.NewTicker(time.Second / time.Duration(s.cfg.fps))
	defer tick.Stop()
	lx, ly, ln := -1, -1, 0
	for range tick.C {
		s.mu.Lock()
		vs := make([]*viewer, 0, len(s.viewers))
		for v := range s.viewers {
			vs = append(vs, v)
		}
		s.mu.Unlock()
		if len(vs) == 0 {
			ln = 0
			continue
		}
		x, y := s.x.pointer()
		if x == lx && y == ly && len(vs) <= ln { // resend when a viewer joins
			ln = len(vs)
			continue
		}
		lx, ly, ln = x, y, len(vs)
		msg, _ := json.Marshal(map[string]any{"type": "pointer", "x": x, "y": y})
		for _, v := range vs {
			v.send(websocket.MessageText, msg)
		}
	}
}

// broadcastClipboard sends text copied in the desktop to every viewer that
// may control it (never to view-only viewers)
func (s *server) broadcastClipboard(text string) {
	if !s.cfg.clipToViewer {
		return
	}
	msg, _ := json.Marshal(map[string]string{"type": "clipboard", "text": text})
	s.mu.Lock()
	vs := make([]*viewer, 0, len(s.viewers))
	for v := range s.viewers {
		if !v.viewOnly {
			vs = append(vs, v)
		}
	}
	s.mu.Unlock()
	for _, v := range vs {
		v.send(websocket.MessageText, msg)
	}
}

// --- helpers ---

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return s.authAny(next, s.cfg.controlToken)
}

// agentAuth accepts the agent token or the control token
func (s *server) agentAuth(next http.HandlerFunc) http.HandlerFunc {
	return s.authAny(next, s.cfg.controlToken, s.cfg.agentToken)
}

func (s *server) authAny(next http.HandlerFunc, tokens ...string) http.HandlerFunc {
	var wants [][]byte
	for _, t := range tokens {
		if t != "" {
			wants = append(wants, []byte("Bearer "+t))
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if s.fails.blocked(ip) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed attempts"})
			return
		}
		got, ok := []byte(r.Header.Get("Authorization")), 0
		for _, want := range wants {
			ok |= subtle.ConstantTimeCompare(got, want)
		}
		if ok != 1 {
			s.fails.fail(ip)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// headers adds security headers everywhere, and CORS for the API and
// client.js. API auth is a bearer header (never cookies), so allowing any
// origin does not expose ambient credentials.
func (s *server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "frame-ancestors "+s.cfg.frameAncestors)
		if !s.cfg.insecureHTTP {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}

		cors := strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/agent/") || r.URL.Path == "/client.js"
		if origin := r.Header.Get("Origin"); origin != "" && cors {
			if s.anyOrigin() {
				h.Set("Access-Control-Allow-Origin", "*")
			} else if s.originAllowed(origin) {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
			}
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Allow-Methods", "GET, POST")
			h.Set("Access-Control-Expose-Headers", "X-Image-Width, X-Image-Height, X-Screen-Width, X-Screen-Height, X-Scale")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) static(name, contentType string) http.HandlerFunc {
	body, err := web.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		if strings.HasSuffix(name, ".html") {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; "+
				"img-src data: blob:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; "+
				"frame-ancestors "+s.cfg.frameAncestors)
		}
		w.Write(body)
	}
}

func (s *server) baseURL(r *http.Request) string {
	if s.cfg.publicURL != "" {
		return s.cfg.publicURL
	}
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

func (s *server) anyOrigin() bool {
	return len(s.cfg.allowedOrigins) == 1 && s.cfg.allowedOrigins[0] == "*"
}

func (s *server) originAllowed(origin string) bool {
	for _, o := range s.cfg.allowedOrigins {
		if strings.EqualFold(strings.TrimSpace(o), origin) {
			return true
		}
	}
	return false
}

// limiter blocks an IP after too many failed auth attempts in a window
type limiter struct {
	max    int
	window time.Duration
	mu     sync.Mutex
	ips    map[string]*failures
}

type failures struct {
	n     int
	reset time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, ips: map[string]*failures{}}
}

func (l *limiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.ips[ip]
	return ok && f.n >= l.max && time.Now().Before(f.reset)
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.ips) > 10000 { // drop stale entries
		for k, f := range l.ips {
			if now.After(f.reset) {
				delete(l.ips, k)
			}
		}
	}
	f, ok := l.ips[ip]
	if !ok || now.After(f.reset) {
		f = &failures{reset: now.Add(l.window)}
		l.ips[ip] = f
	}
	f.n++
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}

func onOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}
