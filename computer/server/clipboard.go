package main

import (
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

// maxClipboard caps clipboard text in either direction. It also keeps a
// whole transfer inside one X request (no INCR protocol needed).
const maxClipboard = 192 << 10

// clipboard bridges the X CLIPBOARD selection and the viewers' clipboards.
// Text copied inside the desktop is reported through onChange; text from a
// viewer is offered to X apps by owning CLIPBOARD and answering their
// requests for it.
type clipboard struct {
	c        *xgb.Conn
	win      xproto.Window // invisible window that owns the selection
	onChange func(text string)

	atoms struct{ clipboard, utf8, targets, text, prop xproto.Atom }

	mu      sync.Mutex
	text    string // what we serve while we own CLIPBOARD
	owned   bool
	waiters []chan string // agents waiting for get()
}

func newClipboard(c *xgb.Conn, root xproto.Window, onChange func(string)) (*clipboard, error) {
	cb := &clipboard{c: c, onChange: onChange}
	for _, a := range []struct {
		name string
		dst  *xproto.Atom
	}{
		{"CLIPBOARD", &cb.atoms.clipboard}, {"UTF8_STRING", &cb.atoms.utf8},
		{"TARGETS", &cb.atoms.targets}, {"TEXT", &cb.atoms.text}, {"DESKTOPD_CLIPBOARD", &cb.atoms.prop},
	} {
		r, err := xproto.InternAtom(c, false, uint16(len(a.name)), a.name).Reply()
		if err != nil {
			return nil, err
		}
		*a.dst = r.Atom
	}
	var err error
	if cb.win, err = xproto.NewWindowId(c); err != nil {
		return nil, err
	}
	xproto.CreateWindow(c, 0, cb.win, root, -1, -1, 1, 1, 0, xproto.WindowClassInputOnly, 0, 0, nil)
	xfixes.SelectSelectionInput(c, cb.win, cb.atoms.clipboard, xfixes.SelectionEventMaskSetSelectionOwner)
	return cb, nil
}

// set makes text the desktop's clipboard
func (cb *clipboard) set(text string) {
	if len(text) > maxClipboard || !utf8.ValidString(text) {
		return
	}
	cb.mu.Lock()
	cb.text, cb.owned = text, true
	cb.mu.Unlock()
	xproto.SetSelectionOwner(cb.c, cb.win, cb.atoms.clipboard, xproto.TimeCurrentTime)
}

// get returns the desktop's clipboard text, asking its owner for it
func (cb *clipboard) get(timeout time.Duration) (string, error) {
	cb.mu.Lock()
	if cb.owned {
		defer cb.mu.Unlock()
		return cb.text, nil
	}
	ch := make(chan string, 1)
	cb.waiters = append(cb.waiters, ch)
	cb.mu.Unlock()
	xproto.ConvertSelection(cb.c, cb.win, cb.atoms.clipboard, cb.atoms.utf8, cb.atoms.prop, xproto.TimeCurrentTime)
	select {
	case text := <-ch:
		return text, nil
	case <-time.After(timeout):
		return "", errors.New("the clipboard owner did not answer")
	}
}

// deliver hands fetched text (or "" if there is none) to waiting get()s
func (cb *clipboard) deliver(text string) {
	cb.mu.Lock()
	ws := cb.waiters
	cb.waiters = nil
	cb.mu.Unlock()
	for _, ch := range ws {
		ch <- text
	}
}

// handle processes X events; it reports whether ev was a clipboard event
func (cb *clipboard) handle(ev xgb.Event) bool {
	switch e := ev.(type) {
	case xfixes.SelectionNotifyEvent: // someone copied: fetch the text
		if e.Selection == cb.atoms.clipboard && e.Owner != cb.win && e.Owner != 0 {
			xproto.ConvertSelection(cb.c, cb.win, cb.atoms.clipboard, cb.atoms.utf8, cb.atoms.prop, xproto.TimeCurrentTime)
		}
	case xproto.SelectionNotifyEvent: // the text arrived (or the owner refused)
		if e.Requestor != cb.win {
			return true
		}
		if e.Property == xproto.AtomNone {
			cb.deliver("")
			return true
		}
		r, err := xproto.GetProperty(cb.c, true, cb.win, cb.atoms.prop, xproto.GetPropertyTypeAny, 0, maxClipboard/4).Reply()
		if err != nil || r.Format != 8 || r.BytesAfter > 0 || !utf8.Valid(r.Value) {
			cb.deliver("")
			return true // too large (or INCR) or not text
		}
		cb.deliver(string(r.Value))
		cb.onChange(string(r.Value))
	case xproto.SelectionRequestEvent: // an app is pasting what we own
		cb.answer(e)
	case xproto.SelectionClearEvent: // someone else copied; they own it now
		cb.mu.Lock()
		cb.text, cb.owned = "", false
		cb.mu.Unlock()
	default:
		return false
	}
	return true
}

func (cb *clipboard) answer(e xproto.SelectionRequestEvent) {
	prop := e.Property
	if prop == xproto.AtomNone { // obsolete clients
		prop = e.Target
	}
	cb.mu.Lock()
	text := cb.text
	cb.mu.Unlock()

	switch e.Target {
	case cb.atoms.targets:
		targets := []xproto.Atom{cb.atoms.targets, cb.atoms.utf8, cb.atoms.text, xproto.AtomString}
		data := make([]byte, 0, 4*len(targets))
		for _, a := range targets {
			data = append(data, byte(a), byte(a>>8), byte(a>>16), byte(a>>24))
		}
		xproto.ChangeProperty(cb.c, xproto.PropModeReplace, e.Requestor, prop, xproto.AtomAtom, 32, uint32(len(targets)), data)
	case cb.atoms.utf8, cb.atoms.text, xproto.AtomString:
		typ := cb.atoms.utf8
		if e.Target == xproto.AtomString {
			typ = xproto.AtomString
		}
		xproto.ChangeProperty(cb.c, xproto.PropModeReplace, e.Requestor, prop, typ, 8, uint32(len(text)), []byte(text))
	default:
		prop = xproto.AtomNone // unsupported format
	}
	notify := xproto.SelectionNotifyEvent{
		Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target, Property: prop,
	}
	xproto.SendEvent(cb.c, false, e.Requestor, 0, string(notify.Bytes()))
}
