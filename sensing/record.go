package main

// Labelled recording: while a session is open, every capture file pulled
// from the router is also saved as-is, and labels sent from the page
// ("nobody", "walking", "toward router", ...) are logged with the time
// they were pressed. That gives ground truth to develop and tune the
// tracking against - without it, signal processing is tuned blind.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type recorder struct {
	root string

	mu     sync.Mutex
	dir    string // current session, "" when not recording
	start  time.Time
	files  int
	bytes  int64
	label  string
	marksF *os.File
}

type recStatus struct {
	Recording bool    `json:"recording"`
	Session   string  `json:"session,omitempty"`
	Seconds   int     `json:"seconds"`
	Files     int     `json:"files"`
	MB        float64 `json:"mb"`
	Label     string  `json:"label,omitempty"`
}

func (r *recorder) status() recStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := recStatus{Recording: r.dir != "", Files: r.files, MB: float64(r.bytes) / 1e6, Label: r.label}
	if r.dir != "" {
		st.Session = filepath.Base(r.dir)
		st.Seconds = int(time.Since(r.start).Seconds())
	}
	return st
}

func (r *recorder) begin() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dir != "" {
		return nil
	}
	dir := filepath.Join(r.root, time.Now().Format("2006-01-02_15-04-05"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "marks.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	r.dir, r.start, r.files, r.bytes, r.label, r.marksF = dir, time.Now(), 0, 0, "", f
	r.writeMark("start")
	return nil
}

func (r *recorder) end() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dir == "" {
		return
	}
	r.writeMark("stop")
	r.marksF.Close()
	r.dir, r.marksF, r.label = "", nil, ""
}

// writeMark: caller holds r.mu. Wall-clock time on this machine; the capture
// files carry the router's wall-clock start time in their names.
func (r *recorder) writeMark(label string) {
	b, _ := json.Marshal(map[string]any{"t": float64(time.Now().UnixNano()) / 1e9, "label": label})
	r.marksF.Write(append(b, '\n'))
}

func (r *recorder) mark(label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dir == "" {
		return
	}
	r.label = label
	r.writeMark(label)
}

func (r *recorder) save(files []dumpFile) {
	r.mu.Lock()
	dir := r.dir
	r.mu.Unlock()
	if dir == "" {
		return
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, 0o644); err == nil {
			r.mu.Lock()
			r.files++
			r.bytes += int64(len(f.data))
			r.mu.Unlock()
		}
	}
}

func (r *recorder) handle(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodPost {
		var body struct {
			Action string `json:"action"` // start | stop | mark
			Label  string `json:"label"`
		}
		b, _ := io.ReadAll(io.LimitReader(req.Body, 4096))
		if err := json.Unmarshal(b, &body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch body.Action {
		case "start":
			if err := r.begin(); err != nil {
				http.Error(w, fmt.Sprint(err), http.StatusInternalServerError)
				return
			}
		case "stop":
			r.end()
		case "mark":
			r.mark(body.Label)
		default:
			http.Error(w, "unknown action", http.StatusBadRequest)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(r.status())
}
