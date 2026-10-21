package main

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// offLine is how far x, y is from the line through a and b, signed by side,
// and how far along it (0 at a, 1 at b)
func offLine(ax, ay, bx, by, x, y float64) (side, along float64) {
	dx, dy := bx-ax, by-ay
	d := math.Hypot(dx, dy)
	return ((x-ax)*dy - (y-ay)*dx) / d, ((x-ax)*dx + (y-ay)*dy) / (d * d)
}

func TestHumanPath(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var overshoots, sCurves, long int
	for n := 0; n < 5000; n++ {
		fx, fy := r.IntN(1280), r.IntN(720)
		px, py := r.IntN(1280), r.IntN(720)
		dist := math.Hypot(float64(px-fx), float64(py-fy))
		if dist < 4 {
			continue
		}
		overshoot := n%2 == 0
		steps := humanPath(fx, fy, px, py, r, overshoot)

		last := steps[len(steps)-1]
		if last.x != px || last.y != py {
			t.Fatalf("%d,%d -> %d,%d: ends at %d,%d", fx, fy, px, py, last.x, last.y)
		}
		var total time.Duration
		maxSide, minSide, maxAlong := 0.0, 0.0, 0.0
		for _, st := range steps {
			total += st.pause
			side, along := offLine(float64(fx), float64(fy), float64(px), float64(py), float64(st.x), float64(st.y))
			maxSide, minSide, maxAlong = max(maxSide, side), min(minSide, side), max(maxAlong, along)
		}
		// Bends up to 12% (max 40 px); an overshoot adds up to 0.4 x 18 px
		// sideways; tremor and rounding about 1 px
		if limit := min(dist*0.12, 40) + 8.5; maxSide > limit || -minSide > limit {
			t.Fatalf("%d,%d -> %d,%d: %.1f/%.1f px off the line (limit %.1f)", fx, fy, px, py, maxSide, minSide, limit)
		}
		if total < 80*time.Millisecond || total > 600*time.Millisecond {
			t.Fatalf("%d,%d -> %d,%d: took %v", fx, fy, px, py, total)
		}
		past := (maxAlong - 1) * dist // px beyond the target
		if !overshoot && past > 2 {
			t.Fatalf("%d,%d -> %d,%d: went %.1f px past the target without overshoot", fx, fy, px, py, past)
		}
		if past > 18+2 {
			t.Fatalf("%d,%d -> %d,%d: overshot by %.1f px", fx, fy, px, py, past)
		}
		if overshoot && dist > 150 {
			long++
			if past > 2 {
				overshoots++
			}
		}
		if maxSide > 3 && -minSide > 3 {
			sCurves++
		}
	}
	// About a third of long moves overshoot; some paths are S-shaped
	if f := float64(overshoots) / float64(long); f < 0.2 || f > 0.45 {
		t.Errorf("overshoot on %.0f%% of long moves, want about 33%%", f*100)
	}
	if sCurves == 0 {
		t.Error("no S-shaped paths")
	}
}

func TestHumanPathTiny(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	steps := humanPath(10, 10, 10, 10, r, true)
	if len(steps) != 1 || steps[0].x != 10 || steps[0].y != 10 {
		t.Fatalf("zero-length move: %+v", steps)
	}
}
