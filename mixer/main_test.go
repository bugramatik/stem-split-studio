package main

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func writeTestWAV(t *testing.T, path string, rate int, amp float64) {
	t.Helper()
	frames := rate / 2
	samples := [][]float64{make([]float64, frames)}
	for i := 0; i < frames; i++ {
		samples[0][i] = amp * math.Sin(2*math.Pi*440*float64(i)/float64(rate))
	}
	var buf bytes.Buffer
	if err := writeWAV(&buf, rate, 1, samples); err != nil {
		t.Fatalf("write test wav: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("save test wav: %v", err)
	}
}

func TestMixAppliesGain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vocals.wav")
	writeTestWAV(t, path, 8000, 0.25)

	quiet, err := mixStems(mixRequest{Stems: []stemSpec{{Name: "vocals", Path: path, GainDB: -6}}})
	if err != nil {
		t.Fatalf("mix: %v", err)
	}
	loud, err := mixStems(mixRequest{Stems: []stemSpec{{Name: "vocals", Path: path, GainDB: 0}}})
	if err != nil {
		t.Fatalf("mix: %v", err)
	}
	ratio := rmsDB(quiet) - rmsDB(loud)
	if math.Abs(ratio+6) > 0.2 {
		t.Fatalf("expected -6 dB from gain, got %.2f dB", ratio)
	}
}

func TestSoloOverridesMute(t *testing.T) {
	stems := []stemSpec{
		{Name: "vocals", Mute: true, Solo: true},
		{Name: "drums", Mute: false},
		{Name: "bass", Mute: true},
	}
	if !audible(stems, stems[0]) {
		t.Fatal("soloed stem should play even when muted")
	}
	if audible(stems, stems[1]) {
		t.Fatal("non-soloed stem should be silent while another is soloed")
	}
	noSolo := []stemSpec{{Name: "vocals", Mute: true}, {Name: "drums"}}
	if audible(noSolo, noSolo[0]) || !audible(noSolo, noSolo[1]) {
		t.Fatal("without solo, mute should decide")
	}
}

func TestNormaliseHitsTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "other.wav")
	writeTestWAV(t, path, 8000, 0.05)

	mixed, err := mixStems(mixRequest{Stems: []stemSpec{{Name: "other", Path: path}}})
	if err != nil {
		t.Fatalf("mix: %v", err)
	}
	before := rmsDB(mixed)
	delta := normalise(mixed, -14)
	after := rmsDB(mixed)
	if math.Abs(after-(-14)) > 0.5 {
		t.Fatalf("expected -14 dBFS after normalise, got %.2f", after)
	}
	if math.Abs((after-before)-delta) > 0.01 {
		t.Fatalf("reported change %.2f does not match measured %.2f", delta, after-before)
	}
}
