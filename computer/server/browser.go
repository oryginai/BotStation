package main

// Browser part of the agent API: drives Helium over the Chrome DevTools
// Protocol (CDP). helium.sh opens the debugging port on 127.0.0.1 inside
// the container (BROWSER_AUTOMATION, on by default); nothing outside the
// container can reach it.
//
// It gives agents the page's structure: tabs, interactive elements with
// roles, names and boxes on screen, navigation, waits and dialogs. Clicks
// and typing still go through the real mouse and keyboard (XTEST), at the
// element's position on screen, so pages get ordinary input events.
//
//	GET  /agent/browser/tabs
//	POST /agent/browser/tabs       {"action": new|activate|close, "id"?, "url"?}
//	GET  /agent/browser/snapshot   ?tab=&all=false&text=0
//	POST /agent/browser/navigate   {"url" | "back" | "forward" | "reload", "tab"?}
//	POST /agent/browser/click      {"ref", "button"?, "clicks"?, "tab"?}
//	POST /agent/browser/fill       {"ref", "text", "clear"?, "submit"?, "tab"?}
//	POST /agent/browser/wait       {"until": load|idle|text, "text"?, "timeout_ms"?, "tab"?}
//	POST /agent/browser/dialog     {"accept", "text"?, "tab"?}

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const cdpAddr = "127.0.0.1:9222" // helium.sh: --remote-debugging-port=9222

// The browser's launcher (helium/helium.sh) and its windows'
// WM_CLASS class
const (
	browserCmd   = "helium"
	browserClass = "Helium"
)

type browser struct {
	addr string

	dialMu sync.Mutex // one connection attempt at a time

	mu      sync.Mutex // guards everything below
	ws      *websocket.Conn
	nextID  int
	pending map[int]chan cdpMsg
	pages   map[string]*page // attached tabs, by target id
	subs    map[*sub]struct{}
}

type cdpMsg struct {
	ID        int             `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// sub receives the params of one event from one tab's session
type sub struct {
	session, method string
	ch              chan json.RawMessage
}

// page is an attached tab. Its fields are guarded by browser.mu.
type page struct {
	target, session string
	ready           bool               // Page and Network events on
	dialog          *dialog            // an alert/confirm/prompt waiting for an answer
	requests        map[string]request // network requests in flight, by id
	netChange       time.Time          // last request start or end
}

type request struct {
	at     time.Time
	loader string // the document that made it
}

type dialog struct {
	Type          string `json:"type"` // alert, confirm, prompt, beforeunload
	Message       string `json:"message"`
	DefaultPrompt string `json:"default_prompt,omitempty"`
}

var (
	errNoBrowser = errors.New("browser not running (POST /agent/launch {\"app\":\"browser\"}; " +
		"/agent/browser/navigate with a url and new tabs start it too)")
	errNoCDP = errors.New("the browser is running without automation (started while BROWSER_AUTOMATION " +
		"was off?): close it and start it again")
	errStale = errors.New("the page changed and that element is gone: take a new snapshot")
)

type dialogError struct{ d *dialog }

func (e dialogError) Error() string {
	return fmt.Sprintf("a %s dialog is open (%q): answer it with POST /agent/browser/dialog", e.d.Type, e.d.Message)
}

func newBrowser(addr string) *browser {
	return &browser{addr: addr, pending: map[int]chan cdpMsg{}, pages: map[string]*page{}, subs: map[*sub]struct{}{}}
}

// --- connection ---

// conn connects to the browser if needed
func (b *browser) conn(ctx context.Context) error {
	b.mu.Lock()
	ok := b.ws != nil
	b.mu.Unlock()
	if ok {
		return nil
	}
	b.dialMu.Lock()
	defer b.dialMu.Unlock()
	b.mu.Lock()
	ok = b.ws != nil
	b.mu.Unlock()
	if ok {
		return nil
	}

	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(dctx, http.MethodGet, "http://"+b.addr+"/json/version", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return errNoBrowser
	}
	var v struct {
		URL string `json:"webSocketDebuggerUrl"`
	}
	json.NewDecoder(res.Body).Decode(&v)
	res.Body.Close()
	if v.URL == "" {
		return errNoBrowser
	}
	ws, _, err := websocket.Dial(dctx, v.URL, nil)
	if err != nil {
		return errNoBrowser
	}
	ws.SetReadLimit(64 << 20)

	b.mu.Lock()
	b.ws = ws
	b.pages = map[string]*page{}
	b.mu.Unlock()
	go b.read(ws)

	// Get told when tabs close
	if _, err := b.call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}); err != nil {
		b.drop(ws)
		return err
	}
	log.Print("browser: connected (CDP)")
	return nil
}

func (b *browser) read(ws *websocket.Conn) {
	for {
		_, data, err := ws.Read(context.Background())
		if err != nil {
			b.drop(ws)
			return
		}
		var m cdpMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.ID != 0 {
			b.mu.Lock()
			ch := b.pending[m.ID]
			delete(b.pending, m.ID)
			b.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		b.event(m)
	}
}

// event keeps page state (dialogs, network) and feeds subscribers
func (b *browser) event(m cdpMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pg := b.bySession(m.SessionID)
	switch m.Method {
	case "Target.targetDestroyed":
		var p struct {
			TargetID string `json:"targetId"`
		}
		json.Unmarshal(m.Params, &p)
		delete(b.pages, p.TargetID)
	case "Target.detachedFromTarget":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		json.Unmarshal(m.Params, &p)
		if dp := b.bySession(p.SessionID); dp != nil {
			delete(b.pages, dp.target)
		}
	case "Page.javascriptDialogOpening":
		if pg != nil {
			var p struct {
				Type          string `json:"type"`
				Message       string `json:"message"`
				DefaultPrompt string `json:"defaultPrompt"`
			}
			json.Unmarshal(m.Params, &p)
			pg.dialog = &dialog{Type: p.Type, Message: p.Message, DefaultPrompt: p.DefaultPrompt}
		}
	case "Page.javascriptDialogClosed":
		if pg != nil {
			pg.dialog = nil
		}
	case "Network.requestWillBeSent", "Network.loadingFinished", "Network.loadingFailed":
		if pg != nil {
			var p struct {
				RequestID string `json:"requestId"`
				LoaderID  string `json:"loaderId"`
			}
			json.Unmarshal(m.Params, &p)
			if m.Method == "Network.requestWillBeSent" {
				pg.requests[p.RequestID] = request{at: time.Now(), loader: p.LoaderID}
			} else {
				delete(pg.requests, p.RequestID)
			}
			pg.netChange = time.Now()
		}
	case "Page.frameStartedLoading":
		if pg != nil {
			pg.netChange = time.Now()
		}
	case "Page.frameNavigated":
		// A new document: requests the old one left open are never reported
		// as finished (e.g. fetches cut off by the navigation)
		var p struct {
			Frame struct {
				ParentID string `json:"parentId"`
				LoaderID string `json:"loaderId"`
			} `json:"frame"`
		}
		json.Unmarshal(m.Params, &p)
		if pg != nil && p.Frame.ParentID == "" {
			for id, r := range pg.requests {
				if r.loader != p.Frame.LoaderID {
					delete(pg.requests, id)
				}
			}
		}
	}
	for s := range b.subs {
		if s.method == m.Method && s.session == m.SessionID {
			select {
			case s.ch <- m.Params:
			default:
			}
		}
	}
}

func (b *browser) bySession(id string) *page {
	if id == "" {
		return nil
	}
	for _, pg := range b.pages {
		if pg.session == id {
			return pg
		}
	}
	return nil
}

func (b *browser) drop(ws *websocket.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ws != ws {
		return
	}
	ws.CloseNow()
	b.ws = nil
	b.pages = map[string]*page{}
	for id, ch := range b.pending {
		ch <- cdpMsg{Error: &cdpError{Message: "browser closed"}}
		delete(b.pending, id)
	}
}

// call sends one CDP command to a tab's session ("" = the browser itself)
func (b *browser) call(ctx context.Context, session, method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	ws := b.ws
	if ws == nil {
		b.mu.Unlock()
		return nil, errNoBrowser
	}
	b.nextID++
	id := b.nextID
	ch := make(chan cdpMsg, 1)
	b.pending[id] = ch
	b.mu.Unlock()

	if params == nil {
		params = struct{}{}
	}
	p, _ := json.Marshal(params)
	msg, _ := json.Marshal(cdpMsg{ID: id, Method: method, SessionID: session, Params: p})
	if err := ws.Write(ctx, websocket.MessageText, msg); err != nil {
		b.drop(ws)
		return nil, errNoBrowser
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, r.Error.Message)
		}
		return r.Result, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// subscribe returns a channel of one event's params from one session, and
// a function to stop
func (b *browser) subscribe(session, method string) (chan json.RawMessage, func()) {
	s := &sub{session: session, method: method, ch: make(chan json.RawMessage, 16)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s.ch, func() {
		b.mu.Lock()
		delete(b.subs, s)
		b.mu.Unlock()
	}
}

// --- tabs ---

type tab struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Active bool   `json:"active"`
}

// attach returns the tab's session, attaching to it if needed
func (b *browser) attach(ctx context.Context, targetID string) (*page, error) {
	b.mu.Lock()
	pg := b.pages[targetID]
	b.mu.Unlock()
	if pg != nil {
		return pg, nil
	}
	res, err := b.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true})
	if err != nil {
		return nil, err
	}
	var r struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(res, &r)
	b.mu.Lock()
	defer b.mu.Unlock()
	if old := b.pages[targetID]; old != nil { // attached twice at once: keep the first
		go b.call(context.Background(), "", "Target.detachFromTarget", map[string]any{"sessionId": r.SessionID})
		return old, nil
	}
	pg = &page{target: targetID, session: r.SessionID, requests: map[string]request{}, netChange: time.Now()}
	b.pages[targetID] = pg
	return pg, nil
}

// ready attaches and turns on the events this API relies on: loads,
// dialogs, network activity
func (b *browser) ready(ctx context.Context, targetID string) (*page, error) {
	pg, err := b.attach(ctx, targetID)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	done := pg.ready
	b.mu.Unlock()
	if done {
		return pg, nil
	}
	for _, c := range []struct {
		method string
		params any
	}{
		{"Page.enable", nil},
		{"Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}},
		{"Network.enable", map[string]any{"maxTotalBufferSize": 1 << 20, "maxResourceBufferSize": 256 << 10}},
	} {
		if _, err := b.call(ctx, pg.session, c.method, c.params); err != nil {
			return nil, err
		}
	}
	b.mu.Lock()
	pg.ready = true
	b.mu.Unlock()
	return pg, nil
}

func (b *browser) dialogOf(pg *page) *dialog {
	b.mu.Lock()
	defer b.mu.Unlock()
	return pg.dialog
}

// targets lists the open tabs, in a stable order
func (b *browser) targets(ctx context.Context) ([]tab, error) {
	if err := b.conn(ctx); err != nil {
		return nil, err
	}
	res, err := b.call(ctx, "", "Target.getTargets", nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			Title    string `json:"title"`
			URL      string `json:"url"`
		} `json:"targetInfos"`
	}
	json.Unmarshal(res, &r)
	out := []tab{}
	for _, t := range r.TargetInfos {
		if t.Type == "page" && !strings.HasPrefix(t.URL, "devtools://") {
			out = append(out, tab{ID: t.TargetID, URL: t.URL, Title: t.Title})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// tabs lists the open tabs and marks the active one: the tab shown in the
// most recently focused browser window. windowTitles are the browser window
// titles, the active window first. A title names its window's visible tab;
// when that is ambiguous the pages are asked whether they are visible.
func (b *browser) tabs(ctx context.Context, windowTitles []string) ([]tab, error) {
	out, err := b.targets(ctx)
	if err != nil {
		return nil, err
	}
	active := -1
	if len(windowTitles) > 0 { // only the frontmost window counts
		for i, t := range out {
			if t.Title != "" && (windowTitles[0] == t.Title || strings.HasPrefix(windowTitles[0], t.Title+" - ")) {
				if active == -1 {
					active = i
				} else {
					active = -2 // two tabs with the same title
				}
			}
		}
	}
	if active < 0 {
		best := 0
		for i, t := range out {
			var st struct{ Visible, Focus bool }
			pctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
			pg, err := b.attach(pctx, t.ID)
			if err == nil {
				err = b.eval(pctx, pg, `({Visible: document.visibilityState === 'visible', Focus: document.hasFocus()})`, &st)
			}
			cancel()
			if err != nil {
				continue
			}
			score := 0
			if st.Visible {
				score = 1
			}
			if st.Visible && st.Focus {
				score = 2
			}
			if score > best {
				active, best = i, score
			}
		}
	}
	if active >= 0 {
		out[active].Active = true
	}
	return out, nil
}

func (b *browser) tabInfo(ctx context.Context, id string) (tab, error) {
	res, err := b.call(ctx, "", "Target.getTargetInfo", map[string]any{"targetId": id})
	if err != nil {
		return tab{}, err
	}
	var r struct {
		TargetInfo struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"targetInfo"`
	}
	json.Unmarshal(res, &r)
	return tab{ID: id, URL: r.TargetInfo.URL, Title: r.TargetInfo.Title}, nil
}

// --- scripts ---

// world returns the tab's isolated script world (same DOM, separate
// globals: the page can't see or change what lives there). Chromium keeps
// one world per name and document, so element refs stored in it last
// until the page navigates.
func (b *browser) world(ctx context.Context, pg *page) (int, error) {
	res, err := b.call(ctx, pg.session, "Page.getFrameTree", nil)
	if err != nil {
		return 0, err
	}
	var ft struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	json.Unmarshal(res, &ft)
	res, err = b.call(ctx, pg.session, "Page.createIsolatedWorld", map[string]any{
		"frameId": ft.FrameTree.Frame.ID, "worldName": "bot-station",
	})
	if err != nil {
		return 0, err
	}
	var w struct {
		ID int `json:"executionContextId"`
	}
	json.Unmarshal(res, &w)
	return w.ID, nil
}

// eval runs JavaScript in the tab's isolated world and decodes its
// (JSON-able) result into out
func (b *browser) eval(ctx context.Context, pg *page, expr string, out any) error {
	if d := b.dialogOf(pg); d != nil {
		return dialogError{d}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cid, err := b.world(ctx, pg)
	if err == nil {
		var res json.RawMessage
		res, err = b.call(ctx, pg.session, "Runtime.evaluate", map[string]any{
			"expression": expr, "contextId": cid, "returnByValue": true, "awaitPromise": true,
		})
		if err == nil {
			return decodeRemote(res, out)
		}
	}
	if d := b.dialogOf(pg); d != nil { // scripts stop while a dialog is open
		return dialogError{d}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("the page is not responding")
	}
	return err
}

func decodeRemote(res json.RawMessage, out any) error {
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return err
	}
	if r.ExceptionDetails != nil {
		msg := r.ExceptionDetails.Exception.Description
		if msg == "" {
			msg = r.ExceptionDetails.Text
		}
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return fmt.Errorf("page script: %s", msg)
	}
	if out == nil || len(r.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(r.Result.Value, out)
}

// call a page script function with JSON arguments
func jsCall(fn string, args ...any) string {
	parts := make([]string, len(args))
	for i, a := range args {
		data, _ := json.Marshal(a)
		parts[i] = string(data)
	}
	return "(" + fn + ")(" + strings.Join(parts, ", ") + ")"
}

// --- navigation ---

// navigate loads url in the tab and waits for the load event
func (b *browser) navigate(ctx context.Context, pg *page, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	loads, stop := b.subscribe(pg.session, "Page.lifecycleEvent")
	defer stop()
	res, err := b.call(ctx, pg.session, "Page.navigate", map[string]any{"url": url})
	if err != nil {
		return err
	}
	var r struct {
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	json.Unmarshal(res, &r)
	if r.ErrorText != "" {
		return fmt.Errorf("could not load %s: %s", url, r.ErrorText)
	}
	if r.LoaderID == "" {
		return nil // same document (#fragment)
	}
	return b.waitLoad(ctx, pg, loads, r.LoaderID)
}

// waitLoad waits for a load lifecycle event (of loaderID, or any)
func (b *browser) waitLoad(ctx context.Context, pg *page, events chan json.RawMessage, loaderID string) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case p := <-events:
			var e struct {
				Name     string `json:"name"`
				LoaderID string `json:"loaderId"`
			}
			json.Unmarshal(p, &e)
			if e.Name == "load" && (loaderID == "" || e.LoaderID == loaderID) {
				return nil
			}
		case <-tick.C:
			if d := b.dialogOf(pg); d != nil { // e.g. "leave this page?"
				return dialogError{d}
			}
		case <-ctx.Done():
			return errors.New("the page did not finish loading within 30 s")
		}
	}
}

func (b *browser) reload(ctx context.Context, pg *page) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	loads, stop := b.subscribe(pg.session, "Page.lifecycleEvent")
	defer stop()
	if _, err := b.call(ctx, pg.session, "Page.reload", nil); err != nil {
		return err
	}
	return b.waitLoad(ctx, pg, loads, "")
}

// history goes back (-1) or forward (+1) and waits until the other page
// is shown (not until it has loaded)
func (b *browser) history(ctx context.Context, pg *page, delta int) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	navs, stop := b.subscribe(pg.session, "Page.frameNavigated")
	defer stop()
	inDoc, stop2 := b.subscribe(pg.session, "Page.navigatedWithinDocument")
	defer stop2()
	res, err := b.call(ctx, pg.session, "Page.getNavigationHistory", nil)
	if err != nil {
		return err
	}
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	json.Unmarshal(res, &h)
	i := h.CurrentIndex + delta
	if i < 0 || i >= len(h.Entries) {
		if delta < 0 {
			return errors.New("no page to go back to")
		}
		return errors.New("no page to go forward to")
	}
	if _, err = b.call(ctx, pg.session, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[i].ID}); err != nil {
		return err
	}
	for {
		select {
		case p := <-navs:
			var e struct {
				Frame struct {
					ParentID string `json:"parentId"`
				} `json:"frame"`
			}
			json.Unmarshal(p, &e)
			if e.Frame.ParentID == "" {
				return nil
			}
		case <-inDoc:
			return nil
		case <-ctx.Done():
			return errors.New("the page did not change within 30 s")
		}
	}
}

// normalizeURL adds https:// to a bare host and allows only web pages
func normalizeURL(u string) (string, error) {
	u = strings.TrimSpace(u)
	if u == "about:blank" {
		return u, nil
	}
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "", errors.New("url must be http(s) or about:blank")
	}
	return u, nil
}

// --- snapshot ---

// element is one interactive thing on the page. ref stays valid for the
// same element until the page navigates. x/y/width/height are screen
// pixels, the same space as screenshots and /agent/actions.
type element struct {
	Ref       int    `json:"ref"`
	Role      string `json:"role"`
	Name      string `json:"name,omitempty"`
	Value     string `json:"value,omitempty"`
	Href      string `json:"href,omitempty"`
	Checked   *bool  `json:"checked,omitempty"`
	Expanded  *bool  `json:"expanded,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	Focused   bool   `json:"focused,omitempty"`
	Offscreen bool   `json:"offscreen,omitempty"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type box struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type snapshot struct {
	Tab      string `json:"tab"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Viewport box    `json:"viewport"` // where the page is drawn on screen
	Scroll   struct {
		Y      int `json:"y"`
		Height int `json:"height"`
	} `json:"scroll"` // scroll position and page height, CSS px
	Dialog *dialog `json:"dialog,omitempty"`
	// A human check on the page that isn't solved yet: cloudflare,
	// recaptcha or hcaptcha. The agent hands it to the person in the viewer.
	Challenge string    `json:"challenge,omitempty"`
	Elements  []element `json:"elements"`
	Text      string    `json:"text,omitempty"`
}

func (b *browser) snapshot(ctx context.Context, t tab, all bool, maxText int) (*snapshot, error) {
	pg, err := b.ready(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if d := b.dialogOf(pg); d != nil {
		return &snapshot{Tab: t.ID, URL: t.URL, Title: t.Title, Dialog: d, Elements: []element{}}, nil
	}
	var s snapshot
	opts := map[string]any{"all": all, "maxText": maxText, "win": b.windowBounds(ctx, pg)}
	if err := b.eval(ctx, pg, jsCall(snapshotJS, opts), &s); err != nil {
		var de dialogError
		if errors.As(err, &de) {
			return &snapshot{Tab: t.ID, URL: t.URL, Title: t.Title, Dialog: de.d, Elements: []element{}}, nil
		}
		return nil, err
	}
	s.Tab = t.ID
	return &s, nil
}

// target is an element scrolled into view: its box on screen
type target struct {
	Error   string `json:"error"`
	Tag     string `json:"tag"`
	Type    string `json:"type"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	W       int    `json:"w"`
	H       int    `json:"h"`
	Covered bool   `json:"covered"`
	Inside  bool   `json:"inside"` // the centre is inside the visible page
}

func (b *browser) target(ctx context.Context, pg *page, ref int) (*target, error) {
	var t target
	if err := b.eval(ctx, pg, jsCall(targetJS, ref, b.windowBounds(ctx, pg)), &t); err != nil {
		return nil, err
	}
	if t.Error == "stale" {
		return nil, errStale
	}
	if t.Error != "" {
		return nil, errors.New(t.Error)
	}
	return &t, nil
}

// windowBounds is the box on screen of the window showing pg, as the
// browser process knows it, or nil (the page scripts then use its
// screenX/outerWidth)
func (b *browser) windowBounds(ctx context.Context, pg *page) map[string]int {
	res, err := b.call(ctx, "", "Browser.getWindowForTarget", map[string]any{"targetId": pg.target})
	if err != nil {
		return nil
	}
	var r struct {
		Bounds struct {
			Left, Top, Width, Height *int
		} `json:"bounds"`
	}
	if json.Unmarshal(res, &r) != nil || r.Bounds.Left == nil || r.Bounds.Top == nil ||
		r.Bounds.Width == nil || r.Bounds.Height == nil {
		return nil
	}
	return map[string]int{"left": *r.Bounds.Left, "top": *r.Bounds.Top, "width": *r.Bounds.Width, "height": *r.Bounds.Height}
}

// refCall runs one of the small element scripts below; they answer
// {error} or {}
func (b *browser) refCall(ctx context.Context, pg *page, fn string, args ...any) error {
	var r struct {
		Error string `json:"error"`
	}
	if err := b.eval(ctx, pg, jsCall(fn, args...), &r); err != nil {
		return err
	}
	if r.Error == "stale" {
		return errStale
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return nil
}

// The page scripts run in the isolated world. Refs live in a per-document
// map there, so a new page means new refs.
const refsJS = `(globalThis.__ac ||= {map: new Map(), of: new WeakMap(), next: 1})`

// toScreen: CSS px in the viewport -> screen px. The page area is at the
// bottom of the browser window (tabs and toolbar are above it), inset by
// the same margin on the sides and the bottom (Helium: 4 px round its
// rounded page; stock Chromium: 0). win is the window's box on screen from
// the browser process (windowBounds); the page's own screenX/outerWidth
// are only a fallback, as a browser that spoofs the screen makes them up.
// innerWidth is CSS px (page zoom = dpr).
const screenJS = `
  const W = win || {left: screenX, top: screenY, width: outerWidth, height: outerHeight};
  const dpr = devicePixelRatio || 1, inset = (W.width - innerWidth * dpr) / 2;
  const ox = W.left + inset, oy = W.top + W.height - innerHeight * dpr - inset;
  const toScreen = (r) => ({x: Math.round(ox + r.left * dpr), y: Math.round(oy + r.top * dpr),
                            w: Math.round(r.width * dpr), h: Math.round(r.height * dpr)});
  const hit = (el, r) => {
    const cx = r.left + r.width / 2, cy = r.top + r.height / 2;
    if (cx < 0 || cy < 0 || cx >= innerWidth || cy >= innerHeight) return true;
    let h = document.elementFromPoint(cx, cy);
    while (h && h.shadowRoot) { const d = h.shadowRoot.elementFromPoint(cx, cy); if (!d || d === h) break; h = d; }
    if (!h) return false;
    if (h === el || el.contains(h) || h.contains(el)) return true;
    for (let n = h; n; n = n.parentNode || n.host) if (n === el) return true; // inside el's shadow tree
    return !!el.labels && [...el.labels].some((l) => l === h || l.contains(h)); // a label covering its input
  };`

var snapshotJS = `(opts) => {
  const R = ` + refsJS + `, win = opts.win;
  ` + screenJS + `
  const SEL = 'a[href],button,input:not([type=hidden]),select,textarea,summary,label[for],[contenteditable=""],[contenteditable=true],' +
    '[role=button],[role=link],[role=checkbox],[role=radio],[role=switch],[role=tab],[role=menuitem],[role=menuitemcheckbox],' +
    '[role=menuitemradio],[role=option],[role=textbox],[role=searchbox],[role=combobox],[role=slider],[role=spinbutton],' +
    '[role=treeitem],[onclick],[tabindex]:not([tabindex="-1"])';
  const clean = (s, n = 120) => { s = (s || '').replace(/\s+/g, ' ').trim(); return s.length > n ? s.slice(0, n - 1) + '…' : s; };
  const found = [];
  const walk = (root) => {
    root.querySelectorAll(SEL).forEach((el) => found.push(el));
    root.querySelectorAll('*').forEach((el) => el.shadowRoot && walk(el.shadowRoot));
  };
  walk(document);
  const roleOf = (el) => {
    const r = el.getAttribute('role');
    if (r) return r.split(/\s+/)[0];
    const t = el.tagName.toLowerCase(), type = (el.getAttribute('type') || 'text').toLowerCase();
    if (t === 'a') return 'link';
    if (t === 'button' || t === 'summary') return 'button';
    if (t === 'select') return el.multiple ? 'listbox' : 'combobox';
    if (t === 'textarea') return 'textbox';
    if (t === 'label') return 'label';
    if (t === 'input') return ({checkbox: 'checkbox', radio: 'radio', submit: 'button', button: 'button', reset: 'button',
      image: 'button', range: 'slider', search: 'searchbox', file: 'file', number: 'spinbutton'})[type] || 'textbox';
    if (el.isContentEditable) return 'textbox';
    return 'clickable';
  };
  const nameOf = (el) => {
    let n = el.getAttribute('aria-label');
    const by = el.getAttribute('aria-labelledby');
    if (!n && by) n = by.split(/\s+/).map((id) => document.getElementById(id)?.innerText || '').join(' ');
    if (!n && el.labels && el.labels.length) n = [...el.labels].map((l) => l.innerText).join(' ');
    if (!n && !['INPUT', 'SELECT', 'TEXTAREA'].includes(el.tagName)) n = el.innerText;
    if (!n && el.tagName === 'INPUT' && ['submit', 'button', 'reset'].includes(el.type)) n = el.value;
    n = n || el.getAttribute('title') || el.getAttribute('placeholder') || el.getAttribute('alt') ||
      el.querySelector?.('img[alt]')?.alt || el.querySelector?.('svg title')?.textContent || el.getAttribute('name') || '';
    return clean(n);
  };
  const seen = new Set(), out = [];
  for (const el of found) {
    if (seen.has(el)) continue;
    seen.add(el);
    const r = el.getBoundingClientRect();
    if (r.width < 1 || r.height < 1) continue;
    const cs = getComputedStyle(el);
    if (cs.visibility === 'hidden' || cs.opacity === '0') continue;
    const offscreen = r.bottom <= 0 || r.right <= 0 || r.top >= innerHeight || r.left >= innerWidth;
    if (offscreen && !opts.all) continue;
    if (!offscreen && !hit(el, r)) continue; // covered by something else (a dialog, a banner)
    let ref = R.of.get(el);
    if (!ref) { ref = R.next++; R.of.set(el, ref); R.map.set(ref, new WeakRef(el)); }
    const s = toScreen(r), role = roleOf(el);
    const e = {ref, role, name: nameOf(el), x: s.x, y: s.y, width: s.w, height: s.h};
    if ('value' in el && ['textbox', 'searchbox', 'combobox', 'listbox', 'spinbutton', 'slider'].includes(role))
      e.value = el.type === 'password' ? (el.value ? '••••' : '') : clean(el.tagName === 'SELECT' ? el.selectedOptions[0]?.text : el.value, 200);
    else if (el.isContentEditable) e.value = clean(el.innerText, 200);
    if (role === 'link' && el.href) e.href = clean(el.href, 200);
    if (el.type === 'checkbox' || el.type === 'radio') e.checked = el.checked;
    else if (el.hasAttribute('aria-checked')) e.checked = el.getAttribute('aria-checked') === 'true';
    if (el.hasAttribute('aria-expanded')) e.expanded = el.getAttribute('aria-expanded') === 'true';
    else if (el.tagName === 'SUMMARY' && el.parentElement?.tagName === 'DETAILS') e.expanded = el.parentElement.open;
    if (el.disabled || el.getAttribute('aria-disabled') === 'true') e.disabled = true;
    let a = document.activeElement;
    while (a?.shadowRoot?.activeElement) a = a.shadowRoot.activeElement;
    if (a === el) e.focused = true;
    if (offscreen) e.offscreen = true;
    out.push(e);
  }
  // Human checks. Their frames are cross-origin (and Turnstile's sits in a
  // closed shadow root), so they're found by their page-side traces: the
  // interstitial's title and form, a visible widget, and the hidden
  // response field, which is filled once the check is solved.
  const shown = (el) => { const r = el.getBoundingClientRect(); return r.width > 1 && r.height > 1 && getComputedStyle(el).visibility !== 'hidden'; };
  const open = (sel) => [...document.querySelectorAll(sel)].some((el) => !el.value);
  const challenge =
    (/^(just a moment|attention required|un instant|einen moment|un momento)/i.test(document.title) && document.querySelector('script[src*="/cdn-cgi/challenge-platform/"], #challenge-form, #challenge-running')) ||
    ([...document.querySelectorAll('.cf-turnstile')].some(shown) && open('[name="cf-turnstile-response"]')) ? 'cloudflare' :
    [...document.querySelectorAll('iframe[src*="/recaptcha/"]')].some((f) => shown(f) && /\/(anchor|bframe)/.test(f.src)) && open('[name="g-recaptcha-response"]') ? 'recaptcha' :
    [...document.querySelectorAll('iframe[src*="hcaptcha.com"]')].some(shown) && open('[name="h-captcha-response"]') ? 'hcaptcha' : '';
  const vp = toScreen({left: 0, top: 0, width: innerWidth, height: innerHeight});
  const res = {url: location.href, title: document.title, challenge, elements: out,
    viewport: {x: vp.x, y: vp.y, width: vp.w, height: vp.h},
    scroll: {y: Math.round(scrollY), height: document.documentElement?.scrollHeight || 0}};
  if (opts.maxText > 0) res.text = (document.body?.innerText || '').slice(0, opts.maxText);
  return res;
}`

const elJS = `const el = (` + refsJS + `).map.get(ref)?.deref();
  if (!el || !el.isConnected) return {error: 'stale'};`

var targetJS = `async (ref, win) => {
  ` + elJS + `
  ` + screenJS + `
  const r0 = el.getBoundingClientRect();
  if (r0.top < 0 || r0.left < 0 || r0.bottom > innerHeight || r0.right > innerWidth) {
    el.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
    await new Promise((ok) => requestAnimationFrame(() => requestAnimationFrame(ok)));
  }
  const r = el.getBoundingClientRect(), s = toScreen(r);
  const cx = r.left + r.width / 2, cy = r.top + r.height / 2;
  return {tag: el.tagName.toLowerCase(), type: (el.type || '').toString().toLowerCase(), x: s.x, y: s.y, w: s.w, h: s.h,
    covered: !hit(el, r), inside: cx >= 0 && cy >= 0 && cx < innerWidth && cy < innerHeight};
}`

// focusJS makes sure typing goes into the element (a click on its label or
// padding may not focus it)
var focusJS = `(ref) => {
  ` + elJS + `
  let a = document.activeElement;
  while (a?.shadowRoot?.activeElement) a = a.shadowRoot.activeElement;
  if (a !== el && !el.contains(a)) el.focus();
  return {};
}`

var selectJS = `(ref, value) => {
  ` + elJS + `
  if (el.tagName !== 'SELECT') return {error: 'element ' + ref + ' is not a <select>'};
  const opt = [...el.options].find((o) => o.value === value) || [...el.options].find((o) => o.text.trim() === value.trim());
  if (!opt) return {error: 'no option ' + JSON.stringify(value) + ' (options: ' + [...el.options].map((o) => o.text.trim()).join(', ') + ')'};
  el.value = opt.value;
  el.dispatchEvent(new Event('input', {bubbles: true}));
  el.dispatchEvent(new Event('change', {bubbles: true}));
  return {};
}`

// --- HTTP ---

func (s *server) browserRoutes(h func(string, http.HandlerFunc)) {
	h("GET /agent/browser/tabs", s.handleBrowserTabs)
	h("POST /agent/browser/tabs", s.handleBrowserTabAction)
	h("GET /agent/browser/snapshot", s.handleBrowserSnapshot)
	h("POST /agent/browser/navigate", s.handleBrowserNavigate)
	h("POST /agent/browser/click", s.handleBrowserClick)
	h("POST /agent/browser/fill", s.handleBrowserFill)
	h("POST /agent/browser/wait", s.handleBrowserWait)
	h("POST /agent/browser/dialog", s.handleBrowserDialog)
}

// browserWindows returns the browser's windows, the active window first, then
// in stacking order as the window manager lists them
func (s *server) browserWindows() []window {
	var out []window
	for _, w := range s.x.windows() {
		if strings.EqualFold(w.App, browserClass) {
			out = append(out, w)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Active && !out[j].Active })
	return out
}

func (s *server) browserTabs(ctx context.Context) ([]tab, error) {
	var titles []string
	for _, w := range s.browserWindows() {
		if !w.Minimized {
			titles = append(titles, w.Title)
		}
	}
	tabs, err := s.br.tabs(ctx, titles)
	return tabs, s.browserErr(err)
}

// browserErr tells "not running" apart from "running without automation"
func (s *server) browserErr(err error) error {
	if err == errNoBrowser && len(s.browserWindows()) > 0 {
		return errNoCDP
	}
	return err
}

// pickTab returns the tab to work on: the given id, or the active tab
func (s *server) pickTab(ctx context.Context, id string) (tab, error) {
	tabs, err := s.browserTabs(ctx)
	if err != nil {
		return tab{}, err
	}
	for _, t := range tabs {
		if (id != "" && t.ID == id) || (id == "" && t.Active) {
			return t, nil
		}
	}
	if id != "" {
		return tab{}, fmt.Errorf("no tab %q", id)
	}
	if len(tabs) > 0 {
		return tabs[0], nil
	}
	return tab{}, errNoBrowser
}

// startBrowser starts the browser if it isn't running (opening url, if given)
// and waits until it can be driven. started reports whether it was started.
func (s *server) startBrowser(ctx context.Context, url string) (started bool, err error) {
	if s.br.conn(ctx) == nil {
		return false, nil
	}
	if len(s.browserWindows()) > 0 {
		return false, errNoCDP
	}
	args := []string{}
	if url != "" {
		args = append(args, url)
	}
	if err := start(exec.Command(browserCmd, args...)); err != nil {
		return false, err
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if tabs, err := s.br.targets(ctx); err == nil && len(tabs) > 0 {
			return true, nil
		}
	}
	return false, errors.New("the browser did not start within 20 s")
}

// front shows the tab and brings its window to the front, so real input
// reaches it
func (s *server) front(ctx context.Context, t tab) error {
	if _, err := s.br.call(ctx, "", "Target.activateTarget", map[string]any{"targetId": t.ID}); err != nil {
		return err
	}
	for try := 0; try < 10; try++ {
		wins := s.browserWindows()
		if len(wins) == 0 {
			return errNoBrowser
		}
		// The window showing this tab: its title starts with the tab's title
		win := wins[0]
		for _, w := range wins {
			if t.Title != "" && strings.HasPrefix(w.Title, t.Title) {
				win = w
				break
			}
		}
		if win.Active && !win.Minimized {
			if try > 0 {
				time.Sleep(100 * time.Millisecond) // let it redraw
			}
			return nil
		}
		if try%4 == 0 {
			s.x.windowAction(win.ID, "activate", 0, 0, 0, 0)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("could not bring the browser window to the front")
}

// locate shows the tab, scrolls the element into view and returns where it
// is on screen
func (s *server) locate(ctx context.Context, tabID string, ref int) (*page, *target, error) {
	if ref <= 0 {
		return nil, nil, errors.New("ref is required (from GET /agent/browser/snapshot)")
	}
	t, err := s.pickTab(ctx, tabID)
	if err != nil {
		return nil, nil, err
	}
	pg, err := s.br.ready(ctx, t.ID)
	if err != nil {
		return nil, nil, err
	}
	if d := s.br.dialogOf(pg); d != nil {
		return nil, nil, dialogError{d}
	}
	if err := s.front(ctx, t); err != nil {
		return nil, nil, err
	}
	tg, err := s.br.target(ctx, pg, ref)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case tg.W <= 0 || tg.H <= 0:
		return nil, nil, errors.New("the element is not visible (zero size)")
	case !tg.Inside:
		return nil, nil, errors.New("the element could not be scrolled into view")
	case tg.Covered:
		return nil, nil, errors.New("the element is covered by something else (a dialog or banner?): take a new snapshot")
	}
	return pg, tg, nil
}

func (s *server) clickAt(tg *target, button, clicks int) (int, int) {
	x, y := s.clamp(tg.X+tg.W/2, tg.Y+tg.H/2)
	s.x.glide(x, y)
	s.x.click(button, clicks)
	s.x.sync()
	settle(clickSettle)
	return x, y
}

// browserFail answers an error with the right status
func browserFail(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if err == errNoBrowser || err == errNoCDP {
		code = http.StatusServiceUnavailable
	}
	writeError(w, code, err)
}

func (s *server) handleBrowserTabs(w http.ResponseWriter, r *http.Request) {
	tabs, err := s.browserTabs(r.Context())
	if err != nil {
		browserFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tabs": tabs})
}

func (s *server) handleBrowserTabAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		URL    string `json:"url"`
	}
	if !readJSON(w, r, 8192, &req) {
		return
	}
	ctx := r.Context()
	switch req.Action {
	case "new":
		url := ""
		if req.URL != "" {
			var err error
			if url, err = normalizeURL(req.URL); err != nil {
				browserFail(w, err)
				return
			}
		}
		started, err := s.startBrowser(ctx, "")
		if err != nil {
			browserFail(w, err)
			return
		}
		var id string
		if started { // a freshly started browser's tab is the new one
			t, err := s.pickTab(ctx, "")
			if err != nil {
				browserFail(w, err)
				return
			}
			id = t.ID
		} else {
			res, err := s.br.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"})
			if err != nil {
				browserFail(w, err)
				return
			}
			var t struct {
				TargetID string `json:"targetId"`
			}
			json.Unmarshal(res, &t)
			id = t.TargetID
		}
		if url != "" { // load it like navigate does: wait for the page
			pg, err := s.br.ready(ctx, id)
			if err == nil {
				err = s.br.navigate(ctx, pg, url)
			}
			if err != nil {
				browserFail(w, err)
				return
			}
		}
		info, err := s.br.tabInfo(ctx, id)
		if err != nil {
			browserFail(w, err)
			return
		}
		info.Active = true
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "tab": info})
	case "activate", "close":
		if req.ID == "" {
			browserFail(w, errors.New("id is required"))
			return
		}
		t, err := s.pickTab(ctx, req.ID)
		if err != nil {
			browserFail(w, err)
			return
		}
		if req.Action == "activate" {
			err = s.front(ctx, t)
		} else {
			_, err = s.br.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": t.ID})
		}
		if err != nil {
			browserFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		browserFail(w, errors.New("action must be new, activate or close"))
	}
}

func (s *server) handleBrowserSnapshot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	maxText, _ := strconv.Atoi(q.Get("text"))
	maxText = min(max(maxText, 0), 200000)
	t, err := s.pickTab(r.Context(), q.Get("tab"))
	if err != nil {
		browserFail(w, err)
		return
	}
	snap, err := s.br.snapshot(r.Context(), t, q.Get("all") == "true" || q.Get("all") == "1", maxText)
	if err != nil {
		browserFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *server) handleBrowserNavigate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tab     string `json:"tab"`
		URL     string `json:"url"`
		Back    bool   `json:"back"`
		Forward bool   `json:"forward"`
		Reload  bool   `json:"reload"`
	}
	if !readJSON(w, r, 16<<10, &req) {
		return
	}
	n := 0
	for _, set := range []bool{req.URL != "", req.Back, req.Forward, req.Reload} {
		if set {
			n++
		}
	}
	if n != 1 {
		browserFail(w, errors.New("give exactly one of url, back, forward or reload"))
		return
	}
	ctx := r.Context()
	if req.URL != "" {
		url, err := normalizeURL(req.URL)
		if err != nil {
			browserFail(w, err)
			return
		}
		req.URL = url
		if req.Tab == "" {
			if _, err := s.startBrowser(ctx, ""); err != nil {
				browserFail(w, err)
				return
			}
		}
	}
	t, err := s.pickTab(ctx, req.Tab)
	if err != nil {
		browserFail(w, err)
		return
	}
	pg, err := s.br.ready(ctx, t.ID)
	if err == nil {
		if d := s.br.dialogOf(pg); d != nil && !req.Reload && req.URL == "" {
			err = dialogError{d}
		}
	}
	if err == nil {
		switch {
		case req.URL != "":
			err = s.br.navigate(ctx, pg, req.URL)
		case req.Reload:
			err = s.br.reload(ctx, pg)
		case req.Back:
			err = s.br.history(ctx, pg, -1)
		default:
			err = s.br.history(ctx, pg, 1)
		}
	}
	if err != nil {
		browserFail(w, err)
		return
	}
	info, err := s.br.tabInfo(ctx, t.ID)
	if err != nil {
		browserFail(w, err)
		return
	}
	info.Active = t.Active
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tab": info})
}

func (s *server) handleBrowserClick(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tab    string `json:"tab"`
		Ref    int    `json:"ref"`
		Button string `json:"button"`
		Clicks int    `json:"clicks"`
	}
	if !readJSON(w, r, 4096, &req) {
		return
	}
	button, err := buttonNumber(req.Button)
	if err != nil {
		browserFail(w, err)
		return
	}
	_, tg, err := s.locate(r.Context(), req.Tab, req.Ref)
	if err != nil {
		browserFail(w, err)
		return
	}
	x, y := s.clickAt(tg, button, min(max(req.Clicks, 1), 3))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "x": x, "y": y})
}

func (s *server) handleBrowserFill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tab    string `json:"tab"`
		Ref    int    `json:"ref"`
		Text   string `json:"text"`
		Clear  *bool  `json:"clear"`
		Submit bool   `json:"submit"`
	}
	if !readJSON(w, r, maxClipboard+4096, &req) {
		return
	}
	if len(req.Text) > maxClipboard {
		browserFail(w, errors.New("text too long"))
		return
	}
	ctx := r.Context()
	pg, tg, err := s.locate(ctx, req.Tab, req.Ref)
	if err != nil {
		browserFail(w, err)
		return
	}
	if tg.Tag == "select" { // a click would open its popup: pick the option directly
		if err := s.br.refCall(ctx, pg, selectJS, req.Ref, req.Text); err != nil {
			browserFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	switch tg.Type {
	case "checkbox", "radio", "submit", "button", "reset", "image", "file", "range", "color":
		browserFail(w, fmt.Errorf("can't type into a %s input: use /agent/browser/click", tg.Type))
		return
	}
	s.clickAt(tg, 1, 1)
	if err := s.br.refCall(ctx, pg, focusJS, req.Ref); err != nil {
		browserFail(w, err)
		return
	}
	if req.Clear == nil || *req.Clear {
		s.x.pressKeys("ctrl+a")
		s.x.pressKeys("backspace")
	}
	if err := s.x.typeText(req.Text, 0); err != nil {
		browserFail(w, err)
		return
	}
	if req.Submit {
		s.x.pressKeys("enter")
	}
	s.x.sync()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleBrowserWait(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tab       string `json:"tab"`
		Until     string `json:"until"`
		Text      string `json:"text"`
		TimeoutMs int    `json:"timeout_ms"`
	}
	if !readJSON(w, r, 16<<10, &req) {
		return
	}
	if req.Until == "" {
		req.Until = "load"
	}
	var check string
	switch req.Until {
	case "load", "idle":
		check = `document.readyState === 'complete'`
	case "text":
		if req.Text == "" {
			browserFail(w, errors.New(`until "text" needs text`))
			return
		}
		check = jsCall(`(t) => (document.body?.innerText || '').includes(t)`, req.Text)
	default:
		browserFail(w, errors.New(`until must be "load", "idle" or "text"`))
		return
	}
	timeout := time.Duration(min(max(req.TimeoutMs, 100), 60000)) * time.Millisecond
	if req.TimeoutMs == 0 {
		timeout = 10 * time.Second
	}
	ctx := r.Context()
	t, err := s.pickTab(ctx, req.Tab)
	if err != nil {
		browserFail(w, err)
		return
	}
	pg, err := s.br.ready(ctx, t.ID)
	if err != nil {
		browserFail(w, err)
		return
	}
	// Network idle: nothing in flight for 500 ms. Requests open for over
	// 10 s (long polling, streams) don't count.
	idle := func() bool {
		s.br.mu.Lock()
		defer s.br.mu.Unlock()
		now := time.Now()
		for _, r := range pg.requests {
			if now.Sub(r.at) < 10*time.Second {
				return false
			}
		}
		return now.Sub(pg.netChange) >= 500*time.Millisecond
	}
	start := time.Now()
	for {
		var ok bool
		err := s.br.eval(ctx, pg, check, &ok)
		if err != nil {
			var de dialogError
			if errors.As(err, &de) {
				browserFail(w, err)
				return
			}
			ok = false // navigating: no document yet
		}
		if ok && req.Until == "idle" {
			ok = idle()
		}
		waited := time.Since(start)
		if ok || waited >= timeout {
			writeJSON(w, http.StatusOK, map[string]any{"until": req.Until, "ok": ok, "waited_ms": waited.Milliseconds()})
			return
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
}

func (s *server) handleBrowserDialog(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tab    string  `json:"tab"`
		Accept *bool   `json:"accept"`
		Text   *string `json:"text"`
	}
	if !readJSON(w, r, 16<<10, &req) {
		return
	}
	if req.Accept == nil {
		browserFail(w, errors.New("accept (true or false) is required"))
		return
	}
	ctx := r.Context()
	t, err := s.pickTab(ctx, req.Tab)
	if err != nil {
		browserFail(w, err)
		return
	}
	pg, err := s.br.ready(ctx, t.ID)
	if err != nil {
		browserFail(w, err)
		return
	}
	if s.br.dialogOf(pg) == nil {
		browserFail(w, errors.New("no dialog is open in that tab"))
		return
	}
	params := map[string]any{"accept": *req.Accept}
	if req.Text != nil {
		params["promptText"] = *req.Text
	}
	if _, err := s.br.call(ctx, pg.session, "Page.handleJavaScriptDialog", params); err != nil {
		browserFail(w, err)
		return
	}
	s.br.mu.Lock()
	pg.dialog = nil
	s.br.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
