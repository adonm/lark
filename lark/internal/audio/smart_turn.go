package audio

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/cmplx"
	"os"
	"path/filepath"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

const (
	SmartTurnModel  = "smart-turn-v3.2-cpu.onnx"
	smartTurnSHA256 = "2bb026316b14a660486a75b1733cd3fbab8c2fd0314dc9af7be49f8cca967e4f"

	smartTurnSampleRate = 16000
	smartTurnSeconds    = 8
	smartTurnSamples    = smartTurnSampleRate * smartTurnSeconds // 128000
	smartTurnNFFT       = 400
	smartTurnHop        = 160
	smartTurnBins       = smartTurnNFFT/2 + 1 // 201
	smartTurnMels       = 80
	smartTurnFrames     = smartTurnSamples / smartTurnHop // 800
	smartTurnPad        = smartTurnNFFT / 2               // 200
)

func CheckModel(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read Smart Turn v3.2 model; run scripts/setup-host.sh")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != smartTurnSHA256 {
		return errors.New("Smart Turn model does not match the pinned smart-turn-v3.2-cpu.onnx")
	}
	return nil
}

var (
	ortInitMu   sync.Mutex
	ortInitDone bool
	ortInitErr  error
)

func findORTLibrary() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "lib", "libonnxruntime.so")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
		p := filepath.Join(data, "lark", "lib", "libonnxruntime.so")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "libonnxruntime.so"
}

func ensureORT() error {
	ortInitMu.Lock()
	defer ortInitMu.Unlock()
	if ortInitDone {
		return ortInitErr
	}
	ort.SetSharedLibraryPath(findORTLibrary())
	ortInitErr = ort.InitializeEnvironment()
	ortInitDone = true
	return ortInitErr
}

type Engine struct {
	mu      sync.Mutex
	session *ort.DynamicAdvancedSession
	inName  string
	outName string
	inData  []float32
	outData []float32
	inTen   *ort.Tensor[float32]
	outTen  *ort.Tensor[float32]
}

// melScratch owns the large FFT/mel working set plus the model input. Feature
// extraction runs outside Engine.mu, so concurrent callers need separate
// scratch rather than serializing the whole mel pass behind ONNX.
type melScratch struct {
	mel whisperMel
	out []float32
}

var melScratchPool = sync.Pool{
	New: func() any {
		return &melScratch{mel: *newWhisperMel(), out: make([]float32, smartTurnMels*smartTurnFrames)}
	},
}

func Open(path string, threads int) (*Engine, error) {
	if err := CheckModel(path); err != nil {
		return nil, err
	}
	if err := ensureORT(); err != nil {
		return nil, fmt.Errorf("onnxruntime init: %w", err)
	}
	inputs, outputs, err := ort.GetInputOutputInfo(path)
	if err != nil {
		return nil, fmt.Errorf("smart-turn model info: %w", err)
	}
	if len(inputs) == 0 || len(outputs) == 0 {
		return nil, errors.New("smart-turn model has no inputs or outputs")
	}
	inName, outName := inputs[0].Name, outputs[0].Name

	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()
	if threads > 0 {
		_ = opts.SetIntraOpNumThreads(threads)
	}
	_ = opts.SetInterOpNumThreads(1)
	_ = opts.SetGraphOptimizationLevel(ort.GraphOptimizationLevelEnableAll)

	inData := make([]float32, smartTurnMels*smartTurnFrames)
	outData := make([]float32, 1)
	inTen, err := ort.NewTensor(ort.NewShape(1, smartTurnMels, smartTurnFrames), inData)
	if err != nil {
		return nil, err
	}
	outTen, err := ort.NewTensor(ort.NewShape(1, 1), outData)
	if err != nil {
		inTen.Destroy()
		return nil, err
	}
	sess, err := ort.NewDynamicAdvancedSession(path, []string{inName}, []string{outName}, opts)
	if err != nil {
		inTen.Destroy()
		outTen.Destroy()
		return nil, err
	}
	return &Engine{
		session: sess,
		inName:  inName,
		outName: outName,
		inData:  inData,
		outData: outData,
		inTen:   inTen,
		outTen:  outTen,
	}, nil
}

func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session != nil {
		_ = e.session.Destroy()
		e.session = nil
	}
	if e.inTen != nil {
		_ = e.inTen.Destroy()
		e.inTen = nil
	}
	if e.outTen != nil {
		_ = e.outTen.Destroy()
		e.outTen = nil
	}
}

func (e *Engine) PredictPCM16(pcm []byte) (float32, error) {
	scratch := melScratchPool.Get().(*melScratch)
	// The expensive mel/FFT pass is independent of the ONNX session and runs
	// before the lock so a slow inference does not serialize feature work.
	scratch.mel.extractPCM16(pcm, scratch.out)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session == nil {
		melScratchPool.Put(scratch)
		return 0, errors.New("smart-turn closed")
	}
	copy(e.inData, scratch.out)
	melScratchPool.Put(scratch)
	if err := e.session.Run([]ort.Value{e.inTen}, []ort.Value{e.outTen}); err != nil {
		return 0, err
	}
	return e.outData[0], nil
}

// whisperMel computes Whisper-style 80-bin log-mel spectrograms for 8 s of 16 kHz audio
// with zero heap allocations per call.
type whisperMel struct {
	wave    [smartTurnSamples]float64
	padded  [smartTurnSamples + 2*smartTurnPad]float64
	hann    [smartTurnNFFT]float64
	melBank [smartTurnMels * smartTurnBins]float64
	frame   [smartTurnNFFT]float64
	power   [smartTurnBins]float64
	tw400   [smartTurnNFFT]complex128
	tw25    [25][25]complex128
	fft16   [16]complex128
	grid    [25][16]complex128
}

func newWhisperMel() *whisperMel {
	m := &whisperMel{}
	for i := range smartTurnNFFT {
		m.hann[i] = 0.5 * (1.0 - math.Cos(2.0*math.Pi*float64(i)/float64(smartTurnNFFT)))
	}
	initSlaneyMelBank(m.melBank[:], smartTurnMels, smartTurnNFFT, smartTurnSampleRate, 0, 8000)
	for k := range smartTurnNFFT {
		ang := -2.0 * math.Pi * float64(k) / float64(smartTurnNFFT)
		m.tw400[k] = cmplx.Rect(1, ang)
	}
	for k := range 25 {
		for n := range 25 {
			ang := -2.0 * math.Pi * float64(k*n) / 25.0
			m.tw25[k][n] = cmplx.Rect(1, ang)
		}
	}
	return m
}

func hzToMelSlaney(hz float64) float64 {
	const (
		minLogHz  = 1000.0
		minLogMel = 15.0                // 1000 * 3 / 200
		logStep   = 0.06875177742094912 // ln(6.4) / 27
	)
	if hz >= minLogHz {
		return minLogMel + math.Log(hz/minLogHz)/logStep
	}
	return 3.0 * hz / 200.0
}

func melToHzSlaney(mel float64) float64 {
	const (
		minLogHz  = 1000.0
		minLogMel = 15.0
		logStep   = 0.06875177742094912
	)
	if mel >= minLogMel {
		return minLogHz * math.Exp(logStep*(mel-minLogMel))
	}
	return 200.0 * mel / 3.0
}

func initSlaneyMelBank(out []float64, nMels, nFFT, SampleRate int, fMin, fMax float64) {
	nBins := nFFT/2 + 1
	melMin := hzToMelSlaney(fMin)
	melMax := hzToMelSlaney(fMax)
	pts := make([]float64, nMels+2)
	for i := range pts {
		m := melMin + (melMax-melMin)*float64(i)/float64(nMels+1)
		pts[i] = melToHzSlaney(m)
	}
	fftFreqs := make([]float64, nBins)
	for i := range fftFreqs {
		fftFreqs[i] = float64(i) * float64(SampleRate) / float64(nFFT)
	}
	for m := range nMels {
		fLeft, fCenter, fRight := pts[m], pts[m+1], pts[m+2]
		enorm := 2.0 / (fRight - fLeft)
		row := out[m*nBins : (m+1)*nBins]
		for k := range nBins {
			f := fftFreqs[k]
			var w float64
			if f >= fLeft && f <= fCenter {
				w = (f - fLeft) / (fCenter - fLeft)
			} else if f > fCenter && f <= fRight {
				w = (fRight - f) / (fRight - fCenter)
			}
			if w > 0 {
				row[k] = w * enorm
			}
		}
	}
}

func (m *whisperMel) extractPCM16(pcm []byte, out []float32) {
	// Keep the last <= 8s (128000 samples), left-padded with zeros.
	nAvail := len(pcm) / 2
	if nAvail > smartTurnSamples {
		pcm = pcm[len(pcm)-smartTurnSamples*2:]
		nAvail = smartTurnSamples
	}
	padSamples := smartTurnSamples - nAvail
	clear(m.wave[:padSamples])
	for i := range nAvail {
		s := int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8)
		m.wave[padSamples+i] = float64(s) / 32768.0
	}

	// Zero-mean, unit-variance normalization across the 128000-sample window
	// (matches WhisperFeatureExtractor(..., do_normalize=True)).
	var sum float64
	for _, v := range m.wave {
		sum += v
	}
	mean := sum / float64(smartTurnSamples)
	var sq float64
	for _, v := range m.wave {
		d := v - mean
		sq += d * d
	}
	invStd := 1.0 / math.Sqrt(sq/float64(smartTurnSamples)+1e-7)
	for i, v := range m.wave {
		m.wave[i] = (v - mean) * invStd
	}

	// Reflect-pad 200 samples on each side (center=True STFT).
	copy(m.padded[smartTurnPad:], m.wave[:])
	for i := range smartTurnPad {
		m.padded[smartTurnPad-1-i] = m.wave[i+1]
		m.padded[smartTurnPad+smartTurnSamples+i] = m.wave[smartTurnSamples-2-i]
	}

	maxLog := -math.MaxFloat64
	// Compute 800 frames of 80 mel bins.
	for t := range smartTurnFrames {
		start := t * smartTurnHop
		for i := range smartTurnNFFT {
			m.frame[i] = m.padded[start+i] * m.hann[i]
		}
		m.fft400Power()
		for mel := range smartTurnMels {
			row := m.melBank[mel*smartTurnBins : (mel+1)*smartTurnBins]
			var energy float64
			for k := range smartTurnBins {
				energy += row[k] * m.power[k]
			}
			if energy < 1e-10 {
				energy = 1e-10
			}
			lg := math.Log10(energy)
			if lg > maxLog {
				maxLog = lg
			}
			out[mel*smartTurnFrames+t] = float32(lg)
		}
	}

	floor := float32(maxLog - 8.0)
	for i, v := range out {
		if v < floor {
			v = floor
		}
		out[i] = (v + 4.0) * 0.25
	}
}

// fft400Power computes |DFT(m.frame)|^2 for bins 0..200 using 16x25 Cooley-Tukey FFT.
func (m *whisperMel) fft400Power() {
	// N = 400 = N1 * N2 with N1 = 16, N2 = 25.
	// Step 1: 16-point DFT along n1 (strided by 25).
	for n2 := range 25 {
		for n1 := range 16 {
			m.fft16[n1] = complex(m.frame[25*n1+n2], 0)
		}
		fft16InPlace(&m.fft16)
		for k1 := range 16 {
			// Multiply by twiddle W_400^(k1 * n2)
			tw := m.tw400[(k1*n2)%400]
			m.grid[n2][k1] = m.fft16[k1] * tw
		}
	}
	// Step 2: 25-point DFT along n2 for each k1.
	// Output index k = k1 + 16*k2. We only need k in 0..200.
	for k1 := range 16 {
		for k2 := range 25 {
			k := k1 + 16*k2
			if k >= smartTurnBins {
				continue
			}
			var acc complex128
			twRow := &m.tw25[k2]
			for n2 := range 25 {
				acc += m.grid[n2][k1] * twRow[n2]
			}
			re, im := real(acc), imag(acc)
			m.power[k] = re*re + im*im
		}
	}
}

func fft16InPlace(x *[16]complex128) {
	// Bit-reversed permutation for 16 elements.
	perm := [16]int{0, 8, 4, 12, 2, 10, 6, 14, 1, 9, 5, 13, 3, 11, 7, 15}
	var tmp [16]complex128
	for i, p := range perm {
		tmp[i] = x[p]
	}
	*x = tmp
	for size := 2; size <= 16; size <<= 1 {
		half := size >> 1
		step := -2.0 * math.Pi / float64(size)
		for start := 0; start < 16; start += size {
			for k := range half {
				w := cmplx.Rect(1, step*float64(k))
				t := w * x[start+k+half]
				u := x[start+k]
				x[start+k] = u + t
				x[start+k+half] = u - t
			}
		}
	}
}
