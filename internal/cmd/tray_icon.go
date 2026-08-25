package cmd

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

// generateTrayIconICO renders a small indigo "A"-dot icon at runtime so no
// binary asset file is needed. Windows accepts PNG-compressed entries inside
// an ICO container (Vista+).
func generateTrayIconICO() []byte {
	const size = 32
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	indigo := color.NRGBA{R: 79, G: 70, B: 229, A: 255} // indigo-600
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			// Rounded-corner mask so the square does not look clipped.
			if cornerMasked(x, y, size) {
				img.Set(x, y, color.NRGBA{})
				continue
			}
			img.Set(x, y, indigo)
		}
	}
	// White dot as the "signal".
	for y := 12; y < 20; y++ {
		for x := 12; x < 20; x++ {
			if (x-16)*(x-16)+(y-16)*(y-16) <= 9 {
				img.Set(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
			}
		}
	}

	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		return nil
	}

	ico := make([]byte, 6+16+len(pngBuf.Bytes()))
	binary.LittleEndian.PutUint16(ico[0:], 0)   // reserved
	binary.LittleEndian.PutUint16(ico[2:], 1)   // type: icon
	binary.LittleEndian.PutUint16(ico[4:], 1)   // count
	ico[6] = size                               // width
	ico[7] = size                               // height
	ico[8] = 0                                  // palette
	ico[9] = 0                                  // reserved
	binary.LittleEndian.PutUint16(ico[10:], 1)  // planes
	binary.LittleEndian.PutUint16(ico[12:], 32) // bpp
	binary.LittleEndian.PutUint32(ico[14:], uint32(len(pngBuf.Bytes())))
	binary.LittleEndian.PutUint32(ico[18:], 22) // data offset
	copy(ico[22:], pngBuf.Bytes())
	return ico
}

func cornerMasked(x, y, size int) bool {
	edge := func(v int) int {
		switch {
		case v < 4:
			return 4 - v
		case v >= size-4:
			return v - (size - 5)
		default:
			return 0
		}
	}
	return edge(x)+edge(y) > 6
}
