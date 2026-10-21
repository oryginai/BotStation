package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// xconn injects viewer input with XTEST, reports the pointer's shape via
// XFIXES and bridges the clipboard. The pointer is hidden on the X side:
// viewers draw it themselves from the shape, so it moves with no network lag.
type xconn struct {
	c    *xgb.Conn
	root xproto.Window
	clip *clipboard

	mu      sync.Mutex
	keys    map[byte]bool // pressed keycodes, released when a viewer leaves
	buttons map[byte]bool
	atoms   map[string]xproto.Atom

	typing sync.Mutex // agent typing: owns km
	km     *keymap

	human bool // agent input at a person's pace (AGENT_HUMAN_INPUT): glide, typeText, click
}

func openX(display string, onCursor func([]byte), onClipboard func(string)) (*xconn, error) {
	var c *xgb.Conn
	var err error
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if c, err = xgb.NewConnDisplay(display); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if err = xtest.Init(c); err != nil {
		return nil, err
	}
	if err = xfixes.Init(c); err != nil {
		return nil, err
	}
	if _, err = xfixes.QueryVersion(c, 5, 0).Reply(); err != nil {
		return nil, err
	}

	x := &xconn{
		c:       c,
		root:    xproto.Setup(c).DefaultScreen(c).Root,
		keys:    map[byte]bool{},
		buttons: map[byte]bool{},
		atoms:   map[string]xproto.Atom{},
	}
	if err = x.loadKeymap(); err != nil {
		return nil, err
	}
	xfixes.HideCursor(c, x.root)
	xfixes.SelectCursorInput(c, x.root, xfixes.CursorNotifyMaskDisplayCursor)
	if x.clip, err = newClipboard(c, x.root, onClipboard); err != nil {
		return nil, err
	}
	go x.events(onCursor)
	if msg := x.cursorMessage(); msg != nil {
		onCursor(msg)
	}
	return x, nil
}

func (x *xconn) events(onCursor func([]byte)) {
	for {
		ev, err := x.c.WaitForEvent()
		if ev == nil && err == nil {
			log.Fatal("X connection closed")
		}
		if x.clip.handle(ev) {
			continue
		}
		if _, ok := ev.(xfixes.CursorNotifyEvent); ok {
			if msg := x.cursorMessage(); msg != nil {
				onCursor(msg)
			}
		}
	}
}

// cursorMessage returns the current pointer shape as a "cursor" message
func (x *xconn) cursorMessage() []byte {
	r, err := xfixes.GetCursorImage(x.c).Reply()
	if err != nil || r.Width == 0 || r.Height == 0 {
		return nil
	}
	img := image.NewNRGBA(image.Rect(0, 0, int(r.Width), int(r.Height)))
	for i, p := range r.CursorImage { // premultiplied ARGB
		a := p >> 24
		if a == 0 {
			continue
		}
		img.Pix[i*4] = byte((p >> 16 & 0xff) * 255 / a)
		img.Pix[i*4+1] = byte((p >> 8 & 0xff) * 255 / a)
		img.Pix[i*4+2] = byte((p & 0xff) * 255 / a)
		img.Pix[i*4+3] = byte(a)
	}
	var buf bytes.Buffer
	if png.Encode(&buf, img) != nil {
		return nil
	}
	msg, _ := json.Marshal(map[string]any{
		"type": "cursor",
		"png":  base64.StdEncoding.EncodeToString(buf.Bytes()),
		"x":    r.Xhot,
		"y":    r.Yhot,
	})
	return msg
}

func (x *xconn) move(px, py int) {
	xtest.FakeInput(x.c, xproto.MotionNotify, 0, 0, x.root, int16(px), int16(py), 0)
}

func (x *xconn) button(b int, down bool) {
	if b < 1 || b > 3 {
		return
	}
	x.mu.Lock()
	x.buttons[byte(b)] = down
	x.mu.Unlock()
	x.fake(xproto.ButtonPress, xproto.ButtonRelease, byte(b), down)
}

func (x *xconn) wheel(dx, dy int) {
	click := func(b byte) {
		x.fake(xproto.ButtonPress, xproto.ButtonRelease, b, true)
		x.fake(xproto.ButtonPress, xproto.ButtonRelease, b, false)
	}
	switch {
	case dy < 0:
		click(4)
	case dy > 0:
		click(5)
	}
	switch {
	case dx < 0:
		click(6)
	case dx > 0:
		click(7)
	}
}

func (x *xconn) key(code string, down bool) {
	kc, ok := keycodes[code]
	if !ok {
		return
	}
	x.mu.Lock()
	x.keys[kc] = down
	x.mu.Unlock()
	x.fake(xproto.KeyPress, xproto.KeyRelease, kc, down)
}

// releaseAll lets go of everything a viewer left pressed (tab closed mid-drag)
func (x *xconn) releaseAll() {
	x.mu.Lock()
	defer x.mu.Unlock()
	for k, down := range x.keys {
		if down {
			x.fake(xproto.KeyPress, xproto.KeyRelease, k, false)
		}
	}
	for b, down := range x.buttons {
		if down {
			x.fake(xproto.ButtonPress, xproto.ButtonRelease, b, false)
		}
	}
	clear(x.keys)
	clear(x.buttons)
}

func (x *xconn) fake(press, release byte, detail byte, down bool) {
	typ := release
	if down {
		typ = press
	}
	xtest.FakeInput(x.c, typ, detail, 0, x.root, 0, 0, 0)
}

// keycodes maps KeyboardEvent.code (a physical key) to the X keycode of the
// same key in Xvfb's default evdev keymap (Linux key code + 8). The layout
// is applied on the X side, so modifiers and shifted characters just work.
var keycodes = func() map[string]byte {
	m := map[string]byte{
		"Escape": 1, "Minus": 12, "Equal": 13, "Backspace": 14, "Tab": 15,
		"BracketLeft": 26, "BracketRight": 27, "Enter": 28, "ControlLeft": 29,
		"Semicolon": 39, "Quote": 40, "Backquote": 41, "ShiftLeft": 42,
		"Backslash": 43, "Comma": 51, "Period": 52, "Slash": 53, "ShiftRight": 54,
		"NumpadMultiply": 55, "AltLeft": 56, "Space": 57, "CapsLock": 58,
		"NumLock": 69, "ScrollLock": 70, "Numpad7": 71, "Numpad8": 72,
		"Numpad9": 73, "NumpadSubtract": 74, "Numpad4": 75, "Numpad5": 76,
		"Numpad6": 77, "NumpadAdd": 78, "Numpad1": 79, "Numpad2": 80,
		"Numpad3": 81, "Numpad0": 82, "NumpadDecimal": 83, "IntlBackslash": 86,
		"F11": 87, "F12": 88, "NumpadEnter": 96, "ControlRight": 97,
		"NumpadDivide": 98, "PrintScreen": 99, "AltRight": 100, "Home": 102,
		"ArrowUp": 103, "PageUp": 104, "ArrowLeft": 105, "ArrowRight": 106,
		"End": 107, "ArrowDown": 108, "PageDown": 109, "Insert": 110,
		"Delete": 111, "Pause": 119, "MetaLeft": 125, "MetaRight": 126,
		"ContextMenu": 127,
	}
	for i, c := range "1234567890" {
		m["Digit"+string(c)] = byte(2 + i)
	}
	for _, row := range []struct {
		keys  string
		first byte
	}{{"QWERTYUIOP", 16}, {"ASDFGHJKL", 30}, {"ZXCVBNM", 44}} {
		for i, c := range row.keys {
			m["Key"+string(c)] = row.first + byte(i)
		}
	}
	for i := 1; i <= 10; i++ {
		m["F"+strconv.Itoa(i)] = byte(58 + i)
	}
	for k, v := range m {
		m[k] = v + 8
	}
	return m
}()
