package handlers

import (
	"bytes"
	"image"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMCC_CompositeSample renders a composite from real photos when
// MCC_SAMPLE_BEFORE / MCC_SAMPLE_AFTER (comma-separated paths) and
// MCC_DUMP_COMPOSITE are set; skipped otherwise.
func TestMCC_CompositeSample(t *testing.T) {
	out := os.Getenv("MCC_DUMP_COMPOSITE")
	if out == "" || os.Getenv("MCC_SAMPLE_BEFORE") == "" {
		t.Skip("set MCC_SAMPLE_BEFORE, MCC_SAMPLE_AFTER and MCC_DUMP_COMPOSITE")
	}
	load := func(list string) []image.Image {
		var imgs []image.Image
		for _, p := range strings.Split(list, ",") {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			data, err := os.ReadFile(p)
			require.NoError(t, err)
			img, _, err := image.Decode(bytes.NewReader(data))
			require.NoError(t, err)
			imgs = append(imgs, img)
		}
		return imgs
	}
	data, err := mccDrawBeforeAfter(load(os.Getenv("MCC_SAMPLE_BEFORE")), load(os.Getenv("MCC_SAMPLE_AFTER")),
		"MMP-SW-000011", "Sweeping of roads  ·  Ward 3  ·  Resolved 15 Nov 2026")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(out, data, 0o644))
}
