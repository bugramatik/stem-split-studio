// Command stem-mixer sums separated stem WAV files with per-stem gain, mute
// and solo, then normalises the result to a target loudness.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"time"
)

type stemSpec struct {
	Name   string  `json:"name"`
	Path   string  `json:"path"`
	GainDB float64 `json:"gain_db"`
	Mute   bool    `json:"mute"`
	Solo   bool    `json:"solo"`
}

type mixRequest struct {
	Stems      []stemSpec `json:"stems"`
	TargetLUFS float64    `json:"target_lufs"`
}

type wavFile struct {
	SampleRate int
	Channels   int
	Samples    [][]float64 // one slice per channel
}

func readWAV(path string) (*wavFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, fmt.Errorf("%s is not a RIFF/WAVE file", path)
	}
	var (
		channels, bits, sampleRate int
		pcm                        []byte
	)
	pos := 12
	for pos+8 <= len(data) {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		body := data[pos+8:]
		if size > len(body) {
			size = len(body)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, errors.New("short fmt chunk")
			}
			channels = int(binary.LittleEndian.Uint16(body[2:4]))
			sampleRate = int(binary.LittleEndian.Uint32(body[4:8]))
			bits = int(binary.LittleEndian.Uint16(body[14:16]))
		case "data":
			pcm = body[:size]
		}
		pos += 8 + size + size%2
	}
	if channels <= 0 || sampleRate <= 0 {
		return nil, fmt.Errorf("%s has no usable fmt chunk", path)
	}
	if bits != 16 && bits != 32 {
		return nil, fmt.Errorf("%s uses %d-bit samples, only 16 and 32 are supported", path, bits)
	}
	bytesPerSample := bits / 8
	frames := len(pcm) / (bytesPerSample * channels)
	samples := make([][]float64, channels)
	for c := 0; c < channels; c++ {
		samples[c] = make([]float64, frames)
	}
	for f := 0; f < frames; f++ {
		for c := 0; c < channels; c++ {
			off := (f*channels + c) * bytesPerSample
			if bits == 16 {
				v := int16(binary.LittleEndian.Uint16(pcm[off : off+2]))
				samples[c][f] = float64(v) / 32768.0
			} else {
				v := int32(binary.LittleEndian.Uint32(pcm[off : off+4]))
				samples[c][f] = float64(v) / 2147483648.0
			}
		}
	}
	return &wavFile{SampleRate: sampleRate, Channels: channels, Samples: samples}, nil
}

func writeWAV(w io.Writer, rate, channels int, samples [][]float64) error {
	frames := 0
	if len(samples) > 0 {
		frames = len(samples[0])
	}
	dataSize := frames * channels * 2
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+dataSize))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(header[32:34], uint16(channels*2))
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataSize))
	if _, err := w.Write(header); err != nil {
		return err
	}
	buf := make([]byte, frames*channels*2)
	for f := 0; f < frames; f++ {
		for c := 0; c < channels; c++ {
			v := 0.0
			if c < len(samples) && f < len(samples[c]) {
				v = samples[c][f]
			}
			if v > 1 {
				v = 1
			} else if v < -1 {
				v = -1
			}
			off := (f*channels + c) * 2
			binary.LittleEndian.PutUint16(buf[off:off+2], uint16(int16(math.Round(v*32767))))
		}
	}
	_, err := w.Write(buf)
	return err
}

func dbToLinear(db float64) float64 { return math.Pow(10, db/20) }

// audible reports whether a stem plays, given the whole set: solo wins over mute.
func audible(stems []stemSpec, s stemSpec) bool {
	anySolo := false
	for _, other := range stems {
		if other.Solo {
			anySolo = true
			break
		}
	}
	if anySolo {
		return s.Solo
	}
	return !s.Mute
}

func mixStems(req mixRequest) (*wavFile, error) {
	if len(req.Stems) == 0 {
		return nil, errors.New("no stems in request")
	}
	var out *wavFile
	for _, s := range req.Stems {
		w, err := readWAV(s.Path)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = &wavFile{SampleRate: w.SampleRate, Channels: w.Channels}
			for c := 0; c < w.Channels; c++ {
				out.Samples = append(out.Samples, make([]float64, len(w.Samples[c])))
			}
		}
		if w.SampleRate != out.SampleRate || w.Channels != out.Channels {
			return nil, fmt.Errorf("stem %s has %d Hz / %d ch, expected %d Hz / %d ch",
				s.Name, w.SampleRate, w.Channels, out.SampleRate, out.Channels)
		}
		if !audible(req.Stems, s) {
			continue
		}
		gain := dbToLinear(s.GainDB)
		for c := 0; c < out.Channels; c++ {
			for i := range out.Samples[c] {
				out.Samples[c][i] += w.Samples[c][i] * gain
			}
		}
	}
	if out == nil {
		return nil, errors.New("no stem could be read")
	}
	return out, nil
}

// rmsDB returns the RMS level of the mix in dBFS.
func rmsDB(w *wavFile) float64 {
	var sum float64
	var n int
	for _, ch := range w.Samples {
		for _, v := range ch {
			sum += v * v
			n++
		}
	}
	if n == 0 || sum == 0 {
		return -120
	}
	return 20 * math.Log10(math.Sqrt(sum/float64(n)))
}

// normalise scales the mix so its RMS sits at the target loudness, capped at 0 dBFS.
func normalise(w *wavFile, targetLUFS float64) float64 {
	if targetLUFS == 0 {
		targetLUFS = -14
	}
	current := rmsDB(w)
	delta := targetLUFS - current
	if delta > 0 {
		peak := 0.0
		for _, ch := range w.Samples {
			for _, v := range ch {
				if a := math.Abs(v); a > peak {
					peak = a
				}
			}
		}
		if peak > 0 {
			if headroom := 20 * math.Log10(1/peak); delta > headroom {
				delta = headroom
			}
		}
	}
	factor := dbToLinear(delta)
	for _, ch := range w.Samples {
		for i := range ch {
			ch[i] *= factor
		}
	}
	return delta
}

func handleMix(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req mixRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	mixed, err := mixStems(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	delta := normalise(mixed, req.TargetLUFS)
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("X-Loudness-Change-DB", fmt.Sprintf("%.2f", delta))
	if err := writeWAV(w, mixed.SampleRate, mixed.Channels, mixed.Samples); err != nil {
		log.Printf("write response: %v", err)
	}
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/mix", handleMix)
	return mux
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8765", "address to listen on")
	flag.Parse()
	server := &http.Server{
		Addr:              *addr,
		Handler:           newMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("stem mixer listening on %s", *addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
