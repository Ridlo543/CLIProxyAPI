package cmd

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestGenerateTrayIconICOIsValidContainer(t *testing.T) {
	ico := generateTrayIconICO()
	if len(ico) < 22 {
		t.Fatalf("ico too small: %d bytes", len(ico))
	}
	if got := binary.LittleEndian.Uint16(ico[0:]); got != 0 {
		t.Fatalf("reserved = %d, want 0", got)
	}
	if got := binary.LittleEndian.Uint16(ico[2:]); got != 1 {
		t.Fatalf("type = %d, want 1 (icon)", got)
	}
	count := int(binary.LittleEndian.Uint16(ico[4:]))
	if count != 2 {
		t.Fatalf("image count = %d, want 2", count)
	}

	offset := 6 + 16*count
	for i := 0; i < count; i++ {
		dirEntry := ico[6+16*i : 6+16*(i+1)]
		dataLen := binary.LittleEndian.Uint32(dirEntry[8:])
		dataOffset := binary.LittleEndian.Uint32(dirEntry[12:])
		if dataOffset != uint32(offset) {
			t.Fatalf("entry %d offset = %d, want %d", i, dataOffset, offset)
		}
		if int(dataOffset+dataLen) > len(ico) {
			t.Fatalf("entry %d overruns container", i)
		}
		// PNG signature inside each entry (Vista+ PNG-compressed ICO).
		pngSig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
		if !bytes.Equal(ico[dataOffset:dataOffset+8], pngSig) {
			t.Fatalf("entry %d is not a PNG payload", i)
		}
		offset += int(dataLen)
	}
}
