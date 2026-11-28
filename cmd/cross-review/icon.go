package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"runtime"

	"github.com/goldquality/cross-review/internal/client"
)

// trayState — что показывает иконка трея.
type trayState int

const (
	stateOnline    trayState = iota // на связи, ждёт заданий
	stateOffline                    // нет связи или не настроен: бледная
	statePaused                     // пауза: янтарная, в круге знак паузы
	stateReviewing                  // идёт ревью: зелёная, в круге треугольник «пуск»
)

// iconState выбирает иконку. Ревью важнее паузы: поставленная во время ревью пауза его не
// останавливает. Пауза важнее связи: её участник ставит сам и должен её видеть.
func iconState(st client.State) trayState {
	switch {
	case st.Current != nil:
		return stateReviewing
	case !st.PausedUntil.IsZero():
		return statePaused
	case !st.Configured || !st.Online:
		return stateOffline
	}
	return stateOnline
}

var (
	inkBlue  = color.NRGBA{0x1f, 0x6f, 0xeb, 255}
	inkAmber = color.NRGBA{0xe8, 0x8a, 0x0c, 255}
	inkGreen = color.NRGBA{0x1f, 0xa6, 0x4a, 255}
)

// iconTemplate — иконка-шаблон на macOS: чёрная с прозрачностью, система перекрашивает её под тему
// строки меню. Пауза и ревью — цветные, иначе их не отличить.
func iconTemplate(s trayState) bool {
	return runtime.GOOS == "darwin" && (s == stateOnline || s == stateOffline)
}

// trayIcon — иконка для трея: PNG, на Windows — PNG внутри ICO.
func trayIcon(s trayState) []byte {
	ink := inkBlue
	switch {
	case s == statePaused:
		ink = inkAmber
	case s == stateReviewing:
		ink = inkGreen
	case iconTemplate(s):
		ink = color.NRGBA{0, 0, 0, 255}
	}
	if s == stateOffline {
		ink.A = 110
	}
	p := encodePNG(drawIcon(ink, s))
	if runtime.GOOS == "windows" {
		return pngToICO(p, iconSize)
	}
	return p
}

const iconSize = 64

// drawIcon рисует иконку кодом: две встречные стрелки по кругу — ревью туда и обратно. На паузе и во
// время ревью в правом нижнем углу кружок с вырезанным знаком: две черты или треугольник «пуск».
// Вокруг кружка прозрачный зазор, чтобы он не сливался со стрелками.
func drawIcon(ink color.NRGBA, s trayState) *image.NRGBA {
	const n, ss = iconSize, 4 // итоговый размер и сглаживание: рисуем в 4 раза крупнее и усредняем
	big := float64(n * ss)
	c, r, w := big/2, big*0.33, big*0.11
	gap := 30.0 // полуширина разрыва сверху и снизу, градусы
	badge := s == statePaused || s == stateReviewing
	br, ring := big*0.25, big*0.06 // радиус кружка и зазор вокруг него
	bc := big - br                 // центр кружка по обеим осям
	glyph := func(x, y float64) bool {
		gh := br * 1.05
		switch s {
		case statePaused:
			bw, bgap := br*0.3, br*0.3
			dx := math.Abs(x - bc)
			return math.Abs(y-bc) <= gh/2 && dx >= bgap/2 && dx <= bgap/2+bw
		case stateReviewing:
			tw := gh * 0.9
			x0 := bc - tw/3 // центр тяжести треугольника — в центре кружка
			return inTriangle(x, y, x0, bc-gh/2, x0, bc+gh/2, x0+tw, bc)
		}
		return false
	}
	arrows := func(x, y float64) bool {
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
	inside := func(x, y float64) bool {
		if !badge {
			return arrows(x, y)
		}
		d := math.Hypot(x-bc, y-bc)
		if d <= br {
			return !glyph(x, y)
		}
		return d > br+ring && arrows(x, y)
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
	return img
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	png.Encode(&buf, img)
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
