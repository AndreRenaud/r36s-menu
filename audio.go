package main

// The menu's one sound, a gentle tick for moving through the list, is made
// here rather than shipped as a file.

import (
	"math"

	"github.com/hajimehoshi/ebiten/v2/audio"
)

const (
	sampleRate = 48000
	tickGain   = 0.6 // on top of the unit's volume
)

type sounds struct {
	ctx  *audio.Context
	tick []byte
}

func newSounds() *sounds {
	return &sounds{ctx: audio.NewContext(sampleRate), tick: makeTick()}
}

// Tick plays the list-movement sound.
func (s *sounds) Tick() {
	p := s.ctx.NewPlayerFromBytes(s.tick)
	p.SetVolume(tickGain)
	p.Play()
}

// makeTick is a short, soft, falling blip (16-bit stereo).
func makeTick() []byte {
	n := sampleRate * 45 / 1000
	pcm := make([]byte, 4*n)
	phase := 0.0
	for i := 0; i < n; i++ {
		t := float64(i) / sampleRate
		freq := 1500 - 14000*t
		phase += 2 * math.Pi * freq / sampleRate
		env := math.Exp(-t*90) * math.Min(1, t*2000)
		v := int16(math.Sin(phase) * env * 0.5 * 32767)
		pcm[4*i], pcm[4*i+1] = byte(v), byte(v>>8)
		pcm[4*i+2], pcm[4*i+3] = byte(v), byte(v>>8)
	}
	return pcm
}
