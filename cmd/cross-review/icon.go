package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"runtime"
)

// Иконка трея рисуется кодом: две встречные стрелки по кругу — ревью туда и обратно. На macOS она
// чёрная с прозрачностью (template, система перекрашивает её под тему), на Windows и Linux — синяя.
// idle — пауза или нет связи: бледнее.
func trayIcon(idle bool) []byte {
	const n, ss = 64, 4 // итоговый размер и сглаживание: рисуем в 4 раза крупнее и усредняем
	ink := color.NRGBA{0, 0, 0, 255}
	if runtime.GOOS != "darwin" {
		ink = color.NRGBA{0x1f, 0x6f, 0xeb, 255}
	}
	if idle {
		ink.A = 110
	}
	big := float64(n * ss)
	c, r, w := big/2, big*0.33, big*0.11
	gap := 30.0 // полуширина разрыва сверху и снизу, градусы
	inside := func(x, y float64) bool {
		dx, dy := x-c, y-c
		d := math.Hypot(dx, dy)
		ang := math.Atan2(dy, dx) * 180 / math.Pi
		if math.Abs(d-r) < w/2 && !(math.Abs(ang+90) < gap || math.Abs(ang-90) < gap) {
			return true
		}
		// наконечники на концах дуг по ходу движения против часовой стрелки
		for _, deg := range []float64{-90 + gap, 90 + gap} {
			th := deg * math.Pi / 180
			px, py := c+r*math.Cos(th), c+r*math.Sin(th)
			dirX, dirY := math.Sin(th), -math.Cos(th)
			if inTriangle(x, y, px+dirX*w*2.1, py+dirY*w*2.1, px-dirY*w*1.5, py+dirX*w*1.5, px+dirY*w*1.5, py-dirX*w*1.5) {
				return true
			}
		}
		return false
	}
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			hits := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					if inside(float64(x*ss+sx)+0.5, float64(y*ss+sy)+0.5) {
						hits++
					}
				}
			}
			if hits > 0 {
				px := ink
				px.A = uint8(int(ink.A) * hits / (ss * ss))
				img.SetNRGBA(x, y, px)
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	if runtime.GOOS == "windows" {
		return pngToICO(buf.Bytes(), n)
	}
	return buf.Bytes()
}

func inTriangle(x, y, ax, ay, bx, by, cx, cy float64) bool {
	s1 := (bx-ax)*(y-ay) - (by-ay)*(x-ax)
	s2 := (cx-bx)*(y-by) - (cy-by)*(x-bx)
	s3 := (ax-cx)*(y-cy) - (ay-cy)*(x-cx)
	return (s1 >= 0 && s2 >= 0 && s3 >= 0) || (s1 <= 0 && s2 <= 0 && s3 <= 0)
}

// pngToICO оборачивает PNG в контейнер ICO: Windows понимает PNG внутри ICO.
func pngToICO(p []byte, size int) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, []uint16{0, 1, 1})
	b.WriteByte(byte(size))
	b.WriteByte(byte(size))
	b.WriteByte(0)
	b.WriteByte(0)
	binary.Write(&b, binary.LittleEndian, []uint16{1, 32})
	binary.Write(&b, binary.LittleEndian, []uint32{uint32(len(p)), 22})
	b.Write(p)
	return b.Bytes()
}
