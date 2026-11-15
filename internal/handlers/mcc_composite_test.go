package handlers

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func solidImage(w, h int, c color.RGBA) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func TestMCC_DrawBeforeAfter(t *testing.T) {
	red := color.RGBA{200, 30, 30, 255}
	green := color.RGBA{30, 160, 60, 255}
	// Portrait "before", three "after" photos: fitted, never cropped.
	data, err := mccDrawBeforeAfter([]image.Image{solidImage(600, 1000, red)},
		[]image.Image{solidImage(1600, 900, green), solidImage(900, 900, green), solidImage(400, 900, green)},
		"MMP-SW-000010", "Burning of garbage  ·  Ward 37  ·  Resolved 15 Nov 2026")
	require.NoError(t, err)
	if out := os.Getenv("MCC_DUMP_COMPOSITE"); out != "" {
		require.NoError(t, os.WriteFile(out, data, 0o644))
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, mccCompositeW, img.Bounds().Dx())
	assert.Equal(t, mccCompositeH, img.Bounds().Dy())
	assert.Less(t, len(data), 400_000, "small enough for a template header")

	near := func(c color.Color, want color.RGBA) bool {
		r, g, b, _ := c.RGBA()
		d := func(a uint32, b uint8) int {
			x := int(a>>8) - int(b)
			if x < 0 {
				x = -x
			}
			return x
		}
		return d(r, want.R) < 20 && d(g, want.G) < 20 && d(b, want.B) < 20
	}
	photoH := mccCompositeH - mccCompositeStrip
	halfW := (mccCompositeW - mccCompositeGutter) / 2
	assert.True(t, near(img.At(halfW/2, photoH/2), red), "before photo centred in the left half")
	assert.True(t, near(img.At(10, photoH/2), mccColorCell), "portrait photo letterboxed, not cropped")
	assert.True(t, near(img.At(mccCompositeW/2, photoH/2), mccColorBg), "white gutter between the halves")
	right := halfW + mccCompositeGutter
	assert.True(t, near(img.At(right+halfW/2, photoH*3/10), green), "first after photo on top")
	assert.True(t, near(img.At(right+halfW/4, photoH*85/100), green), "second after photo bottom left")
	assert.True(t, near(img.At(right+halfW*3/4, photoH*85/100), green), "third after photo bottom right")
	assert.True(t, near(img.At(600, mccCompositeH-10), mccColorStrip), "caption strip at the bottom")

	// Missing photos render placeholder panels instead of failing.
	data, err = mccDrawBeforeAfter(nil, nil, "MMP-SW-000011", "x")
	require.NoError(t, err)
	img, err = jpeg.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.True(t, near(img.At(300, photoH-20), mccColorPanel))
}
