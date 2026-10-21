package main

import (
	"errors"
	"fmt"
	"image"
	"math"
	"math/rand/v2"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

// X-side helpers for the agent API: typing any text, key combos, windows,
// the pointer. Viewers use xconn.key (physical keys); agents send text and
// key names, which are turned into keysyms and looked up in the X keymap.

// keymap is the X keyboard mapping: where each keysym sits (keycode, and
// whether Shift is needed), plus free keycodes used for characters that
// are not on the keyboard (they are mapped on the fly, like xdotool does).
type keymap struct {
	at      map[xproto.Keysym]keyPos
	perCode int
	free    []xproto.Keycode
	next    int                              // next free keycode to reuse
	scratch map[xproto.Keysym]xproto.Keycode // keysyms mapped onto free keycodes
	used    map[xproto.Keycode]xproto.Keysym // and the other way round
	when    map[xproto.Keycode]time.Time     // when each free keycode was last mapped
}

type keyPos struct {
	code  xproto.Keycode
	shift bool
}

func (x *xconn) loadKeymap() error {
	setup := xproto.Setup(x.c)
	first, count := setup.MinKeycode, int(setup.MaxKeycode-setup.MinKeycode)+1
	r, err := xproto.GetKeyboardMapping(x.c, first, byte(count)).Reply()
	if err != nil {
		return err
	}
	km := &keymap{
		at: map[xproto.Keysym]keyPos{}, perCode: int(r.KeysymsPerKeycode),
		scratch: map[xproto.Keysym]xproto.Keycode{}, used: map[xproto.Keycode]xproto.Keysym{},
		when: map[xproto.Keycode]time.Time{},
	}
	for i := 0; i < count; i++ {
		code := first + xproto.Keycode(i)
		syms := r.Keysyms[i*km.perCode : (i+1)*km.perCode]
		empty := true
		for level, ks := range syms {
			if ks == 0 {
				continue
			}
			empty = false
			if level > 1 { // AltGr levels: not reachable with Shift alone
				continue
			}
			if p, ok := km.at[ks]; !ok || (p.shift && level == 0) {
				km.at[ks] = keyPos{code, level == 1}
			}
		}
		if empty {
			km.free = append(km.free, code)
		}
	}
	x.km = km
	return nil
}

// keyFor returns where ks is, mapping it onto a free keycode if needed.
// Called with x.typing held.
func (x *xconn) keyFor(ks xproto.Keysym) (keyPos, error) {
	km := x.km
	if p, ok := km.at[ks]; ok {
		return p, nil
	}
	if code, ok := km.scratch[ks]; ok {
		return keyPos{code: code}, nil
	}
	if len(km.free) == 0 {
		return keyPos{}, fmt.Errorf("no free keycode for keysym %#x", ks)
	}
	code := km.free[km.next%len(km.free)]
	km.next++
	// An app may not have read the old meaning of this keycode yet: give it
	// a moment before reusing a keycode mapped just now
	if t, ok := km.when[code]; ok {
		x.sync()
		if d := 50*time.Millisecond - time.Since(t); d > 0 {
			time.Sleep(d)
		}
	}
	if old, ok := km.used[code]; ok {
		delete(km.scratch, old)
	}
	syms := make([]xproto.Keysym, km.perCode)
	syms[0] = ks
	if km.perCode > 1 {
		syms[1] = ks
	}
	if err := xproto.ChangeKeyboardMappingChecked(x.c, 1, code, byte(km.perCode), syms).Check(); err != nil {
		return keyPos{}, err
	}
	km.scratch[ks], km.used[code], km.when[code] = code, ks, time.Now()
	return keyPos{code: code}, nil
}

const (
	ksShift   = 0xffe1
	ksControl = 0xffe3
	ksAlt     = 0xffe9
	ksSuper   = 0xffeb
	ksReturn  = 0xff0d
	ksTab     = 0xff09
)

// runeKeysym is the keysym that types r (X11 keysym encoding: Latin-1 as
// is, any other Unicode character as 0x01000000 + code point)
func runeKeysym(r rune) xproto.Keysym {
	switch {
	case r == '\n' || r == '\r':
		return ksReturn
	case r == '\t':
		return ksTab
	case r >= 0x20 && r <= 0x7e, r >= 0xa0 && r <= 0xff:
		return xproto.Keysym(r)
	default:
		return xproto.Keysym(0x01000000 + r)
	}
}

// humanKeyDelay is the gap between keys when typing at a person's pace and
// no delay was asked for; text longer than humanTypeMax is typed at once,
// like a paste
const (
	humanKeyDelay = 30 * time.Millisecond
	humanTypeMax  = 300
)

// typeText types text as if on a keyboard, with delay between characters.
// At a person's pace, each gap varies by up to 40% either way.
func (x *xconn) typeText(text string, delay time.Duration) error {
	if !utf8.ValidString(text) {
		return errors.New("text is not valid UTF-8")
	}
	if x.human && delay == 0 && utf8.RuneCountInString(text) <= humanTypeMax {
		delay = humanKeyDelay
	}
	x.typing.Lock()
	defer x.typing.Unlock()
	shift, err := x.keyFor(ksShift)
	if err != nil {
		return err
	}
	for i, r := range text {
		if r == '\r' && strings.HasPrefix(text[i+1:], "\n") {
			continue
		}
		p, err := x.keyFor(runeKeysym(r))
		if err != nil {
			return err
		}
		if p.shift {
			x.fake(xproto.KeyPress, xproto.KeyRelease, byte(shift.code), true)
		}
		x.fake(xproto.KeyPress, xproto.KeyRelease, byte(p.code), true)
		x.fake(xproto.KeyPress, xproto.KeyRelease, byte(p.code), false)
		if p.shift {
			x.fake(xproto.KeyPress, xproto.KeyRelease, byte(shift.code), false)
		}
		if delay > 0 {
			x.sync()
			if x.human {
				time.Sleep(time.Duration(float64(delay) * (0.6 + 0.8*rand.Float64())))
			} else {
				time.Sleep(delay)
			}
		} else if i%64 == 63 {
			x.sync() // don't run far ahead of the X server
		}
	}
	x.sync()
	return nil
}

// keyNames are the names accepted in key combos besides single characters
var keyNames = func() map[string]xproto.Keysym {
	m := map[string]xproto.Keysym{
		"ctrl": ksControl, "control": ksControl, "shift": ksShift, "alt": ksAlt, "option": ksAlt,
		"super": ksSuper, "meta": ksSuper, "win": ksSuper, "cmd": ksSuper,
		"enter": ksReturn, "return": ksReturn, "tab": ksTab, "space": ' ',
		"esc": 0xff1b, "escape": 0xff1b, "backspace": 0xff08, "delete": 0xffff, "del": 0xffff,
		"insert": 0xff63, "home": 0xff50, "end": 0xff57, "pageup": 0xff55, "pagedown": 0xff56,
		"left": 0xff51, "up": 0xff52, "right": 0xff53, "down": 0xff54,
		"capslock": 0xffe5, "menu": 0xff67, "print": 0xff61, "printscreen": 0xff61,
		"plus": '+', "minus": '-',
	}
	for i := 1; i <= 12; i++ {
		m[fmt.Sprintf("f%d", i)] = xproto.Keysym(0xffbe + i - 1)
	}
	return m
}()

// parseKeys turns "ctrl+shift+t" into keysyms, modifiers first
func parseKeys(combo string) ([]xproto.Keysym, error) {
	combo = strings.TrimSpace(combo)
	if combo == "" {
		return nil, errors.New("empty key combo")
	}
	var parts []string
	if combo == "+" {
		parts = []string{"+"}
	} else {
		parts = strings.Split(combo, "+")
		if strings.HasSuffix(combo, "++") { // "ctrl++"
			parts = append(parts[:len(parts)-2], "+")
		}
	}
	syms := make([]xproto.Keysym, 0, len(parts))
	for _, p := range parts {
		name := strings.ToLower(strings.TrimSpace(p))
		if ks, ok := keyNames[name]; ok {
			syms = append(syms, ks)
			continue
		}
		if utf8.RuneCountInString(p) == 1 {
			r, _ := utf8.DecodeRuneInString(p)
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A' // ctrl+T is ctrl+t; add shift explicitly
			}
			syms = append(syms, runeKeysym(r))
			continue
		}
		return nil, fmt.Errorf("unknown key %q", p)
	}
	return syms, nil
}

// pressKeys presses a combo and releases it in reverse order
func (x *xconn) pressKeys(combo string) error {
	syms, err := parseKeys(combo)
	if err != nil {
		return err
	}
	x.typing.Lock()
	defer x.typing.Unlock()
	var codes []byte
	for _, ks := range syms {
		p, err := x.keyFor(ks)
		if err != nil {
			return err
		}
		if p.shift && !contains(syms, ksShift) { // e.g. "ctrl+?"
			s, _ := x.keyFor(ksShift)
			codes = append(codes, byte(s.code))
		}
		codes = append(codes, byte(p.code))
	}
	for _, c := range codes {
		x.fake(xproto.KeyPress, xproto.KeyRelease, c, true)
	}
	for i := len(codes) - 1; i >= 0; i-- {
		x.fake(xproto.KeyPress, xproto.KeyRelease, codes[i], false)
	}
	x.sync()
	return nil
}

// holdKeys presses (down) or releases a combo's keys without the other half
func (x *xconn) holdKeys(combo string, down bool) error {
	syms, err := parseKeys(combo)
	if err != nil {
		return err
	}
	x.typing.Lock()
	defer x.typing.Unlock()
	for _, ks := range syms {
		p, err := x.keyFor(ks)
		if err != nil {
			return err
		}
		x.mu.Lock()
		x.keys[byte(p.code)] = down
		x.mu.Unlock()
		x.fake(xproto.KeyPress, xproto.KeyRelease, byte(p.code), down)
	}
	x.sync()
	return nil
}

func contains(s []xproto.Keysym, v xproto.Keysym) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// sync waits until the X server has processed every request sent so far
func (x *xconn) sync() {
	xproto.GetInputFocus(x.c).Reply()
}

func (x *xconn) pointer() (int, int) {
	r, err := xproto.QueryPointer(x.c, x.root).Reply()
	if err != nil {
		return 0, 0
	}
	return int(r.RootX), int(r.RootY)
}

// glide moves the pointer to px, py. At a person's pace it travels there
// along humanPath and rests a moment, so pages see the mouse arrive before
// a click instead of jumping; otherwise it jumps.
func (x *xconn) glide(px, py int) { x.glidePath(px, py, true) }

// glidePath is glide; overshoot=false for drags, which must not pass over
// other drop targets.
func (x *xconn) glidePath(px, py int, overshoot bool) {
	fx, fy := x.pointer()
	if !x.human || math.Hypot(float64(px-fx), float64(py-fy)) < 4 {
		x.move(px, py)
		return
	}
	r := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	for _, st := range humanPath(fx, fy, px, py, r, overshoot) {
		x.move(st.x, st.y)
		x.sync()
		time.Sleep(st.pause)
	}
	time.Sleep(time.Duration(30+rand.IntN(50)) * time.Millisecond)
}

// pathStep is one pointer position of a glide and the pause after it
type pathStep struct {
	x, y  int
	pause time.Duration
}

// humanPath plans a hand-like move from fx, fy to px, py:
//   - a cubic Bézier curve whose control points sit off the straight line,
//     on one side (an arc) or, one time in four, on both (an S), bending by
//     up to 12% of the distance (max 40 px);
//   - travelled with a minimum-jerk speed profile (slow start, fast middle,
//     slow arrival, as measured in human reaching), 120-350 ms by distance,
//     with each step's time varied by ±25% and up to ±0.5 px of tremor;
//   - on moves over 150 px with overshoot, one time in three it ends 3-7%
//     past the target (max 18 px), pauses, and corrects back in a few
//     short steps.
//
// The last step is always exactly px, py.
func humanPath(fx, fy, px, py int, r *rand.Rand, overshoot bool) []pathStep {
	sx, sy := float64(fx), float64(fy)
	tx, ty := float64(px), float64(py)
	dist := math.Hypot(tx-sx, ty-sy)
	if dist < 1 {
		return []pathStep{{px, py, 0}}
	}
	ux, uy := (tx-sx)/dist, (ty-sy)/dist // along the move
	nx, ny := -uy, ux                    // across it

	ex, ey := tx, ty // where the curve ends
	over := overshoot && dist > 150 && r.Float64() < 1.0/3
	if over {
		o := min(dist*(0.03+0.04*r.Float64()), 18)
		side := (2*r.Float64() - 1) * o * 0.4
		ex, ey = tx+ux*o+nx*side, ty+uy*o+ny*side
	}
	bend := min(dist*0.12, 40)
	c1 := (2*r.Float64() - 1) * bend
	c2 := c1 * (0.3 + 0.7*r.Float64())
	if r.Float64() < 0.25 {
		c2 = -c2
	}
	a1, a2 := 0.2+0.2*r.Float64(), 0.6+0.2*r.Float64() // how far along the control points sit
	p1x, p1y := sx+(ex-sx)*a1+nx*c1, sy+(ey-sy)*a1+ny*c1
	p2x, p2y := sx+(ex-sx)*a2+nx*c2, sy+(ey-sy)*a2+ny*c2

	n := int(min(max(dist/25, 10), 24))
	step := time.Duration(min(120+dist*0.3, 350)) * time.Millisecond / time.Duration(n)
	vary := func(d time.Duration) time.Duration { return time.Duration(float64(d) * (0.75 + 0.5*r.Float64())) }
	steps := make([]pathStep, 0, n+5)
	for i := 1; i <= n; i++ {
		t := float64(i) / float64(n)
		t = t * t * t * (10 + t*(6*t-15)) // minimum jerk
		u := 1 - t
		bx := u*u*u*sx + 3*u*u*t*p1x + 3*u*t*t*p2x + t*t*t*ex
		by := u*u*u*sy + 3*u*u*t*p1y + 3*u*t*t*p2y + t*t*t*ey
		if i < n {
			bx, by = bx+r.Float64()-0.5, by+r.Float64()-0.5
		}
		steps = append(steps, pathStep{int(math.Round(bx)), int(math.Round(by)), vary(step)})
	}
	if over { // notice, then correct back
		steps[len(steps)-1].pause += time.Duration(40+r.IntN(50)) * time.Millisecond
		k := 3 + r.IntN(3)
		for i := 1; i <= k; i++ {
			t := float64(i) / float64(k)
			t = t * t * (3 - 2*t)
			steps = append(steps, pathStep{int(math.Round(ex + (tx-ex)*t)), int(math.Round(ey + (ty-ey)*t)),
				vary(18 * time.Millisecond)})
		}
	}
	steps[len(steps)-1].x, steps[len(steps)-1].y = px, py
	return steps
}

// wheelTick is the i-th wheel click of a scroll. At a person's pace the
// clicks come 20-50 ms apart, as from a real wheel, not all at once.
func (x *xconn) wheelTick(dx, dy, i int) {
	if x.human && i > 0 {
		x.sync()
		time.Sleep(time.Duration(20+rand.IntN(30)) * time.Millisecond)
	}
	x.wheel(dx, dy)
}

func (x *xconn) click(b, count int) {
	for i := 0; i < count; i++ {
		x.fake(xproto.ButtonPress, xproto.ButtonRelease, byte(b), true)
		if x.human { // a pressed button stays down for a moment
			x.sync()
			time.Sleep(time.Duration(25+rand.IntN(35)) * time.Millisecond)
		}
		x.fake(xproto.ButtonPress, xproto.ButtonRelease, byte(b), false)
		if i < count-1 {
			x.sync()
			time.Sleep(40 * time.Millisecond) // well inside the double-click time
		}
	}
}

// --- pointer image, drawn into screenshots ---

type cursorImage struct {
	img  *image.NRGBA
	x, y int // top-left on screen
}

func (x *xconn) cursor() *cursorImage {
	r, err := xfixes.GetCursorImage(x.c).Reply()
	if err != nil || r.Width == 0 || r.Height == 0 {
		return nil
	}
	img := image.NewNRGBA(image.Rect(0, 0, int(r.Width), int(r.Height)))
	for i, p := range r.CursorImage {
		a := p >> 24
		if a == 0 {
			continue
		}
		img.Pix[i*4] = byte((p >> 16 & 0xff) * 255 / a)
		img.Pix[i*4+1] = byte((p >> 8 & 0xff) * 255 / a)
		img.Pix[i*4+2] = byte((p & 0xff) * 255 / a)
		img.Pix[i*4+3] = byte(a)
	}
	return &cursorImage{img: img, x: int(r.X) - int(r.Xhot), y: int(r.Y) - int(r.Yhot)}
}

// --- windows (EWMH, as set by xfwm4) ---

type window struct {
	ID        uint32 `json:"id"`
	Title     string `json:"title"`
	App       string `json:"app"` // WM_CLASS class, e.g. "Chromium-browser", "Thunar"
	PID       int    `json:"pid,omitempty"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Active    bool   `json:"active"`
	Minimized bool   `json:"minimized"`
	Maximized bool   `json:"maximized"`
}

func (x *xconn) atom(name string) xproto.Atom {
	x.mu.Lock()
	a, ok := x.atoms[name]
	x.mu.Unlock()
	if ok {
		return a
	}
	r, err := xproto.InternAtom(x.c, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0
	}
	x.mu.Lock()
	x.atoms[name] = r.Atom
	x.mu.Unlock()
	return r.Atom
}

func (x *xconn) prop(win xproto.Window, name string) []byte {
	r, err := xproto.GetProperty(x.c, false, win, x.atom(name), xproto.GetPropertyTypeAny, 0, 1<<16).Reply()
	if err != nil {
		return nil
	}
	return r.Value
}

func le32s(b []byte) []uint32 {
	out := make([]uint32, len(b)/4)
	for i := range out {
		out[i] = uint32(b[i*4]) | uint32(b[i*4+1])<<8 | uint32(b[i*4+2])<<16 | uint32(b[i*4+3])<<24
	}
	return out
}

func (x *xconn) windows() []window {
	var active uint32
	if a := le32s(x.prop(x.root, "_NET_ACTIVE_WINDOW")); len(a) > 0 {
		active = a[0]
	}
	dock, desktop := uint32(x.atom("_NET_WM_WINDOW_TYPE_DOCK")), uint32(x.atom("_NET_WM_WINDOW_TYPE_DESKTOP"))
	hidden, maxV := uint32(x.atom("_NET_WM_STATE_HIDDEN")), uint32(x.atom("_NET_WM_STATE_MAXIMIZED_VERT"))

	var out []window
	for _, id := range le32s(x.prop(x.root, "_NET_CLIENT_LIST")) {
		win := xproto.Window(id)
		types := le32s(x.prop(win, "_NET_WM_WINDOW_TYPE"))
		if containsU32(types, dock) || containsU32(types, desktop) {
			continue
		}
		w := window{ID: id, Active: id == active}
		if t := x.prop(win, "_NET_WM_NAME"); len(t) > 0 {
			w.Title = string(t)
		} else {
			w.Title = string(x.prop(win, "WM_NAME"))
		}
		if cls := strings.Split(strings.TrimRight(string(x.prop(win, "WM_CLASS")), "\x00"), "\x00"); len(cls) > 0 {
			w.App = cls[len(cls)-1]
		}
		if p := le32s(x.prop(win, "_NET_WM_PID")); len(p) > 0 {
			w.PID = int(p[0])
		}
		state := le32s(x.prop(win, "_NET_WM_STATE"))
		w.Minimized, w.Maximized = containsU32(state, hidden), containsU32(state, maxV)
		if g, err := xproto.GetGeometry(x.c, xproto.Drawable(win)).Reply(); err == nil {
			w.Width, w.Height = int(g.Width), int(g.Height)
		}
		if t, err := xproto.TranslateCoordinates(x.c, win, x.root, 0, 0).Reply(); err == nil {
			w.X, w.Y = int(t.DstX), int(t.DstY)
		}
		out = append(out, w)
	}
	return out
}

func containsU32(s []uint32, v uint32) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// clientMessage asks the window manager to do something to a window
func (x *xconn) clientMessage(win xproto.Window, msg string, data ...uint32) {
	d := make([]uint32, 5)
	copy(d, data)
	ev := xproto.ClientMessageEvent{
		Format: 32, Window: win, Type: x.atom(msg),
		Data: xproto.ClientMessageDataUnionData32New(d),
	}
	xproto.SendEvent(x.c, false, x.root,
		xproto.EventMaskSubstructureRedirect|xproto.EventMaskSubstructureNotify, string(ev.Bytes()))
}

// windowAction: activate, close, minimize, maximize, unmaximize, move
// (x, y), resize (width, height)
func (x *xconn) windowAction(id uint32, action string, px, py, w, h int) error {
	found := false
	for _, win := range x.windows() {
		found = found || win.ID == id
	}
	if !found {
		return fmt.Errorf("no window %d", id)
	}
	win := xproto.Window(id)
	const sourcePager = 2 // "a pager or taskbar asked": WMs honour it without focus-stealing checks
	maxV, maxH := uint32(x.atom("_NET_WM_STATE_MAXIMIZED_VERT")), uint32(x.atom("_NET_WM_STATE_MAXIMIZED_HORZ"))
	switch action {
	case "activate":
		x.clientMessage(win, "_NET_ACTIVE_WINDOW", sourcePager, 0)
	case "close":
		x.clientMessage(win, "_NET_CLOSE_WINDOW", 0, sourcePager)
	case "minimize":
		x.clientMessage(win, "WM_CHANGE_STATE", 3) // IconicState
	case "maximize":
		x.clientMessage(win, "_NET_WM_STATE", 1, maxV, maxH, sourcePager)
	case "unmaximize":
		x.clientMessage(win, "_NET_WM_STATE", 0, maxV, maxH, sourcePager)
	case "move", "resize":
		// _NET_MOVERESIZE_WINDOW: gravity default, flags say which of x/y/w/h are set
		flags := uint32(sourcePager) << 12
		if action == "move" {
			flags |= 1<<8 | 1<<9
		} else {
			flags |= 1<<10 | 1<<11
		}
		x.clientMessage(win, "_NET_MOVERESIZE_WINDOW", flags, uint32(px), uint32(py), uint32(w), uint32(h))
	default:
		return fmt.Errorf("unknown window action %q", action)
	}
	x.sync()
	return nil
}
