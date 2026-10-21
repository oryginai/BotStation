package main

import (
	"testing"
	"time"
)

func newTestServer(w, h int) *server {
	fb := &framebuffer{width: w, height: h, stride: w * 4}
	fb.pix = make([]byte, fb.stride*fb.height)
	return &server{fb: fb, cfg: config{quality: 85, motionQuality: 45}}
}

func (fb *framebuffer) touch(x, y int) { fb.pix[y*fb.stride+x*4]++ }

func TestScanAndRuns(t *testing.T) {
	for _, w := range []int{1280, 1300} { // with and without a partial last tile
		s := newTestServer(w, 100)
		ts := newTileState(s.fb)

		if got := s.nextFrame(ts, true); len(got) != 2 || got[0] != (rect{0, 0, w, 64, 85}) || got[1] != (rect{0, 64, w, 36, 85}) {
			t.Fatalf("w=%d full refresh: %v", w, got)
		}
		if got := s.nextFrame(ts, false); len(got) != 0 {
			t.Fatalf("w=%d unchanged screen: %v", w, got)
		}
		s.fb.touch(w-1, 10) // last column
		s.fb.touch(70, 5)   // second tile of row 0
		last := (w - 1) / 64 * 64
		want := []rect{{64, 0, 64, 64, 85}, {last, 0, w - last, 64, 85}}
		got := s.nextFrame(ts, false)
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("w=%d partial: got %v want %v", w, got, want)
		}
	}
}

func TestMotionThenSharpResend(t *testing.T) {
	s := newTestServer(1280, 720)
	ts := newTileState(s.fb)
	s.nextFrame(ts, true)

	// a big change (scrolling) goes out at motion quality
	for x := 0; x < 1280; x += 64 {
		s.fb.touch(x, 100)
		s.fb.touch(x, 200)
	}
	got := s.nextFrame(ts, false)
	if len(got) != 2 || got[0].quality != 45 || got[1].quality != 45 {
		t.Fatalf("motion frame: %v", got)
	}
	// nothing before the tiles have settled
	if got := s.nextFrame(ts, false); len(got) != 0 {
		t.Fatalf("unsettled: %v", got)
	}
	// once still, the same tiles are re-sent sharp, then nothing more
	for i := range ts.changedAt {
		ts.changedAt[i] = ts.changedAt[i].Add(-time.Second)
	}
	got = s.nextFrame(ts, false)
	if len(got) != 2 || got[0] != (rect{0, 64, 1280, 64, 85}) || got[1] != (rect{0, 192, 1280, 64, 85}) {
		t.Fatalf("sharp resend: %v", got)
	}
	if got := s.nextFrame(ts, false); len(got) != 0 {
		t.Fatalf("after resend: %v", got)
	}

	// a small change (typing) is always sharp
	s.fb.touch(300, 300)
	if got := s.nextFrame(ts, false); len(got) != 1 || got[0].quality != 85 {
		t.Fatalf("typing: %v", got)
	}
}

func TestKeycodes(t *testing.T) {
	for code, want := range map[string]byte{"KeyA": 38, "KeyQ": 24, "Digit1": 10, "Digit0": 19, "F1": 67, "F10": 76, "F12": 96, "Enter": 36, "ArrowUp": 111, "ShiftLeft": 50} {
		if keycodes[code] != want {
			t.Errorf("%s = %d, want %d", code, keycodes[code], want)
		}
	}
}
