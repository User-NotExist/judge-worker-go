// Command devapi is a stand-in for the API's internal events endpoint, for local
// development and smoke tests only. It stores events in memory.
//
//	POST /internal/judge-events          event object or array → 200
//	POST /internal/judge-jobs/{id}/refresh                     → 404
//	GET  /events[?submission_id=N]       stored events (JSON array)
//	POST /admin/down?for=120s            answer 503 for a while (spool tests)
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

type event struct {
	EventID      string          `json:"event_id"`
	Seq          int             `json:"seq"`
	Type         string          `json:"type"`
	SubmissionID int64           `json:"submission_id"`
	Attempt      int             `json:"attempt"`
	JobID        string          `json:"job_id"`
	WorkerID     string          `json:"worker_id"`
	Payload      json.RawMessage `json:"payload"`
}

func main() {
	var mu sync.Mutex
	var events []event
	seen := map[string]bool{}
	latest := map[int64]int{} // submission → highest attempt (409 for older)
	var downUntil time.Time
	token := os.Getenv("API_TOKEN")

	http.HandleFunc("POST /internal/judge-events", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", 401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if time.Now().Before(downUntil) {
			http.Error(w, "down", 503)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var evs []event
		if len(b) > 0 && b[0] == '[' {
			if err := json.Unmarshal(b, &evs); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		} else {
			var e event
			if err := json.Unmarshal(b, &e); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			evs = []event{e}
		}
		for _, e := range evs {
			if e.SubmissionID > 0 && e.Attempt < latest[e.SubmissionID] {
				http.Error(w, "attempt superseded", 409)
				return
			}
		}
		for _, e := range evs {
			if seen[e.EventID] {
				continue // duplicate, ignored
			}
			seen[e.EventID] = true
			if e.SubmissionID > 0 && e.Attempt > latest[e.SubmissionID] {
				latest[e.SubmissionID] = e.Attempt
			}
			events = append(events, e)
			log.Printf("event %s %-11s %s", e.EventID, e.Type, string(e.Payload))
		}
		w.WriteHeader(200)
	})
	http.HandleFunc("POST /internal/judge-jobs/{id}/refresh", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refresh not implemented in devapi", 404)
	})
	http.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		out := []event{}
		sid, _ := strconv.ParseInt(r.URL.Query().Get("submission_id"), 10, 64)
		for _, e := range events {
			if sid == 0 || e.SubmissionID == sid {
				out = append(out, e)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	http.HandleFunc("POST /admin/down", func(w http.ResponseWriter, r *http.Request) {
		d, err := time.ParseDuration(r.URL.Query().Get("for"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		mu.Lock()
		downUntil = time.Now().Add(d)
		mu.Unlock()
		log.Printf("admin: returning 503 for %s", d)
	})
	addr := ":8080"
	log.Printf("devapi listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
