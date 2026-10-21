package main

// Agent API: lets an AI agent (through the MCP server) see and drive the
// computer without a viewer connected: screenshots, mouse, keyboard,
// windows, clipboard, apps and waits. Works on anything on screen. The
// browser's page structure (tabs, elements, navigation) is in browser.go.
//
// Auth: "Authorization: Bearer $AGENT_TOKEN" (or $CONTROL_TOKEN).
//
//	GET  /agent/status                     screen, pointer, browser running, exec
//	GET  /agent/screenshot                 ?format=jpeg|png&quality=80&scale=1&x=&y=&w=&h=&cursor=true
//	POST /agent/actions                    {"actions":[...], "screenshot":{...}} input, waits; optional screenshot after
//	GET  /agent/windows                    top-level windows
//	POST /agent/windows                    {"id", "action": activate|close|minimize|maximize|unmaximize|move|resize, ...}
//	GET  /agent/clipboard                  {"text"}
//	POST /agent/clipboard                  {"text"}
//	POST /agent/launch                     {"app": browser|terminal|files, "url"?, "path"?}
//	POST /agent/exec                       {"command", "timeout"?, "cwd"?} (only with AGENT_EXEC=true)
//	/agent/browser/...                     see browser.go (only with BROWSER_AUTOMATION=true, the default)

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (s *server) agentRoutes(mux *http.ServeMux) {
	h := func(pattern string, fn http.HandlerFunc) { mux.HandleFunc(pattern, s.agentAuth(fn)) }
	h("GET /agent/status", s.handleAgentStatus)
	h("GET /agent/screenshot", s.handleScreenshot)
	h("POST /agent/actions", s.handleActions)
	h("GET /agent/windows", s.handleWindows)
	h("POST /agent/windows", s.handleWindowAction)
	h("GET /agent/clipboard", s.handleClipboardGet)
	h("POST /agent/clipboard", s.handleClipboardSet)
	h("POST /agent/launch", s.handleLaunch)
	h("POST /agent/exec", s.handleExec)
	if s.cfg.browserAuto {
		s.browserRoutes(h)
	} else {
		mux.HandleFunc("/agent/browser/", s.agentAuth(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusForbidden, errors.New("browser automation is off (set BROWSER_AUTOMATION=true)"))
		}))
	}
}

func (s *server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	px, py := s.x.pointer()
	br := map[string]any{"running": false, "automation": s.cfg.browserAuto}
	for _, win := range s.x.windows() {
		if strings.EqualFold(win.App, browserClass) {
			br["running"] = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"width": s.fb.width, "height": s.fb.height,
		"pointer": map[string]int{"x": px, "y": py},
		"browser": br,
		"exec":    s.cfg.agentExec,
	})
}

// --- screenshots ---

type shotOptions struct {
	Format  string  `json:"format"`  // jpeg (default) or png
	Quality int     `json:"quality"` // jpeg, 1-100 (default 80)
	Scale   float64 `json:"scale"`   // 0.1-1 (default 1)
	X       int     `json:"x"`       // region (default: whole screen)
	Y       int     `json:"y"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	Cursor  *bool   `json:"cursor"` // draw the pointer (default true)
}

func shotFromQuery(r *http.Request) shotOptions {
	q := r.URL.Query()
	num := func(k string) int { n, _ := strconv.Atoi(q.Get(k)); return n }
	o := shotOptions{Format: q.Get("format"), Quality: num("quality"), X: num("x"), Y: num("y"), W: num("w"), H: num("h")}
	o.Scale, _ = strconv.ParseFloat(q.Get("scale"), 64)
	if c := q.Get("cursor"); c != "" {
		b := c == "true" || c == "1"
		o.Cursor = &b
	}
	return o
}

type shot struct {
	data          []byte
	mime          string
	width, height int // of the image
	scale         float64
}

func (s *server) screenshot(o shotOptions) (*shot, error) {
	fw, fh := s.fb.width, s.fb.height
	if o.W <= 0 || o.H <= 0 {
		o.X, o.Y, o.W, o.H = 0, 0, fw, fh
	}
	crop := image.Rect(o.X, o.Y, o.X+o.W, o.Y+o.H).Intersect(image.Rect(0, 0, fw, fh))
	if crop.Empty() {
		return nil, errors.New("region is outside the screen")
	}
	if o.Scale <= 0 || o.Scale > 1 {
		o.Scale = 1
	}
	o.Scale = max(o.Scale, 0.1)

	// BGRX framebuffer -> RGBA
	img := image.NewRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
	for y := 0; y < crop.Dy(); y++ {
		src := s.fb.pix[(crop.Min.Y+y)*s.fb.stride+crop.Min.X*4:]
		dst := img.Pix[y*img.Stride:]
		for x := 0; x < crop.Dx(); x++ {
			dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = src[x*4+2], src[x*4+1], src[x*4], 255
		}
	}
	if o.Cursor == nil || *o.Cursor {
		if c := s.x.cursor(); c != nil {
			at := image.Pt(c.x-crop.Min.X, c.y-crop.Min.Y)
			draw.Draw(img, c.img.Bounds().Add(at), c.img, image.Point{}, draw.Over)
		}
	}
	var out image.Image = img
	if o.Scale < 1 {
		out = downscale(img, o.Scale)
	}

	var buf bytes.Buffer
	sh := &shot{width: out.Bounds().Dx(), height: out.Bounds().Dy(), scale: o.Scale}
	switch o.Format {
	case "png":
		sh.mime = "image/png"
		if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, out); err != nil {
			return nil, err
		}
	case "", "jpeg", "jpg":
		q := o.Quality
		if q <= 0 || q > 100 {
			q = 80
		}
		sh.mime = "image/jpeg"
		if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: q}); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("format must be jpeg or png")
	}
	sh.data = buf.Bytes()
	return sh, nil
}

// downscale averages the source pixels under each output pixel (box filter)
func downscale(src *image.RGBA, scale float64) *image.RGBA {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := max(1, int(float64(sw)*scale+0.5)), max(1, int(float64(sh)*scale+0.5))
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		y0, y1 := y*sh/dh, max((y+1)*sh/dh, y*sh/dh+1)
		for x := 0; x < dw; x++ {
			x0, x1 := x*sw/dw, max((x+1)*sw/dw, x*sw/dw+1)
			var r, g, b, n int
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride:]
				for sx := x0; sx < x1; sx++ {
					r += int(row[sx*4])
					g += int(row[sx*4+1])
					b += int(row[sx*4+2])
					n++
				}
			}
			o := y*dst.Stride + x*4
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = byte(r/n), byte(g/n), byte(b/n), 255
		}
	}
	return dst
}

func (s *server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	sh, err := s.screenshot(shotFromQuery(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", sh.mime)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Image-Width", strconv.Itoa(sh.width))
	h.Set("X-Image-Height", strconv.Itoa(sh.height))
	h.Set("X-Screen-Width", strconv.Itoa(s.fb.width))
	h.Set("X-Screen-Height", strconv.Itoa(s.fb.height))
	h.Set("X-Scale", strconv.FormatFloat(sh.scale, 'f', -1, 64))
	w.Write(sh.data)
}

func (sh *shot) json() map[string]any {
	return map[string]any{
		"mime": sh.mime, "width": sh.width, "height": sh.height, "scale": sh.scale,
		"data": base64.StdEncoding.EncodeToString(sh.data),
	}
}

// --- input ---

// action is one step of POST /agent/actions. Coordinates are screen pixels.
//
//	{"type":"move", "x":..,"y":..}
//	{"type":"click", "x"?,"y"?, "button"?:"left|middle|right", "clicks"?:1}
//	{"type":"double_click", "x"?,"y"?}
//	{"type":"down"|"up", "x"?,"y"?, "button"?}
//	{"type":"drag", "x","y", "to_x","to_y", "button"?}
//	{"type":"scroll", "x"?,"y"?, "dy"?:3, "dx"?:0}     (wheel clicks; dy>0 is down)
//	{"type":"type", "text", "delay_ms"?}
//	{"type":"key", "keys":"ctrl+l"}                   (also key_down / key_up)
//	{"type":"wait", "until"?:"stable|change", "stable_ms"?:400, "timeout_ms"?:5000}
//	{"type":"sleep", "ms"}
//	{"type":"clipboard", "text"}                      (set the clipboard)
type action struct {
	Type      string `json:"type"`
	X         *int   `json:"x"`
	Y         *int   `json:"y"`
	ToX       int    `json:"to_x"`
	ToY       int    `json:"to_y"`
	Button    string `json:"button"`
	Clicks    int    `json:"clicks"`
	DX        int    `json:"dx"`
	DY        int    `json:"dy"`
	Text      string `json:"text"`
	Keys      string `json:"keys"`
	DelayMs   int    `json:"delay_ms"`
	Until     string `json:"until"`
	StableMs  int    `json:"stable_ms"`
	TimeoutMs int    `json:"timeout_ms"`
	Ms        int    `json:"ms"`
}

func buttonNumber(name string) (int, error) {
	switch name {
	case "", "left":
		return 1, nil
	case "middle":
		return 2, nil
	case "right":
		return 3, nil
	}
	return 0, fmt.Errorf("button must be left, middle or right (got %q)", name)
}

func (s *server) clamp(x, y int) (int, int) {
	return min(max(x, 0), s.fb.width-1), min(max(y, 0), s.fb.height-1)
}

// run performs one action; the result is non-nil for waits
func (s *server) run(ctx context.Context, a action) (any, error) {
	moveTo := func() {
		if a.X != nil && a.Y != nil {
			s.x.glide(s.clamp(*a.X, *a.Y))
		}
	}
	switch a.Type {
	case "move":
		if a.X == nil || a.Y == nil {
			return nil, errors.New("move needs x and y")
		}
		moveTo()
	case "click", "double_click", "down", "up":
		b, err := buttonNumber(a.Button)
		if err != nil {
			return nil, err
		}
		moveTo()
		switch a.Type {
		case "click":
			s.x.click(b, min(max(a.Clicks, 1), 3))
		case "double_click":
			s.x.click(b, 2)
		default:
			s.x.button(b, a.Type == "down")
		}
		if a.Type != "down" {
			settle(clickSettle)
		}
	case "drag":
		b, err := buttonNumber(a.Button)
		if err != nil {
			return nil, err
		}
		if a.X == nil || a.Y == nil {
			return nil, errors.New("drag needs x, y, to_x and to_y")
		}
		x0, y0 := s.clamp(*a.X, *a.Y)
		x1, y1 := s.clamp(a.ToX, a.ToY)
		s.x.glide(x0, y0)
		s.x.button(b, true)
		if s.x.human {
			s.x.glidePath(x1, y1, false)
		} else {
			const steps = 12 // apps need to see motion to start a drag
			for i := 1; i <= steps; i++ {
				s.x.sync()
				time.Sleep(12 * time.Millisecond)
				s.x.move(x0+(x1-x0)*i/steps, y0+(y1-y0)*i/steps)
			}
		}
		s.x.sync()
		time.Sleep(30 * time.Millisecond)
		s.x.button(b, false)
	case "scroll":
		moveTo()
		dx, dy := a.DX, a.DY
		if dx == 0 && dy == 0 {
			dy = 3
		}
		for i := 0; i < min(abs(dy), 50); i++ {
			s.x.wheelTick(0, dy, i)
		}
		for i := 0; i < min(abs(dx), 50); i++ {
			s.x.wheelTick(dx, 0, i)
		}
	case "type":
		if len(a.Text) > maxClipboard {
			return nil, errors.New("text too long")
		}
		return nil, s.x.typeText(a.Text, time.Duration(min(max(a.DelayMs, 0), 200))*time.Millisecond)
	case "key":
		if err := s.x.pressKeys(a.Keys); err != nil {
			return nil, err
		}
		settle(keySettle)
	case "key_down", "key_up":
		return nil, s.x.holdKeys(a.Keys, a.Type == "key_down")
	case "wait":
		return s.waitScreen(ctx, a.Until, a.StableMs, a.TimeoutMs)
	case "sleep":
		select {
		case <-time.After(time.Duration(min(max(a.Ms, 0), 30000)) * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case "clipboard":
		if len(a.Text) > maxClipboard {
			return nil, errors.New("text too long")
		}
		s.x.clip.set(a.Text)
	default:
		return nil, fmt.Errorf("unknown action type %q", a.Type)
	}
	s.x.sync()
	return nil, nil
}

// Apps move focus asynchronously after a shortcut (Ctrl+L, Ctrl+T) or a
// click: keys typed right after can land before the new field has focus
// (measured in Helium: with no gap the first characters are lost, 30 ms is
// enough). These pauses come after every such action.
const (
	keySettle   = 60 * time.Millisecond
	clickSettle = 30 * time.Millisecond
)

func settle(d time.Duration) {
	time.Sleep(d)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

type waitResult struct {
	Until    string `json:"until"`
	OK       bool   `json:"ok"` // stable / changed before the timeout
	WaitedMs int64  `json:"waited_ms"`
}

// waitScreen waits until the screen stops changing (stable) or until it
// changes at all (change)
func (s *server) waitScreen(ctx context.Context, until string, stableMs, timeoutMs int) (*waitResult, error) {
	if until == "" {
		until = "stable"
	}
	if until != "stable" && until != "change" {
		return nil, errors.New(`until must be "stable" or "change"`)
	}
	stable := time.Duration(min(max(stableMs, 50), 10000)) * time.Millisecond
	if stableMs == 0 {
		stable = 400 * time.Millisecond
	}
	timeout := time.Duration(min(max(timeoutMs, 100), 60000)) * time.Millisecond
	if timeoutMs == 0 {
		timeout = 5 * time.Second
	}

	start := time.Now()
	prev := bytes.Clone(s.fb.pix)
	last := start
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case now := <-tick.C:
			changed := !bytes.Equal(s.fb.pix, prev)
			res := &waitResult{Until: until, WaitedMs: now.Sub(start).Milliseconds()}
			if until == "change" && changed {
				res.OK = true
				return res, nil
			}
			if changed {
				copy(prev, s.fb.pix)
				last = now
			} else if until == "stable" && now.Sub(last) >= stable {
				res.OK = true
				return res, nil
			}
			if now.Sub(start) >= timeout {
				return res, nil
			}
		}
	}
}

func (s *server) handleActions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Actions    []action     `json:"actions"`
		Screenshot *shotOptions `json:"screenshot"`
	}
	if !readJSON(w, r, 1<<20, &req) {
		return
	}
	if len(req.Actions) > 100 {
		writeError(w, http.StatusBadRequest, errors.New("at most 100 actions per call"))
		return
	}
	results := make([]any, 0, len(req.Actions))
	for i, a := range req.Actions {
		res, err := s.run(r.Context(), a)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("action %d (%s): %v", i, a.Type, err), "done": i, "results": results,
			})
			return
		}
		results = append(results, res)
	}
	px, py := s.x.pointer()
	out := map[string]any{"done": len(req.Actions), "results": results, "pointer": map[string]int{"x": px, "y": py}}
	if req.Screenshot != nil {
		sh, err := s.screenshot(*req.Screenshot)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		out["screenshot"] = sh.json()
	}
	writeJSON(w, http.StatusOK, out)
}

// --- windows, clipboard, apps ---

func (s *server) handleWindows(w http.ResponseWriter, r *http.Request) {
	ws := s.x.windows()
	if ws == nil {
		ws = []window{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"windows": ws})
}

func (s *server) handleWindowAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     uint32 `json:"id"`
		Action string `json:"action"`
		X      int    `json:"x"`
		Y      int    `json:"y"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	if !readJSON(w, r, 4096, &req) {
		return
	}
	if err := s.x.windowAction(req.ID, req.Action, req.X, req.Y, req.Width, req.Height); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleClipboardGet(w http.ResponseWriter, r *http.Request) {
	text, err := s.x.clip.get(2 * time.Second)
	if err != nil {
		writeError(w, http.StatusGatewayTimeout, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

func (s *server) handleClipboardSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, maxClipboard+1024, &req) {
		return
	}
	s.x.clip.set(req.Text)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		App  string `json:"app"`
		URL  string `json:"url"`
		Path string `json:"path"`
	}
	if !readJSON(w, r, 8192, &req) {
		return
	}
	var cmd *exec.Cmd
	switch req.App {
	case "browser", browserCmd:
		args := []string{}
		if req.URL != "" {
			if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") && req.URL != "about:blank" {
				writeError(w, http.StatusBadRequest, errors.New("url must start with http:// or https://"))
				return
			}
			args = append(args, req.URL)
		}
		cmd = exec.Command(browserCmd, args...)
	case "terminal":
		cmd = exec.Command("xfce4-terminal")
		if req.Path != "" {
			cmd.Args = append(cmd.Args, "--working-directory="+req.Path)
		}
	case "files":
		cmd = exec.Command("thunar")
		if req.Path != "" {
			cmd.Args = append(cmd.Args, "--", req.Path)
		}
	default:
		writeError(w, http.StatusBadRequest, errors.New("app must be browser, terminal or files"))
		return
	}
	if err := start(cmd); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pid": cmd.Process.Pid})
}

// start runs an app detached from desktopd, in the home folder
func start(cmd *exec.Cmd) error {
	cmd.Dir = "/root"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// --- shell (off unless AGENT_EXEC=true) ---

const maxOutput = 256 << 10

type capped struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.buf.Len(); room < len(p) {
		c.buf.Write(p[:max(room, 0)])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (s *server) handleExec(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.agentExec {
		writeError(w, http.StatusForbidden, errors.New("exec is disabled (set AGENT_EXEC=true)"))
		return
	}
	var req struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"` // seconds, default 30, max 600
		Cwd     string `json:"cwd"`
	}
	if !readJSON(w, r, 64<<10, &req) {
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeError(w, http.StatusBadRequest, errors.New("command is empty"))
		return
	}
	timeout := time.Duration(min(max(req.Timeout, 1), 600)) * time.Second
	if req.Timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", "-lc", req.Command)
	cmd.Dir = "/root"
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = append(os.Environ(), "TERM=dumb")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) } // the whole process group
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	start := time.Now()
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	case err != nil && ctx.Err() == nil:
		writeError(w, http.StatusBadRequest, err)
		return
	}
	log.Printf("agent exec (%d): %.80q", code, req.Command)
	writeJSON(w, http.StatusOK, map[string]any{
		"exit_code": code, "stdout": stdout.buf.String(), "stderr": stderr.buf.String(),
		"truncated": stdout.truncated || stderr.truncated, "timed_out": ctx.Err() == context.DeadlineExceeded,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

// --- helpers ---

func readJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %v", err))
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
