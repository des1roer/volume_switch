// Package trayicon generates a small icon for the tray at runtime so the
// project does not depend on any external .ico asset.
package trayicon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

// Data holds the generated icon in .ico format (PNG-compressed, supported
// since Windows Vista).
var Data = render()

func render() []byte {
	const size = 32

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{R: 30, G: 30, B: 35, A: 255}
	fg := color.RGBA{R: 90, G: 200, B: 250, A: 255}

	cx, cy, r := size/2, size/2, size/2-4
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				img.Set(x, y, fg)
			} else {
				img.Set(x, y, bg)
			}
		}
	}

	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		panic(err)
	}

	return wrapICO(pngBuf.Bytes(), size, size)
}

// wrapICO wraps a single PNG image into a minimal valid .ico container.
func wrapICO(pngData []byte, w, h int) []byte {
	var buf bytes.Buffer

	// ICONDIR
	binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // image count

	// ICONDIRENTRY (dimensions >=256 are encoded as 0 per spec; not needed here)
	buf.WriteByte(byte(w))
	buf.WriteByte(byte(h))
	buf.WriteByte(0)                                              // color palette
	buf.WriteByte(0)                                              // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1))            // color planes
	binary.Write(&buf, binary.LittleEndian, uint16(32))           // bits per pixel
	binary.Write(&buf, binary.LittleEndian, uint32(len(pngData))) // size of image data
	binary.Write(&buf, binary.LittleEndian, uint32(6+16))         // offset to image data

	buf.Write(pngData)
	return buf.Bytes()
}
