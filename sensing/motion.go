package main

// Motion detection per link (router <-> one Wi-Fi client) from CFR, and the
// light dot on the floor plan.
//
// Each record is cleaned up first (csi.go: per-chain gain, the client's
// transmit states, random phase/timing, glitches). The metric is then the
// power of the moving part of the channel relative to the static channel
// over the last second. On recorded data the old amplitude-fluctuation
// metric read +7.5 dB in an empty room (the TV switching transmit states)
// and only +2.5 dB while someone walked; this one reads 0 dB and +12 dB.
//
// Each link gets its own baseline (20th percentile of the last 10 minutes)
// and the score is how many dB the current value is above it. Each link
// also gets its own threshold: at least -threshold dB, and at least twice
// its normal noise band (75th minus 20th percentile) once there are ~2
// minutes of history. Links whose threshold ends up above 8 dB are reported
// as noisy.
//
// Per link there is also the Doppler spectrum of the last half second: how
// fast the moving reflection's path gets longer or shorter (m/s), and for
// 4-chain links the angle signature of the moving reflection (see
// signature in csi.go) - the raw material for placing a person on the map.

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	metricWindow   = time.Second
	baselineWindow = 10 * time.Minute
	tickEvery      = 250 * time.Millisecond
	staleAfter     = 3 * time.Second
	minFrames      = 10 // per metric window
	dopplerWindow  = 500 * time.Millisecond
	warmupSamples  = 40  // ~10 s of ticks before a link reports a score
	noiseSamples   = 480 // ~2 min before a link's threshold adapts to its noise
)

type link struct {
	mac      string
	width    int // values per frame (chains x 52); a change resets the link
	chains   int
	dsp      *linkDSP
	frames   []csiFrame
	anchorTs uint64    // router clock of the record that set anchorT
	anchorT  time.Time // local estimate of when that record was captured
	lastTs   uint64
	lastSeen time.Time
	rssi     int8
	hist     []float64 // one metric value per tick, for the baseline
	histT    []time.Time
	metric   float64
	baseline float64
	thr      float64 // this link's own threshold on score
	score    float64
}

// LinkState is what the page gets for each link.
type LinkState struct {
	MAC       string  `json:"mac"`
	Rate      float64 `json:"rate"`      // frames/s
	Metric    float64 `json:"metric"`    // moving power relative to static
	Baseline  float64 `json:"baseline"`  // this link's normal level
	Score     float64 `json:"score"`     // dB above the baseline, smoothed
	Motion    bool    `json:"motion"`    // score above this link's threshold
	Threshold float64 `json:"threshold"` // this link's threshold on score
	Noisy     bool    `json:"noisy"`     // jittery link: threshold raised well above the default
	Learning  bool    `json:"learning"`  // not enough history for a baseline yet
	Stale     bool    `json:"stale"`     // no frames for a few seconds
	RSSI      int     `json:"rssi"`

	States  int       `json:"states"`            // transmit states seen (antennas/modes the client alternates)
	Doppler []float64 `json:"doppler,omitempty"` // dB above baseline per path speed in dopplerV (-2.5..2.5 m/s)
	Speed   float64   `json:"speed"`             // how fast the moving path length changes, m/s (0 when still)
	Toward  float64   `json:"toward"`            // -1..1: path mostly lengthening (-) or shortening (+); sign not yet verified
	Sig     []float64 `json:"sig,omitempty"`     // angle signature, degrees, chains 1..3 vs chain 0
	SigQ    float64   `json:"sig_q"`             // 0..1, how much of the movement is one clean reflection
}

// Dot is the light on the plan: where movement is, roughly, and how much.
type Dot struct {
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Intensity float64 `json:"intensity"` // 0..1
	Placed    bool    `json:"placed"`    // false when no moving link has both ends on the plan
}

type analyzer struct {
	mu        sync.Mutex
	links     map[string]*link
	threshold float64
	dot       Dot
	lastTick  time.Time
	span      map[string]time.Duration // observed time each dump file covers, per radio
}

func newAnalyzer(threshold float64) *analyzer {
	return &analyzer{links: map[string]*link{}, threshold: threshold}
}

// ingest adds a batch of dump files. cfr_test_app names each file after the
// time it started writing (cfr_dump_<radio>_YYYY_MM_DD_HH:MM:SS.bin), so a
// file's records are spread evenly between its start and the next file's
// start on the same radio; for the newest file, the span is estimated from
// the previous ones. (Assuming a fixed span per file overcounted the rate:
// the reader's restart makes each file cover a bit more than POLL_SECONDS.)
// These are router-clock times, used only relative to each other.
func (a *analyzer) ingest(files []dumpFile, rotation time.Duration) (records int) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.span == nil {
		a.span = map[string]time.Duration{}
	}
	starts := make([]time.Time, len(files))
	for i, f := range files {
		starts[i] = fileStart(f.name)
	}
	for i, f := range files {
		radio := radioOf(f.name)
		span := a.span[radio]
		if span <= 0 {
			span = rotation
		}
		if !starts[i].IsZero() {
			for j := i + 1; j < len(files); j++ {
				if radioOf(files[j].name) == radio && !starts[j].IsZero() {
					if d := starts[j].Sub(starts[i]); d > 0 && d < 10*rotation {
						span = d
						a.span[radio] = span
					}
					break
				}
			}
		}
		start := starts[i]
		if start.IsZero() {
			start = now.Add(-span)
		}
		recs := splitRecords(f.data)
		for k, rec := range recs {
			h, chains := toneCSI(rec)
			if h == nil {
				continue
			}
			est := start.Add(time.Duration(float64(span) * (float64(k) + 0.5) / float64(len(recs))))
			l := a.links[rec.mac]
			if l == nil {
				l = &link{mac: rec.mac}
				a.links[rec.mac] = l
			}
			if l.width != len(h) {
				*l = link{mac: rec.mac, width: len(h), chains: chains, dsp: newLinkDSP(chains, len(h))}
			}
			// The record's own capture time keeps the true spacing (records
			// come 2-10 ms apart, with gaps); it is tied to local time once,
			// and again whenever the router clock jumps (reboot).
			t := est
			if rec.ts != 0 {
				if l.anchorTs == 0 || rec.ts < l.lastTs || rec.ts-l.lastTs > 30e6 {
					l.anchorTs, l.anchorT = rec.ts, est
				}
				l.lastTs = rec.ts
				t = l.anchorT.Add(time.Duration(rec.ts-l.anchorTs) * time.Microsecond)
			}
			if n := len(l.frames); n > 0 && !t.After(l.frames[n-1].t) {
				t = l.frames[n-1].t.Add(time.Microsecond)
			}
			if fr := l.dsp.push(t, h); fr != nil {
				l.frames = append(l.frames, *fr)
			}
			l.lastSeen = now
			if rec.rssi != 0 {
				l.rssi = rec.rssi
			}
			records++
		}
	}
	return records
}

// fileStart parses the start time out of a cfr_dump_<radio>_<time>.bin name.
func fileStart(name string) time.Time {
	base := strings.TrimSuffix(name, ".bin")
	rest := strings.TrimPrefix(base, "cfr_dump_")
	i := strings.Index(rest, "_")
	if i < 0 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("2006_01_02_15:04:05", rest[i+1:], time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// frameRate: frames per second over the span the buffered frames cover.
func frameRate(frames []csiFrame) float64 {
	if len(frames) < 2 {
		return 0
	}
	span := frames[len(frames)-1].t.Sub(frames[0].t).Seconds()
	if span <= 0 {
		return 0
	}
	return float64(len(frames)-1) / span
}

func percentile(vals []float64, p float64) float64 {
	c := append([]float64(nil), vals...)
	sort.Float64s(c)
	return c[int(p*float64(len(c)-1))]
}

// tick updates every link's metric and score and the dot. plan supplies
// where the router and the devices are (devices without a position don't
// move the dot).
func (a *analyzer) tick(now time.Time, pl Plan) ([]LinkState, Dot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	dt := tickEvery.Seconds()
	if !a.lastTick.IsZero() {
		dt = math.Min(1, now.Sub(a.lastTick).Seconds())
	}
	a.lastTick = now

	var states []LinkState
	var wSum, xSum, ySum float64
	for mac, l := range a.links {
		// drop old frames / arrivals / history
		cut := 0
		if n := len(l.frames); n > 0 {
			newest := l.frames[n-1].t
			for cut < n && newest.Sub(l.frames[cut].t) > 2*metricWindow {
				cut++
			}
		}
		l.frames = l.frames[cut:]
		cut = 0
		for cut < len(l.histT) && now.Sub(l.histT[cut]) > baselineWindow {
			cut++
		}
		l.hist, l.histT = l.hist[cut:], l.histT[cut:]

		stale := now.Sub(l.lastSeen) > staleAfter
		if stale && now.Sub(l.lastSeen) > baselineWindow {
			delete(a.links, mac) // gone for good (left, or capture moved to another device)
			continue
		}
		// metric over the newest metricWindow of frames
		win := newest(l.frames, metricWindow)
		raw := 0.0
		if !stale && len(win) >= minFrames {
			l.metric = dynPower(win)
			l.hist = append(l.hist, l.metric)
			l.histT = append(l.histT, now)
			if len(l.hist) >= warmupSamples {
				l.baseline = percentile(l.hist, 0.2)
				if l.baseline > 0 && l.metric > 0 {
					raw = math.Max(0, dB(l.metric/l.baseline))
					// Adapt the threshold to the link's noise only once there
					// are ~2 minutes of history: someone walking during the
					// first minute otherwise reads as "this link is noisy".
					l.thr = a.threshold
					if len(l.hist) >= noiseSamples {
						band := dB(percentile(l.hist, 0.75) / l.baseline)
						l.thr = math.Max(a.threshold, 2*band)
					}
				}
			}
		}
		l.score += (raw - l.score) * math.Min(1, dt/0.75)

		thr := l.thr
		if thr <= 0 {
			thr = a.threshold
		}
		st := LinkState{
			MAC: mac, Rate: frameRate(l.frames), Metric: l.metric, Baseline: l.baseline,
			Score: l.score, Motion: !stale && len(l.hist) >= warmupSamples && l.score > thr,
			Threshold: thr, Noisy: thr > 8, Learning: len(l.hist) < warmupSamples,
			Stale: stale, RSSI: int(l.rssi), States: len(l.dsp.states),
		}
		if !stale && l.baseline > 0 {
			a.describeMovement(l, &st, win)
		}
		states = append(states, st)

		if st.Motion && pl.Router != nil {
			if p, ok := pl.Devices[mac]; ok {
				w := l.score / thr // how far past its own threshold
				wSum += w
				xSum += w * (pl.Router[0] + p[0]) / 2
				ySum += w * (pl.Router[1] + p[1]) / 2
			}
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].MAC < states[j].MAC })

	// The dot: glide toward the weighted middle of the moving links,
	// fade in/out with the total amount of movement.
	target := math.Min(1, wSum/2.5)
	if wSum > 0 {
		tx, ty := xSum/wSum, ySum/wSum
		if !a.dot.Placed || a.dot.Intensity < 0.05 {
			a.dot.X, a.dot.Y = tx, ty
		} else {
			k := math.Min(1, dt/1.2)
			a.dot.X += (tx - a.dot.X) * k
			a.dot.Y += (ty - a.dot.Y) * k
		}
		a.dot.Placed = true
	}
	a.dot.Intensity += (target - a.dot.Intensity) * math.Min(1, dt/0.8)
	if a.dot.Intensity < 0.01 {
		a.dot.Intensity = 0
	}
	return states, a.dot
}

func dB(x float64) float64 { return 10 * math.Log10(x) }

// newest: the frames within d of the latest one.
func newest(frames []csiFrame, d time.Duration) []csiFrame {
	if len(frames) == 0 {
		return nil
	}
	last := frames[len(frames)-1].t
	i := len(frames)
	for i > 0 && last.Sub(frames[i-1].t) <= d {
		i--
	}
	return frames[i:]
}

// describeMovement fills in the Doppler spectrum, speed and angle signature.
func (a *analyzer) describeMovement(l *link, st *LinkState, win []csiFrame) {
	lambda := 299792458 / 5.2e9
	if l.chains <= 2 {
		lambda = 299792458 / 2.44e9
	}
	P := doppler(newest(l.frames, dopplerWindow), lambda, st.Rate)
	st.Doppler = make([]float64, len(P))
	floor := percentile(P, 0.5)
	var wSum, vSum, sSum float64
	for i, p := range P {
		st.Doppler[i] = math.Round(10*dB(math.Max(p, 1e-12)/l.baseline)) / 10
		v := dopplerV[i]
		if math.Abs(v) < 0.3 {
			continue
		}
		if w := p - 2*floor; w > 0 {
			wSum += w
			vSum += w * math.Abs(v)
			sSum += w * math.Copysign(1, v)
		}
	}
	if st.Motion && wSum > 0 {
		st.Speed = math.Round(100*vSum/wSum) / 100
		st.Toward = math.Round(100*sSum/wSum) / 100
	}
	if st.Motion && l.chains >= 3 {
		groups := groupByState(win)
		best := -1
		for k, g := range groups {
			if best < 0 || len(g) > len(groups[best]) {
				best = k
			}
		}
		if best >= 0 && best < len(l.dsp.states) {
			sig, q := signature(groups[best], l.dsp.states[best], l.chains, l.dsp.tones)
			for i := range sig {
				sig[i] = math.Round(sig[i])
			}
			st.Sig, st.SigQ = sig, math.Round(100*q)/100
		}
	}
}
