package main

// -replay DIR: run the analyzer over saved capture files (a test recording
// from data/rec, or any directory of cfr_dump_*.bin) and print what it
// would have shown, one line per link per tick, with the label that was
// active at the time if the directory has a marks.jsonl. For tuning the
// signal processing against what really happened.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type mark struct {
	T     float64 `json:"t"`
	Label string  `json:"label"`
}

func replay(dir, planPath string, threshold float64, rotation time.Duration) error {
	names, err := filepath.Glob(filepath.Join(dir, "cfr_dump_*.bin"))
	if err != nil {
		return err
	}
	sort.Strings(names)
	var files []dumpFile
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			return err
		}
		files = append(files, dumpFile{name: filepath.Base(n), data: b})
	}
	var marks []mark
	if f, err := os.Open(filepath.Join(dir, "marks.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var m mark
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				marks = append(marks, m)
			}
		}
		f.Close()
	}
	labelAt := func(t time.Time) string {
		s := float64(t.UnixNano()) / 1e9
		l := ""
		for _, m := range marks {
			if m.T <= s {
				l = m.Label
			}
		}
		return l
	}

	var pl Plan
	if b, err := os.ReadFile(planPath); err == nil {
		_ = json.Unmarshal(b, &pl)
	}
	trk := newTracker()
	a := newAnalyzer(threshold)
	a.ingest(files, rotation)
	all := map[string][]csiFrame{}
	var first, last time.Time
	for mac, l := range a.links {
		all[mac] = l.frames
		if len(l.frames) > 0 {
			if first.IsZero() || l.frames[0].t.Before(first) {
				first = l.frames[0].t
			}
			if t := l.frames[len(l.frames)-1].t; t.After(last) {
				last = t
			}
		}
		fmt.Fprintf(os.Stderr, "%s: %d frames, %d transmit states, %d glitches dropped\n", mac, len(l.frames), len(l.dsp.states), l.dsp.dropped)
	}
	fmt.Println("time\tmac\tlabel\tmetric_dB\tscore\tthr\tmotion\tspeed\ttoward\tsig\tsig_q")
	for T := first.Add(time.Second); !T.After(last); T = T.Add(tickEvery) {
		for mac, fr := range all {
			l := a.links[mac]
			if l == nil {
				continue
			}
			i := sort.Search(len(fr), func(i int) bool { return fr[i].t.After(T) })
			l.frames = fr[:i]
			l.lastSeen = T
		}
		states := a.tick(T)
		if tr := trk.step(T, pl, states); tr.Ready {
			for _, b := range tr.Bodies {
				fmt.Printf("%s\tbody%d\t%s\t%.2f,%.2f\tshare %.2f\tspread %.2f\tspeed %.2f\n", T.Format("15:04:05.00"), b.ID, labelAt(T), b.X, b.Y, b.Share, b.Spread, b.Speed)
			}
		}
		for _, s := range states {
			sig := make([]string, len(s.Sig))
			for i, v := range s.Sig {
				sig[i] = fmt.Sprintf("%.0f", v)
			}
			md := 0.0
			if s.Metric > 0 {
				md = dB(s.Metric)
			}
			fmt.Printf("%s\t%s\t%s\t%.1f\t%.1f\t%.1f\t%v\t%.2f\t%.2f\t%s\t%.2f\n", T.Format("15:04:05.00"), s.MAC, labelAt(T), md, s.Score, s.Threshold, s.Motion, s.Speed, s.Toward, strings.Join(sig, ","), s.SigQ)
		}
	}
	return nil
}
