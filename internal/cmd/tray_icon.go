package cmd

import (
	"bytes"
	"embed"
	"encoding/binary"
	"image/png"
)

//go:embed assets/logo-256.png assets/logo-32.png
var trayLogoFS embed.FS

// generateTrayIconICO builds a Windows .ico container from the official
// AinyRouter logo renders (transparent background, brand crimson mark).
// Two sizes are embedded: 32px for the tray and 256px for shell scaling.
func generateTrayIconICO() []byte {
	entries := []struct {
		width  byte
		height byte
		file   string
	}{
		{32, 32, "assets/logo-32.png"},
		{0, 0, "assets/logo-256.png"}, // width/height byte 0 == 256
	}

	type pngEntry struct {
		header [16]byte
		data   []byte
	}
	var entriesOut []pngEntry

	for _, e := range entries {
		raw, err := trayLogoFS.ReadFile(e.file)
		if err != nil {
			continue
		}
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			continue
		}
		b := img.Bounds()
		var header [16]byte
		header[0] = byte(b.Dx())
		header[1] = byte(b.Dy())
		header[2] = 0 // palette
		header[3] = 0 // reserved
		header[4] = 1 // color planes
		header[5] = 32
		dataLen := uint32(len(raw))
		binary.LittleEndian.PutUint32(header[8:], dataLen)
		entriesOut = append(entriesOut, pngEntry{header: header, data: raw})
	}
	if len(entriesOut) == 0 {
		return nil
	}

	offset := 6 + 16*len(entriesOut)
	total := offset
	for _, en := range entriesOut {
		total += len(en.data)
	}

	out := make([]byte, total)
	binary.LittleEndian.PutUint16(out[0:], 0) // reserved
	binary.LittleEndian.PutUint16(out[2:], 1) // type: icon
	binary.LittleEndian.PutUint16(out[4:], uint16(len(entriesOut)))

	pos := 6
	for _, en := range entriesOut {
		copy(out[pos:pos+16], en.header[:])
		binary.LittleEndian.PutUint32(out[pos+12:], uint32(offset))
		copy(out[offset:], en.data)
		offset += len(en.data)
		pos += 16
	}
	return out
}
