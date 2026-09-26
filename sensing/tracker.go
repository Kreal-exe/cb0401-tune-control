package main

// Moving bodies on the plan: a particle filter over positions and
// velocities, driven by what every anchor link measures.
//
// Every link is router -> reflection off a person -> anchor. Its Doppler
// spectrum says how fast that path's length changes; for a person at x
// moving with velocity v the rate is v . (u_router + u_anchor), where u_*
// are unit vectors from the router and from the anchor to x (the model of
// Widar, MobiHoc'17, and WiSLAT). Four links in different directions pin
// the velocity down; integrated over time and checked against the walls
// (nobody walks through a drawn wall) that becomes a track. How strong each
// link's moving part is adds where the person can be at all: a link only
// feels movement close to its router-anchor line (sensitivity falls with
// the extra path length), and movement next to the router moves every link.
//
// Several bodies: some particles are re-seeded everywhere on each step, so
// a second person elsewhere gets its own cluster; the cloud's clusters are
// reported as separate bodies. Two people in the same part of the flat look
// like one to four links from one router - that isn't separable.

import (
	"math"
	"math/rand"
	"sort"
	"strconv"
	"time"
)

const (
	nParticles    = 1500
	reseedShare   = 0.08
	maxSpeed      = 2.0  // m/s, walking
	accelNoise    = 2.0  // m/s^2, how freely direction/speed change
	sensScale     = 0.9  // m of extra path length at which a link's sensitivity falls to 1/e
	powerSigma    = 0.12 // spread of the observed vs predicted share of moving power
	bodyMinShare  = 0.12 // a cluster needs this share of the weight to count as a body
	bodyCell      = 0.4  // m, clustering grid
	idleFadeAfter = 2 * time.Second
	forgetAfter   = 20 * time.Second
)

type particle struct{ x, y, vx, vy, w float64 }

// Body is one moving thing on the plan.
type Body struct {
	ID     int     `json:"id"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Share  float64 `json:"share"`  // of the particle weight: how sure
	Spread float64 `json:"spread"` // m, how scattered its particles are
	Speed  float64 `json:"speed"`  // m/s
}

// TrackState is what the page gets about tracking.
type TrackState struct {
	Bodies []Body     `json:"bodies"`
	Ready  bool       `json:"ready"`  // router and at least two anchors placed
	Reason string     `json:"reason"` // why not ready
	Moving float64    `json:"moving"` // 0..1 overall movement
	Bounds [4]float64 `json:"bounds,omitempty"`
}

type linkObs struct {
	a      [2]float64 // anchor position
	moving float64    // linear moving power above normal (0 = normal)
	dop    []float64  // excess Doppler power per dopplerV bin, max-normalised
	rmax   float64    // highest path rate this link can see without aliasing
	active bool
}

type tracker struct {
	rng      *rand.Rand
	ps       []particle
	lastT    time.Time
	lastMove time.Time
	nextID   int
	bodies   []Body
	sig      string // plan signature: reset when the plan changes
}

func newTracker() *tracker { return &tracker{rng: rand.New(rand.NewSource(1))} }

func planBoundsOf(pl Plan, pts [][2]float64) [4]float64 {
	b := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	add := func(x, y float64) {
		b[0], b[1] = math.Min(b[0], x), math.Min(b[1], y)
		b[2], b[3] = math.Max(b[2], x), math.Max(b[3], y)
	}
	for _, w := range pl.Walls {
		add(w[0], w[1])
		add(w[2], w[3])
	}
	if len(pl.Walls) < 3 {
		// no rooms drawn: the area around the router and anchors
		for _, p := range pts {
			add(p[0]-2, p[1]-2)
			add(p[0]+2, p[1]+2)
		}
		return b
	}
	return [4]float64{b[0] + 0.1, b[1] + 0.1, b[2] - 0.1, b[3] - 0.1}
}

func segCross(ax, ay, bx, by, cx, cy, dx, dy float64) bool {
	d := (bx-ax)*(dy-cy) - (by-ay)*(dx-cx)
	if d == 0 {
		return false
	}
	t := ((cx-ax)*(dy-cy) - (cy-ay)*(dx-cx)) / d
	u := ((cx-ax)*(by-ay) - (cy-ay)*(bx-ax)) / d
	return t >= 0 && t <= 1 && u >= 0 && u <= 1
}

func (t *tracker) spawn(b [4]float64) particle {
	a := t.rng.Float64() * 2 * math.Pi
	s := t.rng.Float64() * 1.0
	return particle{
		x: b[0] + t.rng.Float64()*(b[2]-b[0]), y: b[1] + t.rng.Float64()*(b[3]-b[1]),
		vx: s * math.Cos(a), vy: s * math.Sin(a), w: 1,
	}
}

// step advances the filter with one tick of link states.
func (t *tracker) step(now time.Time, pl Plan, links []LinkState) TrackState {
	st := TrackState{Bodies: []Body{}}
	if pl.Router == nil {
		st.Reason = "Place the router on the plan (Edit plan) to see moving bodies."
		t.ps = nil
		return st
	}
	R := *pl.Router
	var obs []linkObs
	pts := [][2]float64{R}
	var total float64
	for _, l := range links {
		p, ok := pl.Devices[l.MAC]
		if !ok || l.Stale || l.Learning {
			continue
		}
		pts = append(pts, p)
		o := linkObs{a: p, rmax: 2.5}
		lambda := 299792458 / 5.2e9
		if l.Chains <= 2 {
			lambda = 299792458 / 2.44e9
		}
		if l.Rate > 0 {
			o.rmax = math.Min(2.5, l.Rate*lambda/2)
		}
		o.moving = math.Max(0, math.Pow(10, l.Score/10)-1)
		total += o.moving
		o.active = l.Score > l.Threshold*0.5
		if len(l.Doppler) == len(dopplerV) {
			o.dop = make([]float64, len(dopplerV))
			var mx float64
			for i, d := range l.Doppler {
				e := math.Max(0, math.Pow(10, d/10)-2) // above ~3 dB: this speed is present
				o.dop[i] = e
				mx = math.Max(mx, e)
			}
			if mx > 0 {
				for i := range o.dop {
					o.dop[i] /= mx
				}
			} else {
				o.dop = nil
			}
		}
		obs = append(obs, o)
	}
	if len(obs) < 2 {
		st.Reason = "Place at least two anchors (the devices being captured) on the plan to see moving bodies."
		t.ps = nil
		return st
	}
	st.Ready = true
	b := planBoundsOf(pl, pts)
	st.Bounds = b

	sig := planSig(pl)
	if sig != t.sig || len(t.ps) != nParticles {
		t.sig = sig
		t.ps = make([]particle, nParticles)
		for i := range t.ps {
			t.ps[i] = t.spawn(b)
		}
	}
	dt := tickEvery.Seconds()
	if !t.lastT.IsZero() {
		dt = math.Max(0.05, math.Min(1, now.Sub(t.lastT).Seconds()))
	}
	t.lastT = now

	moving := false
	for _, l := range links {
		if l.Motion {
			moving = true
		}
	}
	if moving {
		t.lastMove = now
	}
	st.Moving = math.Min(1, total/10)
	if !moving {
		// a pause: keep the hypotheses where they are (slowing down), show
		// nothing; after a long quiet spell forget them - the next movement
		// may start anywhere
		for i := range t.ps {
			t.ps[i].vx *= 0.8
			t.ps[i].vy *= 0.8
		}
		if now.Sub(t.lastMove) > forgetAfter {
			for i := range t.ps {
				t.ps[i] = t.spawn(b)
			}
			t.bodies = nil
		}
		if now.Sub(t.lastMove) > idleFadeAfter {
			return st
		}
	}
	// predict: constant velocity with random acceleration; walls block
	sa := accelNoise * math.Sqrt(dt)
	for i := range t.ps {
		p := &t.ps[i]
		p.vx += t.rng.NormFloat64() * sa
		p.vy += t.rng.NormFloat64() * sa
		if s := math.Hypot(p.vx, p.vy); s > maxSpeed {
			p.vx, p.vy = p.vx*maxSpeed/s, p.vy*maxSpeed/s
		}
		nx, ny := p.x+p.vx*dt, p.y+p.vy*dt
		blocked := nx < b[0] || nx > b[2] || ny < b[1] || ny > b[3]
		for _, w := range pl.Walls {
			if blocked {
				break
			}
			blocked = segCross(p.x, p.y, nx, ny, w[0], w[1], w[2], w[3])
		}
		if blocked {
			p.vx, p.vy = -0.3*p.vx, -0.3*p.vy
		} else {
			p.x, p.y = nx, ny
		}
	}
	// a few fresh hypotheses everywhere, so a new or second body is found
	for k := 0; k < int(reseedShare*nParticles); k++ {
		t.ps[t.rng.Intn(nParticles)] = t.spawn(b)
	}

	// weight
	var wsum float64
	for i := range t.ps {
		p := &t.ps[i]
		lw := 0.0
		var fs, ms float64
		f := make([]float64, len(obs))
		for k, o := range obs {
			dR := math.Hypot(p.x-R[0], p.y-R[1])
			dA := math.Hypot(p.x-o.a[0], p.y-o.a[1])
			direct := math.Hypot(o.a[0]-R[0], o.a[1]-R[1])
			f[k] = math.Exp(-(dR + dA - direct) / sensScale)
			fs += f[k]
			ms += o.moving
			// Doppler: this particle's path-length rate on link k
			if o.dop != nil && o.active {
				ux, uy := 0.0, 0.0
				if dR > 0.05 {
					ux, uy = (p.x-R[0])/dR, (p.y-R[1])/dR
				}
				if dA > 0.05 {
					ux += (p.x - o.a[0]) / dA
					uy += (p.y - o.a[1]) / dA
				}
				r := p.vx*ux + p.vy*uy
				// aliasing: rates beyond what this link can sample fold back
				if o.rmax > 0 && math.Abs(r) > o.rmax {
					r = math.Mod(r+o.rmax, 2*o.rmax)
					if r < 0 {
						r += 2 * o.rmax
					}
					r -= o.rmax
				}
				// the sign convention isn't verified yet: use both signs
				lw += math.Log(0.15 + dopAt(o.dop, r) + dopAt(o.dop, -r))
			}
		}
		if ms > 0 && fs > 0 {
			// which links feel it, relative to each other...
			for k, o := range obs {
				d := o.moving/ms - f[k]/fs
				lw -= d * d / (2 * powerSigma * powerSigma)
			}
			// ...and somebody far from every link can't move any of them
			mf := 0.0
			for _, v := range f {
				mf = math.Max(mf, v)
			}
			lw += math.Min(1, ms/3) * math.Log(mf+0.02)
		}
		p.w = math.Exp(lw)
		wsum += p.w
	}
	if wsum <= 0 || math.IsNaN(wsum) {
		for i := range t.ps {
			t.ps[i] = t.spawn(b)
		}
		return st
	}
	for i := range t.ps {
		t.ps[i].w /= wsum
	}
	st.Bodies = t.cluster(b)
	t.resample()
	return st
}

func dopAt(d []float64, r float64) float64 {
	i := int(math.Round((r - dopplerV[0]) / 0.1))
	if i < 0 || i >= len(d) {
		return 0
	}
	return d[i]
}

func planSig(pl Plan) string {
	s := ""
	if pl.Router != nil {
		s += ftoa(pl.Router[0]) + "," + ftoa(pl.Router[1])
	}
	keys := make([]string, 0, len(pl.Devices))
	for k := range pl.Devices {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s += ";" + k + ftoa(pl.Devices[k][0]) + ftoa(pl.Devices[k][1])
	}
	for _, w := range pl.Walls {
		s += "|" + ftoa(w[0]) + ftoa(w[1]) + ftoa(w[2]) + ftoa(w[3])
	}
	return s
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

// resample: systematic, keeps the cloud from collapsing onto few particles.
func (t *tracker) resample() {
	n := len(t.ps)
	out := make([]particle, n)
	u := t.rng.Float64() / float64(n)
	c := t.ps[0].w
	i := 0
	for j := 0; j < n; j++ {
		for u > c && i < n-1 {
			i++
			c += t.ps[i].w
		}
		out[j] = t.ps[i]
		out[j].w = 1 / float64(n)
		u += 1 / float64(n)
	}
	t.ps = out
}

// cluster finds the weight's peaks on a coarse grid (smoothed), and turns
// each big enough one into a body, keeping IDs of bodies close to last time's.
func (t *tracker) cluster(b [4]float64) []Body {
	nx := int((b[2]-b[0])/bodyCell) + 1
	ny := int((b[3]-b[1])/bodyCell) + 1
	g := make([]float64, nx*ny)
	for _, p := range t.ps {
		i := int((p.x - b[0]) / bodyCell)
		j := int((p.y - b[1]) / bodyCell)
		if i >= 0 && i < nx && j >= 0 && j < ny {
			g[j*nx+i] += p.w
		}
	}
	s := make([]float64, nx*ny)
	for j := 0; j < ny; j++ {
		for i := 0; i < nx; i++ {
			var v float64
			for dj := -2; dj <= 2; dj++ {
				for di := -2; di <= 2; di++ {
					ii, jj := i+di, j+dj
					if ii >= 0 && ii < nx && jj >= 0 && jj < ny {
						v += g[jj*nx+ii] * math.Exp(-float64(di*di+dj*dj)/2)
					}
				}
			}
			s[j*nx+i] = v
		}
	}
	type peak struct {
		x, y float64
		v    float64
	}
	var peaks []peak
	for j := 0; j < ny; j++ {
		for i := 0; i < nx; i++ {
			v := s[j*nx+i]
			if v <= 0 {
				continue
			}
			isMax := true
			for dj := -3; dj <= 3 && isMax; dj++ {
				for di := -3; di <= 3; di++ {
					ii, jj := i+di, j+dj
					if (di != 0 || dj != 0) && ii >= 0 && ii < nx && jj >= 0 && jj < ny && s[jj*nx+ii] > v {
						isMax = false
						break
					}
				}
			}
			if isMax {
				peaks = append(peaks, peak{b[0] + (float64(i)+0.5)*bodyCell, b[1] + (float64(j)+0.5)*bodyCell, v})
			}
		}
	}
	sort.Slice(peaks, func(i, j int) bool { return peaks[i].v > peaks[j].v })
	var bodies []Body
	for _, pk := range peaks {
		if len(bodies) >= 3 {
			break
		}
		// refine: particles within 1.2 m of the peak
		var w, x, y, vx, vy float64
		for _, p := range t.ps {
			if math.Hypot(p.x-pk.x, p.y-pk.y) < 1.2 {
				w += p.w
				x += p.w * p.x
				y += p.w * p.y
				vx += p.w * p.vx
				vy += p.w * p.vy
			}
		}
		if w < bodyMinShare {
			continue
		}
		x, y = x/w, y/w
		var sp float64
		for _, p := range t.ps {
			if d := math.Hypot(p.x-pk.x, p.y-pk.y); d < 1.2 {
				sp += p.w * math.Hypot(p.x-x, p.y-y)
			}
		}
		dup := false
		for _, o := range bodies {
			if math.Hypot(o.X-x, o.Y-y) < 1.2 {
				dup = true
			}
		}
		if dup {
			continue
		}
		bodies = append(bodies, Body{X: round2(x), Y: round2(y), Share: round2(w), Spread: round2(sp / w), Speed: round2(math.Hypot(vx, vy) / w)})
	}
	// keep IDs: nearest previous body within 1.5 m
	used := map[int]bool{}
	for i := range bodies {
		best, bd := -1, 1.5
		for _, o := range t.bodies {
			if d := math.Hypot(o.X-bodies[i].X, o.Y-bodies[i].Y); d < bd && !used[o.ID] {
				best, bd = o.ID, d
			}
		}
		if best < 0 {
			t.nextID++
			best = t.nextID
		}
		used[best] = true
		bodies[i].ID = best
	}
	t.bodies = bodies
	return bodies
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
