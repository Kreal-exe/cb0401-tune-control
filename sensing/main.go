// sensing: this project's own Wi-Fi motion map. It pulls the router's CFR
// captures (armed by router/cfr_capture_daemon.sh) over SSH, measures per
// link how much the radio channel is being disturbed, and serves a page
// with your floor plan - walls, router and devices placed by hand - on
// which links light up and a dot glows where movement is.
//
// Started by ../sensing.sh, which also installs the router side, opens the
// optional Cloudflare tunnel and sends the link.
package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// Plan is the floor plan, in metres. Walls are segments {x1, y1, x2, y2}.
type Plan struct {
	Walls   [][4]float64          `json:"walls"`
	Router  *[2]float64           `json:"router"`
	Devices map[string][2]float64 `json:"devices"`        // MAC -> position
	Names   map[string]string     `json:"names"`          // MAC -> your own label
	Auto    bool                  `json:"auto,omitempty"` // laid out automatically, not yet adjusted by hand
}

type server struct {
	rt       *router
	an       *analyzer
	planPath string

	mu       sync.Mutex
	plan     Plan
	stations []station
	stErr    string
	lastData time.Time
	records  int

	subsMu sync.Mutex
	subs   map[chan []byte]struct{}
}

func (s *server) loadPlan() {
	s.plan = Plan{Devices: map[string][2]float64{}, Names: map[string]string{}}
	b, err := os.ReadFile(s.planPath)
	if err == nil {
		_ = json.Unmarshal(b, &s.plan)
	}
	if s.plan.Devices == nil {
		s.plan.Devices = map[string][2]float64{}
	}
	if s.plan.Names == nil {
		s.plan.Names = map[string]string{}
	}
}

func (s *server) savePlan(p Plan) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.planPath), 0o755); err != nil {
		return err
	}
	tmp := s.planPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.planPath)
}

// capture loop: pull finished dump files as soon as they exist.
func (s *server) pull(rotation time.Duration) {
	for {
		files, err := s.rt.takeCompleted()
		if err != nil {
			log.Printf("fetch: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		if len(files) == 0 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		n := s.an.ingest(files, rotation)
		s.mu.Lock()
		s.records += n
		if n > 0 {
			s.lastData = time.Now()
		}
		s.mu.Unlock()
	}
}

// station list: who's connected, names, what's being captured.
func (s *server) refreshStations() {
	for {
		st, err := s.rt.stations()
		s.mu.Lock()
		if err != nil {
			s.stErr = err.Error()
		} else {
			s.stations, s.stErr = st, ""
		}
		s.mu.Unlock()
		time.Sleep(15 * time.Second)
	}
}

type snapshot struct {
	Time      int64       `json:"time"`
	Links     []LinkState `json:"links"`
	Dot       Dot         `json:"dot"`
	Stations  []station   `json:"stations"`
	Receiving bool        `json:"receiving"` // capture data arrived in the last few seconds
	Error     string      `json:"error,omitempty"`
}

func (s *server) broadcastLoop() {
	t := time.NewTicker(tickEvery)
	for now := range t.C {
		s.mu.Lock()
		pl := s.plan
		st := append([]station(nil), s.stations...)
		errText := s.stErr
		receiving := now.Sub(s.lastData) < 5*time.Second
		s.mu.Unlock()
		links, dot := s.an.tick(now, pl)
		b, _ := json.Marshal(snapshot{Time: now.UnixMilli(), Links: links, Dot: dot, Stations: st, Receiving: receiving, Error: errText})
		s.subsMu.Lock()
		for ch := range s.subs {
			select {
			case ch <- b:
			default: // slow client: drop this update rather than block
			}
		}
		s.subsMu.Unlock()
	}
}

func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := make(chan []byte, 4)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()
	defer func() {
		s.subsMu.Lock()
		delete(s.subs, ch)
		s.subsMu.Unlock()
	}()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *server) handlePlan(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		b, _ := json.Marshal(s.plan)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	case http.MethodPut, http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var p Plan
		if err := json.Unmarshal(body, &p); err != nil {
			http.Error(w, "bad plan: "+err.Error(), http.StatusBadRequest)
			return
		}
		if p.Devices == nil {
			p.Devices = map[string][2]float64{}
		}
		if p.Names == nil {
			p.Names = map[string]string{}
		}
		if err := s.savePlan(p); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.mu.Lock()
		s.plan = p
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// withKey gates a handler behind an access key: ?k=<key> once, then a
// cookie. Used for the listener the Cloudflare tunnel points at - the tunnel
// hostname is public, so the link itself is the credential.
func withKey(key string, next http.Handler) http.Handler {
	const cookie = "sensing_k"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k := r.URL.Query().Get("k"); k != "" {
			if subtle.ConstantTimeCompare([]byte(k), []byte(key)) != 1 {
				http.Error(w, "wrong access key", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookie, Value: key, Path: "/", HttpOnly: true,
				SameSite: http.SameSiteLaxMode, Secure: strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")})
			q := r.URL.Query()
			q.Del("k")
			u := *r.URL
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.RequestURI(), http.StatusFound)
			return
		}
		if c, err := r.Cookie(cookie); err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(key)) != 1 {
			http.Error(w, "this page needs the access link you were sent", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	keyPath := flag.String("key", "", "router SSH private key (gui/router_key)")
	host := flag.String("host", "root@192.168.31.1", "router SSH target")
	listen := flag.String("listen", "127.0.0.1:3000", "address for this machine's browser (no key)")
	public := flag.String("public", "", "optional second address for the Cloudflare tunnel, gated by -access-key")
	accessKey := flag.String("access-key", "", "access key for -public")
	planPath := flag.String("plan", "data/plan.json", "where the floor plan is stored")
	rotation := flag.Duration("rotation", 2*time.Second, "how long each router dump file covers (cfr_capture_daemon.sh POLL_SECONDS)")
	threshold := flag.Float64("threshold", 0.4, "motion threshold: how far above its own normal level a link must be (0.4 = 40%)")
	flag.Parse()
	if *keyPath == "" {
		fmt.Fprintln(os.Stderr, "usage: sensing -key path/to/router_key [-host root@192.168.31.1] [-listen 127.0.0.1:3000]")
		os.Exit(2)
	}
	if *public != "" && *accessKey == "" {
		fmt.Fprintln(os.Stderr, "-public needs -access-key")
		os.Exit(2)
	}

	s := &server{
		rt:       &router{keyPath: *keyPath, host: *host},
		an:       newAnalyzer(*threshold),
		planPath: *planPath,
		subs:     map[chan []byte]struct{}{},
	}
	s.loadPlan()
	go s.pull(*rotation)
	go s.refreshStations()
	go s.broadcastLoop()

	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/stream", s.handleStream)
	mux.HandleFunc("/api/plan", s.handlePlan)
	noCache := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})

	if *public != "" {
		go func() {
			log.Printf("sensing: public listener %s (access key required)", *public)
			log.Fatal(http.ListenAndServe(*public, withKey(*accessKey, noCache)))
		}()
	}
	log.Printf("sensing: http://%s  (router %s, plan %s)", *listen, *host, *planPath)
	log.Fatal(http.ListenAndServe(*listen, noCache))
}
