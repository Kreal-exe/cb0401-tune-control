package main

// Taught positions: where on the plan a moving person is, learned from this
// home's own examples.
//
// A pure geometric fix (Widar2.0: angle from the router's antennas plus the
// reflection's path length) needs the antenna layout inside the router,
// which isn't known, and 16 MHz of bandwidth gives path lengths only to
// several metres. So the direction information is learned instead: you
// stand somewhere, click that spot on the map, and walk slowly around it
// for a few seconds. Every tick with movement stores what the links saw -
// how strong each link's moving part is (near a link's line it is strong,
// far away weak) and, for 4-antenna links, the angle signature of the
// moving reflection. Live, the most similar stored moments vote for their
// spots, and the dot sits at the weighted middle of the winners.
//
// The spots also cross-check each other: each spot's later half is located
// using everything else; the share that lands nearest its own spot says how
// well this home's spots can be told apart.

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	teachDuration = 20 * time.Second
	knnK          = 12
	sigScale      = 45.0 // degrees of angle signature difference ~ one unit
	powScale      = 4.0  // dB of link strength difference ~ one unit
)

type teachSample struct {
	Score map[string]float64   `json:"score"` // link -> dB above its normal level
	Sig   map[string][]float64 `json:"sig,omitempty"`
	Q     map[string]float64   `json:"q,omitempty"`
}

type teachSpot struct {
	X       float64       `json:"x"`
	Y       float64       `json:"y"`
	Samples []teachSample `json:"samples"`
}

// Pos is the taught estimate of where the movement is.
type Pos struct {
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Intensity float64 `json:"intensity"` // 0..1
	Placed    bool    `json:"placed"`
	Spread    float64 `json:"spread"` // metres: how far apart the voting spots are
}

type teacher struct {
	path string

	mu       sync.Mutex
	spots    []teachSpot
	active   int // index of the spot being taught, -1 when idle
	until    time.Time
	pos      Pos
	lastTick time.Time
	check    float64 // cross-check score, -1 when not enough spots
}

func newTeacher(path string) *teacher {
	t := &teacher{path: path, active: -1, check: -1}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &t.spots)
	}
	t.check = t.crossCheck()
	return t
}

func (t *teacher) save() {
	b, _ := json.Marshal(t.spots)
	_ = os.MkdirAll(filepath.Dir(t.path), 0o755)
	tmp := t.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, t.path)
	}
}

func sampleOf(links []LinkState) (teachSample, bool) {
	s := teachSample{Score: map[string]float64{}, Sig: map[string][]float64{}, Q: map[string]float64{}}
	moving := false
	for _, l := range links {
		if l.Stale || l.Learning {
			continue
		}
		s.Score[l.MAC] = math.Min(l.Score, 25)
		if l.Motion {
			moving = true
		}
		if len(l.Sig) > 0 && l.SigQ > 0 {
			s.Sig[l.MAC] = l.Sig
			s.Q[l.MAC] = l.SigQ
		}
	}
	return s, moving
}

func angDiff(a, b float64) float64 {
	d := math.Mod(a-b+540, 360) - 180
	return d
}

// distance between two moments: link strengths, plus angle signatures where
// both have one (weighted by how clean the weaker of the two is).
func sampleDist(a, b teachSample) float64 {
	var d2 float64
	n := 0
	for mac, sa := range a.Score {
		sb, ok := b.Score[mac]
		if !ok {
			continue
		}
		x := (sa - sb) / powScale
		d2 += x * x
		n++
	}
	for mac, ga := range a.Sig {
		gb, ok := b.Sig[mac]
		if !ok || len(ga) != len(gb) {
			continue
		}
		q := math.Min(a.Q[mac], b.Q[mac])
		for i := range ga {
			x := angDiff(ga[i], gb[i]) / sigScale
			d2 += q * x * x
		}
		n++
	}
	if n == 0 {
		return math.Inf(1)
	}
	return math.Sqrt(d2 / float64(n))
}

type vote struct {
	d    float64
	spot int
}

// locate: weighted middle of the spots of the k most similar stored moments.
func locate(s teachSample, spots []teachSpot, skip func(spot, i int) bool) (x, y, spread float64, best int, ok bool) {
	best = -1
	var vs []vote
	for k, sp := range spots {
		for i, smp := range sp.Samples {
			if skip != nil && skip(k, i) {
				continue
			}
			if d := sampleDist(s, smp); !math.IsInf(d, 1) {
				vs = append(vs, vote{d, k})
			}
		}
	}
	if len(vs) == 0 {
		return 0, 0, 0, -1, false
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].d < vs[j].d })
	if len(vs) > knnK {
		vs = vs[:knnK]
	}
	w := map[int]float64{}
	var wSum float64
	for _, v := range vs {
		ww := 1 / (v.d + 0.25)
		w[v.spot] += ww
		wSum += ww
	}
	for k, ww := range w {
		x += ww * spots[k].X
		y += ww * spots[k].Y
		if best < 0 || ww > w[best] {
			best = k
		}
	}
	x, y = x/wSum, y/wSum
	for k, ww := range w {
		spread += ww * math.Hypot(spots[k].X-x, spots[k].Y-y)
	}
	return x, y, spread / wSum, best, true
}

// crossCheck: share of each spot's later half that is located nearest to
// its own spot when only the earlier halves are known. -1 with < 2 spots.
func (t *teacher) crossCheck() float64 {
	usable := 0
	for _, sp := range t.spots {
		if len(sp.Samples) >= 8 {
			usable++
		}
	}
	if usable < 2 {
		return -1
	}
	var hit, tot float64
	for k, sp := range t.spots {
		h := len(sp.Samples) / 2
		for i := h; i < len(sp.Samples); i++ {
			_, _, _, best, ok := locate(sp.Samples[i], t.spots, func(spot, j int) bool { return j >= len(t.spots[spot].Samples)/2 })
			if !ok {
				continue
			}
			tot++
			if best == k {
				hit++
			}
		}
	}
	if tot == 0 {
		return -1
	}
	return hit / tot
}

// tick: collect while teaching, otherwise place the dot.
func (t *teacher) tick(now time.Time, links []LinkState) Pos {
	t.mu.Lock()
	defer t.mu.Unlock()
	dt := tickEvery.Seconds()
	if !t.lastTick.IsZero() {
		dt = math.Min(1, now.Sub(t.lastTick).Seconds())
	}
	t.lastTick = now
	s, moving := sampleOf(links)

	if t.active >= 0 {
		if moving {
			t.spots[t.active].Samples = append(t.spots[t.active].Samples, s)
		}
		if now.After(t.until) {
			t.active = -1
			t.save()
			t.check = t.crossCheck()
		}
		t.pos.Intensity = 0
		t.pos.Placed = false
		return t.pos
	}

	target := 0.0
	if moving {
		if x, y, spread, _, ok := locate(s, t.spots, nil); ok {
			if !t.pos.Placed || t.pos.Intensity < 0.05 {
				t.pos.X, t.pos.Y = x, y
			} else {
				k := math.Min(1, dt/0.8)
				t.pos.X += (x - t.pos.X) * k
				t.pos.Y += (y - t.pos.Y) * k
			}
			t.pos.Spread = spread
			t.pos.Placed = true
			target = 1
		}
	}
	t.pos.Intensity += (target - t.pos.Intensity) * math.Min(1, dt/0.6)
	if t.pos.Intensity < 0.02 {
		t.pos.Intensity = 0
	}
	return t.pos
}

type teachStatus struct {
	Active  bool        `json:"active"`
	Left    int         `json:"left"` // seconds left while teaching
	Samples int         `json:"samples"`
	Spots   []spotBrief `json:"spots"`
	Check   float64     `json:"check"` // cross-check 0..1, -1 unknown
}

type spotBrief struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	N int     `json:"n"`
}

func (t *teacher) status() teachStatus {
	st := teachStatus{Active: t.active >= 0, Check: t.check, Spots: []spotBrief{}}
	if t.active >= 0 {
		st.Left = int(time.Until(t.until).Seconds() + 0.99)
		st.Samples = len(t.spots[t.active].Samples)
	}
	for _, sp := range t.spots {
		st.Spots = append(st.Spots, spotBrief{sp.X, sp.Y, len(sp.Samples)})
	}
	return st
}

func (t *teacher) handle(w http.ResponseWriter, r *http.Request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.Method == http.MethodPost {
		var body struct {
			Action string  `json:"action"` // start | stop | delete | clear
			X      float64 `json:"x"`
			Y      float64 `json:"y"`
			Index  int     `json:"index"`
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		if json.Unmarshal(b, &body) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch body.Action {
		case "start":
			t.spots = append(t.spots, teachSpot{X: body.X, Y: body.Y})
			t.active = len(t.spots) - 1
			t.until = time.Now().Add(teachDuration)
		case "stop":
			t.active = -1
		case "delete":
			if body.Index >= 0 && body.Index < len(t.spots) && t.active < 0 {
				t.spots = append(t.spots[:body.Index], t.spots[body.Index+1:]...)
			}
		case "clear":
			if t.active < 0 {
				t.spots = nil
			}
		default:
			http.Error(w, "unknown action", http.StatusBadRequest)
			return
		}
		if t.active < 0 {
			// drop spots that never saw movement
			kept := t.spots[:0]
			for _, sp := range t.spots {
				if len(sp.Samples) > 0 {
					kept = append(kept, sp)
				}
			}
			t.spots = kept
			t.save()
			t.check = t.crossCheck()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(t.status())
}
