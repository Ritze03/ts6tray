package main

// alarm.go is the loud half of the alarm: notify.go decides *when* the watched
// user has arrived in a watched channel, and this plays the sound.
//
// The sound is synthesised rather than shipped: a two-tone siren, written once
// as a WAV into the cache dir, the same way the notification icons are
// unpacked. A notification server's sound hints are no use here — most servers
// ignore them, and the ones that honour them play a polite chime — so the file
// is handed to whichever PipeWire, PulseAudio or ALSA player is installed.

import (
	"bytes"
	"encoding/binary"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
)

const (
	alarmRate      = 44100 // samples per second, mono, 16-bit
	alarmHigh      = 1400  // Hz
	alarmLow       = 1000  // Hz
	alarmToneMs    = 180   // per tone
	alarmGapMs     = 40    // of silence after each high/low pair
	alarmCycles    = 6     // high/low pairs: about 2.4 s in all
	alarmFadeMs    = 4     // at each tone edge, against clicks
	alarmAmplitude = 0.25  // of full scale: a quarter, loud enough to notice without startling
)

// alarmPlayers are tried in order; the first one that is installed and plays
// the file wins. pw-play and paplay go through the sound server at full stream
// volume; aplay is the bare-ALSA fallback.
var alarmPlayers = [][]string{
	{"pw-play", "--volume", "1.0"},
	{"paplay", "--volume", "65536"},
	{"aplay", "-q"},
}

// alarmWAV renders the siren. Each tone is a sine with its third and fifth
// harmonics added, which reads as a buzzer rather than a beep and is far
// louder to the ear at the same peak level.
func alarmWAV() []byte {
	tone := alarmRate * alarmToneMs / 1000
	gap := alarmRate * alarmGapMs / 1000
	fade := alarmRate * alarmFadeMs / 1000
	n := alarmCycles * (2*tone + gap)

	// Scale to the waveform's measured peak, not the sum of the amplitudes:
	// the harmonics never crest together, and the sum would leave the siren
	// well short of full scale.
	wave := func(ph float64) float64 { return math.Sin(ph) + math.Sin(3*ph)/3 + math.Sin(5*ph)/5 }
	peak := 0.0
	for i := range 4096 {
		peak = max(peak, math.Abs(wave(2*math.Pi*float64(i)/4096)))
	}
	pcm := make([]int16, 0, n)
	for range alarmCycles {
		for _, f := range []float64{alarmHigh, alarmLow} {
			for i := range tone {
				ph := 2 * math.Pi * f * float64(i) / alarmRate
				v := wave(ph) / peak
				env := 1.0
				if i < fade {
					env = float64(i) / float64(fade)
				} else if i >= tone-fade {
					env = float64(tone-1-i) / float64(fade)
				}
				pcm = append(pcm, int16(v*env*alarmAmplitude*math.MaxInt16))
			}
		}
		pcm = pcm[:len(pcm)+gap] // silence: the reserved tail is already zero
	}

	// A canonical 44-byte header: RIFF, a 16-byte PCM fmt chunk, then data.
	le := binary.LittleEndian
	size := uint32(len(pcm) * 2)
	b := make([]byte, 0, 44+size)
	b = append(b, "RIFF"...)
	b = le.AppendUint32(b, 36+size)
	b = append(b, "WAVEfmt "...)
	b = le.AppendUint32(b, 16)          // fmt chunk size
	b = le.AppendUint16(b, 1)           // PCM
	b = le.AppendUint16(b, 1)           // mono
	b = le.AppendUint32(b, alarmRate)   // sample rate
	b = le.AppendUint32(b, alarmRate*2) // byte rate
	b = le.AppendUint16(b, 2)           // block align
	b = le.AppendUint16(b, 16)          // bits per sample
	b = append(b, "data"...)
	b = le.AppendUint32(b, size)
	for _, s := range pcm {
		b = le.AppendUint16(b, uint16(s))
	}
	return b
}

// alarmSoundFile writes the siren to $XDG_CACHE_HOME/ts6tray/alarm.wav once
// per process and returns its path. Like the icons, an unchanged file is left
// alone.
var alarmSoundFile = sync.OnceValues(func() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "ts6tray")
	path := filepath.Join(dir, "alarm.wav")
	wav := alarmWAV()
	if old, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(old, wav) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, wav, 0o644); err != nil {
		return "", err
	}
	return path, nil
})

// alarmBusy is set while the siren plays: a second arrival in the meantime is
// already being shouted about, and two overlapping sirens help nobody.
var alarmBusy atomic.Bool

// playAlarm sounds the siren in the background and returns at once.
func playAlarm() {
	if !alarmBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer alarmBusy.Store(false)
		path, err := alarmSoundFile()
		if err != nil {
			log.Printf("alarm: writing the sound: %v", err)
			return
		}
		for _, p := range alarmPlayers {
			bin, lerr := exec.LookPath(p[0])
			if lerr != nil {
				continue
			}
			args := append(append([]string{}, p[1:]...), path)
			if rerr := exec.Command(bin, args...).Run(); rerr != nil {
				log.Printf("alarm: %s: %v", p[0], rerr)
				continue
			}
			return
		}
		log.Printf("alarm: no sound player worked (tried pw-play, paplay, aplay)")
	}()
}
