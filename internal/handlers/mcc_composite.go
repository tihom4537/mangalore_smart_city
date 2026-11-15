package handlers

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // decode PNG photos
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"github.com/shridarpatil/whatomate/internal/models"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font/gofont/gobold"
	_ "golang.org/x/image/webp" // decode WebP photos
)

// mccFont is the bundled Go Bold face used for the labels.
var mccFont = func() *truetype.Font {
	f, err := truetype.Parse(gobold.TTF)
	if err != nil {
		panic(err)
	}
	return f
}()

func mccSetFont(dc *gg.Context, size float64) {
	dc.SetFontFace(truetype.NewFace(mccFont, &truetype.Options{Size: size}))
}

// Before/after composite sent to the citizen as the closure template's
// image header: the citizen's first complaint photo on the left, the
// supervisor's first work photo on the right, a caption strip below.
const (
	mccCompositeW      = 1200
	mccCompositeH      = 675
	mccCompositeStrip  = 75 // caption strip height
	mccCompositeGutter = 24 // white gap between the before and after halves
	mccCompositeGap    = 6  // white gap between photos on one side
	mccCompositeMax    = 3  // photos per side
	mccCompositeDir    = "images"
)

var (
	mccColorBg     = color.RGBA{0xff, 0xff, 0xff, 0xff}
	mccColorCell   = color.RGBA{0xf4, 0xf4, 0xf4, 0xff} // letterbox behind a fitted photo
	mccColorStrip  = color.RGBA{0x1f, 0x3a, 0x5f, 0xff}
	mccColorPanel  = color.RGBA{0xd9, 0xd9, 0xd9, 0xff}
	mccColorRibbon = color.RGBA{0x00, 0x00, 0x00, 0xa0} // translucent, reads on any photo
	mccColorBefore = color.RGBA{0xe7, 0x4c, 0x3c, 0xff}
	mccColorAfter  = color.RGBA{0x2e, 0xcc, 0x71, 0xff}
)

// renderMCCBeforeAfter draws the composite and returns it as JPEG. Each
// side shows up to mccCompositeMax photos, fitted without cropping; a side
// without a readable photo gets a grey "No photo" panel, so the composite
// can always be produced.
func (a *App) renderMCCBeforeAfter(g *models.MCCGrievance) ([]byte, error) {
	before := a.mccImages(g.PhotoURLs, mccCompositeMax)
	after := a.mccImages(g.ActionPhotoURLs, mccCompositeMax)
	return mccDrawBeforeAfter(before, after, mccRef(g), mccCompositeCaption(g))
}

func mccCompositeCaption(g *models.MCCGrievance) string {
	parts := g.ComplaintType
	if g.WardNo > 0 {
		parts += fmt.Sprintf("  ·  Ward %d", g.WardNo)
	}
	when := time.Now()
	if g.FixedOn != nil {
		when = *g.FixedOn
	} else if g.ResolvedAt != nil {
		when = *g.ResolvedAt
	}
	return parts + "  ·  Resolved " + when.In(mccIST).Format("02 Jan 2006")
}

// mccImages decodes up to max readable photos from the list, in order.
func (a *App) mccImages(photos models.JSONBArray, max int) []image.Image {
	var out []image.Image
	for _, raw := range photos {
		if len(out) == max {
			break
		}
		path, _ := raw.(string)
		data, _, err := a.readMCCMedia(path)
		if err != nil {
			continue
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			a.Log.Warn("MCC photo not decodable", "path", path, "error", err)
			continue
		}
		out = append(out, img)
	}
	return out
}

// mccCells splits a side into cells for n photos: 1 fills it, 2 are
// stacked, 3 are one on top and two below.
func mccCells(r image.Rectangle, n int) []image.Rectangle {
	g := mccCompositeGap
	switch {
	case n <= 1:
		return []image.Rectangle{r}
	case n == 2:
		mid := r.Min.Y + (r.Dy()-g)/2
		return []image.Rectangle{
			image.Rect(r.Min.X, r.Min.Y, r.Max.X, mid),
			image.Rect(r.Min.X, mid+g, r.Max.X, r.Max.Y),
		}
	default:
		mid := r.Min.Y + (r.Dy()-g)*6/10
		xm := r.Min.X + (r.Dx()-g)/2
		return []image.Rectangle{
			image.Rect(r.Min.X, r.Min.Y, r.Max.X, mid),
			image.Rect(r.Min.X, mid+g, xm, r.Max.Y),
			image.Rect(xm+g, mid+g, r.Max.X, r.Max.Y),
		}
	}
}

func mccDrawBeforeAfter(before, after []image.Image, ref, caption string) ([]byte, error) {
	dc := gg.NewContext(mccCompositeW, mccCompositeH)
	dc.SetColor(mccColorBg)
	dc.Clear()

	photoH := mccCompositeH - mccCompositeStrip
	halfW := (mccCompositeW - mccCompositeGutter) / 2
	panels := []struct {
		x     int
		imgs  []image.Image
		label string
		col   color.RGBA
	}{
		{0, before, "BEFORE", mccColorBefore},
		{halfW + mccCompositeGutter, after, "AFTER", mccColorAfter},
	}
	for _, p := range panels {
		rect := image.Rect(p.x, 0, p.x+halfW, photoH)
		if len(p.imgs) > 0 {
			cells := mccCells(rect, len(p.imgs))
			for i, img := range p.imgs {
				dc.SetColor(mccColorCell)
				dc.DrawRectangle(float64(cells[i].Min.X), float64(cells[i].Min.Y), float64(cells[i].Dx()), float64(cells[i].Dy()))
				dc.Fill()
				mccDrawContain(dc.Image().(draw.Image), img, cells[i])
			}
		} else {
			dc.SetColor(mccColorPanel)
			dc.DrawRectangle(float64(rect.Min.X), 0, float64(halfW), float64(photoH))
			dc.Fill()
			dc.SetRGB(0.45, 0.45, 0.45)
			mccSetFont(dc, 30)
			dc.DrawStringAnchored("No photo", float64(rect.Min.X+halfW/2), float64(photoH/2), 0.5, 0.5)
		}
		// Label ribbon: dark translucent box, coloured dot, white text.
		dc.SetColor(mccColorRibbon)
		dc.DrawRoundedRectangle(float64(rect.Min.X+18), 18, 190, 48, 8)
		dc.Fill()
		dc.SetColor(p.col)
		dc.DrawCircle(float64(rect.Min.X+18+24), 18+24, 9)
		dc.Fill()
		dc.SetRGB(1, 1, 1)
		mccSetFont(dc, 26)
		dc.DrawStringAnchored(p.label, float64(rect.Min.X+18+44), 18+24, 0, 0.5)
	}

	// Caption strip.
	dc.SetColor(mccColorStrip)
	dc.DrawRectangle(0, float64(photoH), mccCompositeW, mccCompositeStrip)
	dc.Fill()
	dc.SetRGB(1, 1, 1)
	mccSetFont(dc, 30)
	dc.DrawStringAnchored(ref, 24, float64(photoH+mccCompositeStrip/2), 0, 0.5)
	mccSetFont(dc, 22)
	dc.DrawStringAnchored(mccFitCaption(dc, caption, mccCompositeW-24-320), mccCompositeW-24, float64(photoH+mccCompositeStrip/2), 1, 0.5)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dc.Image(), &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mccDrawContain scales img to fit inside rect without cropping, centred.
func mccDrawContain(dst draw.Image, img image.Image, rect image.Rectangle) {
	sb := img.Bounds()
	sw, sh := float64(sb.Dx()), float64(sb.Dy())
	if sw == 0 || sh == 0 {
		return
	}
	scale := math.Min(float64(rect.Dx())/sw, float64(rect.Dy())/sh)
	w, h := int(sw*scale), int(sh*scale)
	x0 := rect.Min.X + (rect.Dx()-w)/2
	y0 := rect.Min.Y + (rect.Dy()-h)/2
	xdraw.CatmullRom.Scale(dst, image.Rect(x0, y0, x0+w, y0+h), img, sb, draw.Src, nil)
}

// mccFitCaption trims the caption to the available width.
func mccFitCaption(dc *gg.Context, s string, maxW float64) string {
	r := []rune(s)
	for len(r) > 0 {
		if w, _ := dc.MeasureString(string(r)); w <= maxW {
			break
		}
		r = r[:len(r)-1]
	}
	if len(r) < len([]rune(s)) && len(r) > 1 {
		return string(r[:len(r)-1]) + "…"
	}
	return string(r)
}

// saveMCCComposite stores the composite with the grievance media and
// returns its relative path.
func (a *App) saveMCCComposite(g *models.MCCGrievance, data []byte) (string, error) {
	rel := filepath.Join(mccCompositeDir, fmt.Sprintf("mcc-%d-before-after.jpg", g.ID))
	full := filepath.Join(a.getMediaStoragePath(), rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}
