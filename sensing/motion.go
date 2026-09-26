package main

// Motion detection per link (router <-> one Wi-Fi client) from CFR tone
// amplitudes, and the light dot on the floor plan.
//
// The metric is the one validated against a real walk on 2026-09-26: over
// the last second, how much each tone's amplitude fluctuates relative to
// its level (sum of per-tone standard deviations / sum of per-tone means).
// With nobody moving it sat around 0.040 (0.026-0.052) on the test link;
// walking between the router and the device pushed it to 0.06-0.16 for as
// long as the walk lasted. People moving change the paths the signal
// takes; a still room doesn't.
//
// Each link gets its own baseline (20th percentile of the last 10 minutes),
// because links differ a lot in level and noise; the score is how far the
// current value is above it (0 = normal, 1 = double). Each link also gets
// its own threshold: at least -threshold, and at least twice its normal
// noise band (75th minus 20th percentile, relative to the baseline). A
// quiet, steady link keeps the plain 40%; a jittery one - a device in power
// save, a weak signal - needs a proportionally bigger swing, so it stops
// crying wolf (the smart bulb's link was flagged as "motion" 108 of 121
// times with a fixed 40%, confirmed live). Links whose threshold ends up
// above 150% are reported as noisy.
//
// What this can't do, stated plainly: tell where along the link someone
// is, or see a person standing perfectly still for long. The dot on the
// plan is placed at the middle of the links that currently see movement,
// weighted by how strongly - "somewhere around here", not a coordinate.

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
	minFrames      = 10  // per metric window
	warmupSamples  = 40  // ~10 s of ticks before a link reports a score
	noiseSamples   = 480 // ~2 min before a link's threshold adapts to its noise
)

type frame struct {
	t   time.Time
	amp []float64
}

type link struct {
	mac      string
	width    int // values per frame (chains x 52); a change resets the link
	frames   []frame
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
	Metric    float64 `json:"metric"`    // current fluctuation
	Baseline  float64 `json:"baseline"`  // this link's normal level
	Score     float64 `json:"score"`     // metric/baseline - 1, smoothed
	Motion    bool    `json:"motion"`    // score above this link's threshold
	Threshold float64 `json:"threshold"` // this link's threshold on score
	Noisy     bool    `json:"noisy"`     // jittery link: threshold raised well above the default
	Learning  bool    `json:"learning"`  // not enough history for a baseline yet
	Stale     bool    `json:"stale"`     // no frames for a few seconds
	RSSI      int     `json:"rssi"`
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
			amp := toneAmplitudes(rec)
			if amp == nil {
				continue
			}
			t := start.Add(time.Duration(float64(span) * (float64(k) + 0.5) / float64(len(recs))))
			l := a.links[rec.mac]
			if l == nil {
				l = &link{mac: rec.mac}
				a.links[rec.mac] = l
			}
			if l.width != len(amp) {
				*l = link{mac: rec.mac, width: len(amp)}
			}
			if n := len(l.frames); n > 0 && !t.After(l.frames[n-1].t) {
				t = l.frames[n-1].t.Add(time.Millisecond) // keep order if clocks/spans overlap
			}
			l.frames = append(l.frames, frame{t: t, amp: amp})
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

// fluctuation: sum of per-tone standard deviations over sum of per-tone
// means, across the frames given.
func fluctuation(frames []frame) float64 {
	n := len(frames[0].amp)
	var sumStd, sumMean float64
	for k := 0; k < n; k++ {
		var s, s2 float64
		for _, f := range frames {
			v := f.amp[k]
			s += v
			s2 += v * v
		}
		m := s / float64(len(frames))
		v := s2/float64(len(frames)) - m*m
		if v < 0 {
			v = 0
		}
		sumStd += math.Sqrt(v)
		sumMean += m
	}
	if sumMean <= 0 {
		return 0
	}
	return sumStd / sumMean
}

// frameRate: frames per second over the span the buffered frames cover.
func frameRate(frames []frame) float64 {
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
		var win []frame
		if len(l.frames) > 0 {
			newest := l.frames[len(l.frames)-1].t
			for _, f := range l.frames {
				if newest.Sub(f.t) <= metricWindow {
					win = append(win, f)
				}
			}
		}
		raw := 0.0
		if !stale && len(win) >= minFrames {
			l.metric = fluctuation(win)
			l.hist = append(l.hist, l.metric)
			l.histT = append(l.histT, now)
			if len(l.hist) >= warmupSamples {
				l.baseline = percentile(l.hist, 0.2)
				if l.baseline > 0 {
					raw = math.Max(0, l.metric/l.baseline-1)
					// Adapt the threshold to the link's noise only once there
					// are ~2 minutes of history: someone walking during the
					// first minute otherwise reads as "this link is noisy".
					l.thr = a.threshold
					if len(l.hist) >= noiseSamples {
						band := (percentile(l.hist, 0.75) - l.baseline) / l.baseline
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
			Threshold: thr, Noisy: thr > 1.5, Learning: len(l.hist) < warmupSamples,
			Stale: stale, RSSI: int(l.rssi),
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
