package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// trayIcon — кольцо со стрелкой-галочкой: чёрное на прозрачном, на macOS — шаблонная иконка.
func trayIcon() []byte {
	const n = 44
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	c := float64(n-1) / 2
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)-c, float64(y)-c
			r := math.Hypot(dx, dy)
			// кольцо толщиной 4 и точка в центре
			a := math.Max(edge(r, 15, 19), edge(r, -1, 6))
			if a > 0 {
				img.SetNRGBA(x, y, color.NRGBA{A: uint8(a * 255)})
			}
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// edge — 1 внутри [lo, hi], плавно спадает на пиксель по краям.
func edge(r, lo, hi float64) float64 {
	return math.Max(0, math.Min(1, math.Min(r-lo+0.5, hi-r+0.5)))
}
