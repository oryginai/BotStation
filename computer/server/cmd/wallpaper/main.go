// wallpaper sets an image, scaled to fill the screen, as the root window's
// background and exits. The X server keeps the pixmap and repaints the
// desktop from it by itself, so nothing stays running (it replaces
// xfdesktop: no icons, no menus).
//
//	wallpaper [image]   (default /usr/share/backgrounds/wallpaper.jpg)
package main

import (
	"image"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"os"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

func main() {
	path := "/usr/share/backgrounds/wallpaper.jpg"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}

	var c *xgb.Conn
	var err error
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if c, err = xgb.NewConn(); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		log.Fatalf("wallpaper: X: %v", err)
	}
	setup := xproto.Setup(c)
	screen := setup.DefaultScreen(c)
	w, h := int(screen.WidthInPixels), int(screen.HeightInPixels)

	pix, err := xproto.NewPixmapId(c)
	if err != nil {
		log.Fatal(err)
	}
	xproto.CreatePixmap(c, screen.RootDepth, pix, xproto.Drawable(screen.Root), uint16(w), uint16(h))
	if err := upload(c, setup, screen, pix, load(path, w, h), w, h); err != nil {
		log.Fatalf("wallpaper: %v", err)
	}
	root := screen.Root
	xproto.ChangeWindowAttributes(c, root, xproto.CwBackPixmap, []uint32{uint32(pix)})
	xproto.ClearArea(c, false, root, 0, 0, 0, 0)
	// Where other programs look for the wallpaper
	for _, prop := range []string{"_XROOTPMAP_ID", "ESETROOT_PMAP_ID"} {
		setProp(c, root, prop, "PIXMAP", uint32(pix))
	}
	// Keep the pixmap after this program exits
	xproto.SetCloseDownMode(c, xproto.CloseDownRetainPermanent)
	xproto.GetInputFocus(c).Reply() // round trip: every request above is done
	c.Close()
}

// load decodes the image and scales it to cover w x h (cropping the
// overflow), averaging the source pixels under each screen pixel.
// It returns BGRX rows, the X server's 24-bit pixel layout.
func load(path string, w, h int) []byte {
	out := make([]byte, w*h*4)
	f, err := os.Open(path)
	if err != nil {
		log.Printf("wallpaper: %v (using a plain background)", err)
		return out
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		log.Printf("wallpaper: %v (using a plain background)", err)
		return out
	}
	b := src.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)

	sw, sh := float64(b.Dx()), float64(b.Dy())
	scale := max(float64(w)/sw, float64(h)/sh)
	offX, offY := (sw*scale-float64(w))/2, (sh*scale-float64(h))/2
	for y := 0; y < h; y++ {
		y0, y1 := int((float64(y)+offY)/scale), int((float64(y+1)+offY)/scale)
		y1 = min(max(y1, y0+1), b.Dy())
		for x := 0; x < w; x++ {
			x0, x1 := int((float64(x)+offX)/scale), int((float64(x+1)+offX)/scale)
			x1 = min(max(x1, x0+1), b.Dx())
			var r, g, bl, n int
			for sy := y0; sy < y1; sy++ {
				row := rgba.Pix[sy*rgba.Stride:]
				for sx := x0; sx < x1; sx++ {
					r += int(row[sx*4])
					g += int(row[sx*4+1])
					bl += int(row[sx*4+2])
					n++
				}
			}
			o := (y*w + x) * 4
			out[o], out[o+1], out[o+2] = byte(bl/n), byte(g/n), byte(r/n)
		}
	}
	return out
}

// upload copies the pixels into the pixmap, in bands that fit one request
func upload(c *xgb.Conn, setup *xproto.SetupInfo, screen *xproto.ScreenInfo, pix xproto.Pixmap, data []byte, w, h int) error {
	gc, err := xproto.NewGcontextId(c)
	if err != nil {
		return err
	}
	xproto.CreateGC(c, gc, xproto.Drawable(pix), 0, nil)
	rows := max(1, (int(setup.MaximumRequestLength)*4-64)/(w*4))
	for y := 0; y < h; y += rows {
		n := min(rows, h-y)
		err := xproto.PutImageChecked(c, xproto.ImageFormatZPixmap, xproto.Drawable(pix), gc,
			uint16(w), uint16(n), 0, int16(y), 0, screen.RootDepth, data[y*w*4:(y+n)*w*4]).Check()
		if err != nil {
			return err
		}
	}
	xproto.FreeGC(c, gc)
	return nil
}

func setProp(c *xgb.Conn, win xproto.Window, prop, typ string, value uint32) {
	atom := func(name string) xproto.Atom {
		r, err := xproto.InternAtom(c, false, uint16(len(name)), name).Reply()
		if err != nil {
			log.Fatal(err)
		}
		return r.Atom
	}
	data := []byte{byte(value), byte(value >> 8), byte(value >> 16), byte(value >> 24)}
	xproto.ChangeProperty(c, xproto.PropModeReplace, win, atom(prop), atom(typ), 32, 1, data)
}
