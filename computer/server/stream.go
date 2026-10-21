package main

// Stream protocol, one WebSocket per viewer, on wss:// (TLS 1.3).
// The client offers two subprotocols: "desktop.v1" and "token.<viewer token>";
// the server answers with "desktop.v1". Integers are big-endian.
//
// Server -> client
//
//	text    {"type":"hello","width":1280,"height":720,"view_only":false}
//	text    {"type":"cursor","png":"<base64 PNG>","x":hotX,"y":hotY}
//	text    {"type":"pointer","x":10,"y":20}  pointer moved (by anyone), in desktop pixels
//	text    {"type":"clipboard","text":"..."}  copied in the desktop (not to view-only)
//	binary  frame: 0x01, u16 count, then count times:
//	        u16 x, u16 y, u16 w, u16 h, u32 len, len bytes of JPEG
//	        Draw every tile of a frame, then send an ack.
//
// Client -> server (text JSON)
//
//	{"t":"ack"}                     frame drawn (at most 2 frames are in flight)
//	{"t":"r"}                       resend the whole screen
//	{"t":"m","x":10,"y":20}         pointer move, in desktop pixels
//	{"t":"b","b":1,"d":true}        button down/up: 1 left, 2 middle, 3 right
//	{"t":"w","dx":0,"dy":1}         wheel, one notch per message
//	{"t":"k","c":"KeyA","d":true}   key down/up by KeyboardEvent.code
//	{"t":"clip","text":"..."}       make this the desktop's clipboard (send it
//	                                before the Ctrl+V keys that paste it)
//
// Nothing is captured or encoded unless a viewer is connected, and a viewer
// only gets the 64x64 tiles that changed since its last frame. While a large
// area keeps changing (scrolling, dragging) those tiles go out at
// STREAM_MOTION_QUALITY; each is re-sent at STREAM_QUALITY once it has been
// still for a moment, so text ends up sharp. Small changes (typing) are
// always sent at full quality.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

const tileSize = 64

// framebuffer is Xvfb's screen, memory-mapped from its -fbdir file (an XWD
// image the X server draws into). Reading it costs no X round trips and
// shows exactly what is on screen, compositor output included.
type framebuffer struct {
	pix           []byte // BGRX, 4 bytes per pixel
	width, height int
	stride        int
}

func openFramebuffer(path string, wait time.Duration) (*framebuffer, error) {
	deadline := time.Now().Add(wait)
	for {
		fb, err := mapFramebuffer(path)
		if err == nil || time.Now().After(deadline) {
			return fb, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func mapFramebuffer(path string) (*framebuffer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < 100 {
		return nil, errors.New("framebuffer not ready")
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}

	// XWD header: 25 big-endian uint32s, then the window name and colormap
	h := func(i int) int { return int(binary.BigEndian.Uint32(data[i*4:])) }
	headerSize, width, height := h(0), h(4), h(5)
	byteOrder, bpp, stride, redMask, ncolors := h(7), h(11), h(12), h(14), h(19)
	if bpp != 32 || byteOrder != 0 || redMask != 0xff0000 {
		syscall.Munmap(data)
		return nil, fmt.Errorf("unsupported framebuffer format (bpp=%d order=%d red=%#x), run Xvfb with -screen 0 WxHx24", bpp, byteOrder, redMask)
	}
	off := headerSize + ncolors*12
	if off+stride*height > len(data) {
		syscall.Munmap(data)
		return nil, errors.New("framebuffer file is truncated")
	}
	return &framebuffer{pix: data[off : off+stride*height], width: width, height: height, stride: stride}, nil
}

type rect struct{ x, y, w, h, quality int }

func (fb *framebuffer) cols() int { return (fb.width + tileSize - 1) / tileSize }
func (fb *framebuffer) rows() int { return (fb.height + tileSize - 1) / tileSize }

// scan copies every tile that differs from snap (or all of them when full)
// into snap and marks it in dirty. It returns how many tiles changed.
func (fb *framebuffer) scan(snap []byte, dirty []bool, full bool) int {
	n, cols := 0, fb.cols()
	for i := range dirty {
		tx, ty := i%cols*tileSize, i/cols*tileSize
		tw, th := min(tileSize, fb.width-tx), min(tileSize, fb.height-ty)
		dirty[i] = false
		for y := ty; y < ty+th; y++ {
			a, b := y*fb.stride+tx*4, y*fb.stride+(tx+tw)*4
			if full || !bytes.Equal(fb.pix[a:b], snap[a:b]) {
				dirty[i] = true
				n++
				for ; y < ty+th; y++ { // copy the rest of the tile
					a, b = y*fb.stride+tx*4, y*fb.stride+(tx+tw)*4
					copy(snap[a:b], fb.pix[a:b])
				}
				break
			}
		}
	}
	return n
}

// runs merges the selected tiles of each tile row into horizontal rects
func (fb *framebuffer) runs(sel []bool, quality int, out []rect) []rect {
	cols := fb.cols()
	for row := 0; row < fb.rows(); row++ {
		ty := row * tileSize
		th := min(tileSize, fb.height-ty)
		start := -1
		for c := 0; c <= cols; c++ {
			on := c < cols && sel[row*cols+c]
			switch {
			case on && start < 0:
				start = c
			case !on && start >= 0:
				x := start * tileSize
				out = append(out, rect{x, ty, min(c*tileSize, fb.width) - x, th, quality})
				start = -1
			}
		}
	}
	return out
}

// tileState is what one viewer has been sent, tile by tile
type tileState struct {
	snap      []byte // the pixels the viewer currently shows
	dirty     []bool
	low       []bool      // last sent at motion quality, needs a sharp resend
	changedAt []time.Time // when each tile last changed
	hi, lo    []bool      // scratch: tiles to send this frame, by quality
}

func newTileState(fb *framebuffer) *tileState {
	n := fb.cols() * fb.rows()
	return &tileState{
		snap: make([]byte, len(fb.pix)), dirty: make([]bool, n), low: make([]bool, n),
		changedAt: make([]time.Time, n), hi: make([]bool, n), lo: make([]bool, n),
	}
}

const (
	motionTiles = 24                     // this many changed tiles in a frame counts as motion
	settleTime  = 300 * time.Millisecond // a tile still this long gets its sharp resend
)

// nextFrame returns the rects to send now, or nil if nothing changed
func (s *server) nextFrame(t *tileState, full bool) []rect {
	now := time.Now()
	motion := s.fb.scan(t.snap, t.dirty, full) >= motionTiles && !full
	for i, d := range t.dirty {
		t.hi[i], t.lo[i] = false, false
		switch {
		case d && motion:
			t.lo[i], t.low[i] = true, true
		case d:
			t.hi[i], t.low[i] = true, false
		case t.low[i] && now.Sub(t.changedAt[i]) >= settleTime:
			t.hi[i], t.low[i] = true, false
		}
		if d {
			t.changedAt[i] = now
		}
	}
	rects := s.fb.runs(t.hi, s.cfg.quality, nil)
	return s.fb.runs(t.lo, s.cfg.motionQuality, rects)
}

// encodeFrame JPEG-encodes the rects (from the viewer's snapshot) in parallel
// and packs them into one frame message.
func (s *server) encodeFrame(snap []byte, rects []rect) ([]byte, error) {
	jpegs := make([][]byte, len(rects))
	errs := make([]error, len(rects))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i, r := range rects {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			img := image.NewRGBA(image.Rect(0, 0, r.w, r.h))
			for y := 0; y < r.h; y++ {
				src := snap[(r.y+y)*s.fb.stride+r.x*4:]
				dst := img.Pix[y*img.Stride:]
				for x := 0; x < r.w*4; x += 4 {
					dst[x], dst[x+1], dst[x+2], dst[x+3] = src[x+2], src[x+1], src[x], 0xff
				}
			}
			var buf bytes.Buffer
			errs[i] = jpeg.Encode(&buf, img, &jpeg.Options{Quality: r.quality})
			jpegs[i] = buf.Bytes()
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	size := 3
	for _, j := range jpegs {
		size += 12 + len(j)
	}
	msg := make([]byte, 3, size)
	msg[0] = 1
	binary.BigEndian.PutUint16(msg[1:], uint16(len(rects)))
	for i, r := range rects {
		msg = binary.BigEndian.AppendUint16(msg, uint16(r.x))
		msg = binary.BigEndian.AppendUint16(msg, uint16(r.y))
		msg = binary.BigEndian.AppendUint16(msg, uint16(r.w))
		msg = binary.BigEndian.AppendUint16(msg, uint16(r.h))
		msg = binary.BigEndian.AppendUint32(msg, uint32(len(jpegs[i])))
		msg = append(msg, jpegs[i]...)
	}
	return msg, nil
}

type viewer struct {
	ws        *websocket.Conn
	cancel    context.CancelFunc
	viewOnly  bool
	closeOnce sync.Once
}

func (v *viewer) send(typ websocket.MessageType, msg []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := v.ws.Write(ctx, typ, msg)
	if err != nil {
		v.cancel()
	}
	return err
}

// close sends a close frame with a reason; the read loop then ends the viewer
func (v *viewer) close(code websocket.StatusCode, reason string) {
	v.closeOnce.Do(func() { go v.ws.Close(code, reason) })
}

func (s *server) serveViewer(ctx context.Context, v *viewer, cursor []byte) {
	hello, _ := json.Marshal(map[string]any{
		"type": "hello", "width": s.fb.width, "height": s.fb.height, "view_only": v.viewOnly,
	})
	if v.send(websocket.MessageText, hello) != nil {
		return
	}
	if cursor != nil && v.send(websocket.MessageText, cursor) != nil {
		return
	}

	credits := make(chan struct{}, 2) // frames the viewer may have in flight
	credits <- struct{}{}
	credits <- struct{}{}
	var refresh atomic.Bool
	refresh.Store(true)
	go func() {
		defer v.cancel()
		s.readInput(ctx, v, credits, &refresh)
	}()

	tiles := newTileState(s.fb)
	tick := time.NewTicker(time.Second / time.Duration(s.cfg.fps))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-credits:
		}
		var rects []rect
		for len(rects) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			rects = s.nextFrame(tiles, refresh.Swap(false))
		}
		msg, err := s.encodeFrame(tiles.snap, rects)
		if err != nil || v.send(websocket.MessageBinary, msg) != nil {
			return
		}
	}
}

func (s *server) readInput(ctx context.Context, v *viewer, credits chan struct{}, refresh *atomic.Bool) {
	for {
		_, data, err := v.ws.Read(ctx)
		if err != nil {
			return
		}
		var m struct {
			T    string `json:"t"`
			X    int    `json:"x"`
			Y    int    `json:"y"`
			B    int    `json:"b"`
			D    bool   `json:"d"`
			DX   int    `json:"dx"`
			DY   int    `json:"dy"`
			C    string `json:"c"`
			Text string `json:"text"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.T {
		case "ack":
			select {
			case credits <- struct{}{}:
			default:
			}
		case "r":
			refresh.Store(true)
		}
		if v.viewOnly {
			continue
		}
		switch m.T {
		case "m":
			s.x.move(min(max(m.X, 0), s.fb.width-1), min(max(m.Y, 0), s.fb.height-1))
		case "b":
			s.x.button(m.B, m.D)
		case "w":
			s.x.wheel(m.DX, m.DY)
		case "k":
			s.x.key(m.C, m.D)
		case "clip":
			if s.cfg.clipToDesktop {
				s.x.clip.set(m.Text)
			}
		}
	}
}
