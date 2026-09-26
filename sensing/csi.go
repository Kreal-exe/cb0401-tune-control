package main

// Turning the raw CFR snapshots of one link into a clean series that can
// be compared record to record - the preprocessing Widar2.0-style tracking
// needs, adapted to what this router's captures actually do (measured on
// recorded data, 2026-09-26/27):
//
//  1. Gain: each receive chain's level steps by 0.5-1 dB between records
//     (AGC). Movement changes the shape across tones, not a chain's overall
//     level, so every chain is divided by its mean tone amplitude.
//  2. Transmit states: a client may alternate between transmit antennas or
//     modes. The TV switches about every 1.2 s, and its channel then differs
//     by up to 14 dB and 100+ degrees per tone - as one series that looked
//     like violent movement in an empty room, and it drowned real walking.
//     Each record is sorted into the state it matches (by inter-chain
//     products, which don't depend on the snapshot's random phase) and every
//     state is processed on its own.
//  3. Phase: every snapshot has a random common phase and timing offset (a
//     phase slope across tones). Both are fitted against the state's static
//     channel and removed, so the static channel becomes the phase reference
//     - the role of the boosted reference antenna in Widar2.0 - and all
//     chains stay usable for angle estimation.
//  4. Glitches: a single record where one chain's gain changed mid-capture
//     looks nothing like its neighbours; such isolated records are dropped.

import (
	"math"
	"math/cmplx"
	"time"
)

const (
	maxTxStates   = 4
	stateMatch    = 0.9    // similarity to join an existing transmit state
	tmplRate      = 0.02   // how fast a state's signature follows the channel
	staticRate    = 0.01   // alignment reference: ~0.4 s at 230 frames/s
	slowRate      = 0.0005 // angle reference: ~10 s at 230 frames/s
	glitchFactor  = 3.0    // an isolated record this much further from both neighbours...
	glitchMinDist = 0.08   // ...and at least this far (relative) is dropped
)

type csiFrame struct {
	t     time.Time
	state int
	y     []complex128 // chains x 52, gain-normalised and phase-aligned
}

type txState struct {
	tmpl    []complex128 // unit-norm inter-chain products
	static  []complex128 // recent static channel, the phase reference
	slow    []complex128 // long-term static channel, the angle reference
	lastUse time.Time
}

type linkDSP struct {
	chains, tones int
	states        []*txState
	prev, pend    *csiFrame
	dropped       int
}

func newLinkDSP(chains, width int) *linkDSP {
	return &linkDSP{chains: chains, tones: width / chains}
}

func vnorm(v []complex128) float64 {
	var s float64
	for _, x := range v {
		s += real(x)*real(x) + imag(x)*imag(x)
	}
	return math.Sqrt(s)
}

func vdist(a, b []complex128) float64 {
	var s float64
	for i := range a {
		d := a[i] - b[i]
		s += real(d)*real(d) + imag(d)*imag(d)
	}
	return math.Sqrt(s)
}

// push takes one record's raw channel and returns the frame that is now
// final (the previous one, once its neighbour is known), or nil.
func (d *linkDSP) push(t time.Time, h []complex128) *csiFrame {
	C, F := d.chains, d.tones
	hn := make([]complex128, len(h))
	for c := 0; c < C; c++ {
		var m float64
		for f := 0; f < F; f++ {
			m += cmplx.Abs(h[c*F+f])
		}
		m /= float64(F)
		if m < 1 {
			return nil // a dead chain: not a usable capture
		}
		for f := 0; f < F; f++ {
			hn[c*F+f] = h[c*F+f] / complex(m, 0)
		}
	}

	// transmit state
	p := make([]complex128, len(hn))
	for c := 0; c < C; c++ {
		for f := 0; f < F; f++ {
			p[c*F+f] = hn[c*F+f] * cmplx.Conj(hn[f])
		}
	}
	pn := vnorm(p)
	for i := range p {
		p[i] /= complex(pn, 0)
	}
	best, bestSim := -1, 0.0
	for k, s := range d.states {
		var dot complex128
		for i := range p {
			dot += cmplx.Conj(s.tmpl[i]) * p[i]
		}
		if a := cmplx.Abs(dot); a > bestSim {
			best, bestSim = k, a
		}
	}
	if best < 0 || bestSim < stateMatch {
		st := &txState{tmpl: p, static: append([]complex128(nil), hn...), slow: append([]complex128(nil), hn...)}
		if len(d.states) < maxTxStates {
			d.states = append(d.states, st)
			best = len(d.states) - 1
		} else {
			best = 0
			for k, s := range d.states {
				if s.lastUse.Before(d.states[best].lastUse) {
					best = k
				}
			}
			d.states[best] = st
			d.prev, d.pend = nil, nil // frames of the replaced state are meaningless now
		}
	} else {
		s := d.states[best]
		for i := range p {
			s.tmpl[i] = complex(1-tmplRate, 0)*s.tmpl[i] + complex(tmplRate, 0)*p[i]
		}
		n := vnorm(s.tmpl)
		for i := range s.tmpl {
			s.tmpl[i] /= complex(n, 0)
		}
	}
	st := d.states[best]
	st.lastUse = t

	// common phase + timing slope against the state's static channel
	y := alignTo(hn, st.static, C, F)
	f := &csiFrame{t: t, state: best, y: y}

	// glitch check on the pending frame, now that its successor is known
	out := d.pend
	if d.pend != nil && d.prev != nil && d.prev.state == d.pend.state && d.pend.state == f.state {
		dpp, dpf, dff := vdist(d.pend.y, d.prev.y), vdist(d.pend.y, f.y), vdist(f.y, d.prev.y)
		lim := math.Max(glitchFactor*dff, glitchMinDist*vnorm(f.y))
		if dpp > lim && dpf > lim {
			out = nil
			d.dropped++
		}
	}
	if out != nil {
		s := d.states[out.state]
		for i := range s.static {
			s.static[i] = complex(1-staticRate, 0)*s.static[i] + complex(staticRate, 0)*out.y[i]
			s.slow[i] = complex(1-slowRate, 0)*s.slow[i] + complex(slowRate, 0)*out.y[i]
		}
		d.prev = out
	}
	d.pend = f
	return out
}

// alignTo removes the common phase a and timing slope b (phase a + b*tone,
// the same on every chain) that best line h up with the reference ref.
func alignTo(h, ref []complex128, C, F int) []complex128 {
	z := make([]complex128, F)
	for c := 0; c < C; c++ {
		for f := 0; f < F; f++ {
			z[f] += h[c*F+f] * cmplx.Conj(ref[c*F+f])
		}
	}
	// unwrap and weighted least squares on phase(z) = a + b*x
	var sw, sx, sy, sxx, sxy, prev, off float64
	for f := 0; f < F; f++ {
		ph := cmplx.Phase(z[f])
		if f > 0 {
			for ph+off-prev > math.Pi {
				off -= 2 * math.Pi
			}
			for ph+off-prev < -math.Pi {
				off += 2 * math.Pi
			}
		}
		ph += off
		prev = ph
		w := cmplx.Abs(z[f])
		x := float64(f) - float64(F-1)/2
		sw += w
		sx += w * x
		sy += w * ph
		sxx += w * x * x
		sxy += w * x * ph
	}
	var a, b float64
	if den := sw*sxx - sx*sx; sw > 0 && den != 0 {
		b = (sw*sxy - sx*sy) / den
		a = (sy - b*sx) / sw
	}
	y := make([]complex128, len(h))
	for f := 0; f < F; f++ {
		x := float64(f) - float64(F-1)/2
		r := cmplx.Exp(complex(0, -(a + b*x)))
		for c := 0; c < C; c++ {
			y[c*F+f] = h[c*F+f] * r
		}
	}
	return y
}

// groupByState splits frames by transmit state, keeping order.
func groupByState(frames []csiFrame) map[int][]csiFrame {
	g := map[int][]csiFrame{}
	for _, f := range frames {
		g[f.state] = append(g[f.state], f)
	}
	return g
}

// dynamics returns, for frames of one state, each frame's moving part: the
// frame minus the window's mean, with anything shaped like the mean itself
// (leftover common gain/phase jitter) projected out. Also the mean's power.
func dynamics(frames []csiFrame) ([][]complex128, float64) {
	n := len(frames)
	w := len(frames[0].y)
	mean := make([]complex128, w)
	for _, f := range frames {
		for i, v := range f.y {
			mean[i] += v
		}
	}
	for i := range mean {
		mean[i] /= complex(float64(n), 0)
	}
	sp := vnorm(mean)
	sp *= sp
	out := make([][]complex128, n)
	for k, f := range frames {
		d := make([]complex128, w)
		var c complex128
		for i, v := range f.y {
			d[i] = v - mean[i]
			c += cmplx.Conj(mean[i]) * d[i]
		}
		if sp > 0 {
			c /= complex(sp, 0)
			for i := range d {
				d[i] -= c * mean[i]
			}
		}
		out[k] = d
	}
	return out, sp
}

// dynPower: power of the moving part relative to the static channel, per
// frame, averaged over the transmit states present in the window.
func dynPower(frames []csiFrame) float64 {
	var num, cnt float64
	for _, g := range groupByState(frames) {
		if len(g) < minFrames {
			continue
		}
		D, sp := dynamics(g)
		if sp <= 0 {
			continue
		}
		var e float64
		for _, d := range D {
			n := vnorm(d)
			e += n * n
		}
		num += e / sp
		cnt += float64(len(g))
	}
	if cnt == 0 {
		return 0
	}
	return num / cnt
}

// Doppler: how fast the length of the moving reflection's path changes,
// from the phase rotation of the moving part over half a second. The
// spectrum is evaluated straight on the irregular capture times (there are
// gaps of ~90 ms every ~1.2 s, and a transmit state is only present part of
// the time), with Hann weights; a sample next to a gap may not dominate.
var dopplerV = func() []float64 {
	v := make([]float64, 0, 51)
	for i := -25; i <= 25; i++ {
		v = append(v, float64(i)*0.1)
	}
	return v
}()

func doppler(frames []csiFrame, lambda float64, rate float64) []float64 {
	P := make([]float64, len(dopplerV))
	if len(frames) < 2 || rate <= 0 {
		return P
	}
	t0 := frames[0].t
	span := frames[len(frames)-1].t.Sub(t0).Seconds()
	if span <= 0 {
		return P
	}
	for _, g := range groupByState(frames) {
		if len(g) < minFrames {
			continue
		}
		D, sp := dynamics(g)
		if sp <= 0 {
			continue
		}
		n := len(g)
		ts := make([]float64, n)
		for k, f := range g {
			ts[k] = f.t.Sub(t0).Seconds()
		}
		wts := make([]float64, n)
		for k := range g {
			var dt float64
			switch {
			case n == 1:
				dt = 1 / rate
			case k == 0:
				dt = ts[1] - ts[0]
			case k == n-1:
				dt = ts[n-1] - ts[n-2]
			default:
				dt = (ts[k+1] - ts[k-1]) / 2
			}
			dt = math.Min(dt, 2/rate)
			wts[k] = (0.5 - 0.5*math.Cos(2*math.Pi*ts[k]/span)) * dt
		}
		for vi, v := range dopplerV {
			fq := v / lambda
			var e float64
			acc := make([]complex128, len(D[0]))
			for k, d := range D {
				r := cmplx.Exp(complex(0, -2*math.Pi*fq*ts[k])) * complex(wts[k], 0)
				for i, x := range d {
					acc[i] += x * r
				}
			}
			for _, x := range acc {
				e += real(x)*real(x) + imag(x)*imag(x)
			}
			P[vi] += e / sp / (span * span)
		}
	}
	return P
}

// signature: for a 4-chain link, the direction-dependent part of the moving
// reflection - the phases across chains of the moving part's dominant
// spatial component, relative to the long-term static channel's (which
// cancels the chains' fixed hardware offsets). quality is the share of the
// moving power in that one component (1 = a single clean reflection).
func signature(frames []csiFrame, st *txState, C, F int) (sig []float64, quality float64) {
	if C < 3 || st == nil || len(frames) < minFrames {
		return nil, 0
	}
	D, _ := dynamics(frames)
	R := make([]complex128, C*C)
	for _, d := range D {
		for f := 0; f < F; f++ {
			for i := 0; i < C; i++ {
				for j := 0; j < C; j++ {
					R[i*C+j] += d[i*F+f] * cmplx.Conj(d[j*F+f])
				}
			}
		}
	}
	Rs := make([]complex128, C*C)
	for f := 0; f < F; f++ {
		for i := 0; i < C; i++ {
			for j := 0; j < C; j++ {
				Rs[i*C+j] += st.slow[i*F+f] * cmplx.Conj(st.slow[j*F+f])
			}
		}
	}
	v, lam := principal(R, C)
	vs, _ := principal(Rs, C)
	var tr float64
	for i := 0; i < C; i++ {
		tr += real(R[i*C+i])
	}
	if tr <= 0 {
		return nil, 0
	}
	r0 := v[0] * cmplx.Conj(vs[0])
	for c := 1; c < C; c++ {
		rc := v[c] * cmplx.Conj(vs[c])
		sig = append(sig, cmplx.Phase(rc*cmplx.Conj(r0))*180/math.Pi)
	}
	return sig, lam / tr
}

// principal: dominant eigenvector and eigenvalue of a small Hermitian
// matrix, by power iteration.
func principal(R []complex128, C int) ([]complex128, float64) {
	v := make([]complex128, C)
	for i := range v {
		v[i] = complex(1/math.Sqrt(float64(C)), float64(i)*0.01)
	}
	var lam float64
	for it := 0; it < 60; it++ {
		w := make([]complex128, C)
		for i := 0; i < C; i++ {
			for j := 0; j < C; j++ {
				w[i] += R[i*C+j] * v[j]
			}
		}
		n := vnorm(w)
		if n == 0 {
			break
		}
		lam = n
		for i := range w {
			w[i] /= complex(n, 0)
		}
		v = w
	}
	return v, lam
}
