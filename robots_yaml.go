package main

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

type robotsFile struct {
	Robots []RobotRecord `yaml:"robots"`
}

// lastYAMLRecords caches the last-applied yaml records (keyed by ID) so we only
// re-Register robots whose connection-relevant fields actually changed.
var (
	lastYAMLMu      sync.Mutex
	lastYAMLRecords = map[string]RobotRecord{}
)

// robotRecordChanged reports whether the connection-relevant fields of a differ
// from b. Status/Source/LastSeenAt are intentionally ignored.
func robotRecordChanged(a, b RobotRecord) bool {
	return a.Manufacturer != b.Manufacturer ||
		a.Serial != b.Serial ||
		a.AtomBaseURL != b.AtomBaseURL ||
		a.FastAPIHTTPURL != b.FastAPIHTTPURL ||
		a.FastAPIWSURL != b.FastAPIWSURL ||
		a.WebhookSecret != b.WebhookSecret
}

// LoadRobotsYAML returns the robots declared in path. A missing file is not an error.
func LoadRobotsYAML(path string) ([]RobotRecord, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f robotsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	for i := range f.Robots {
		if f.Robots[i].Source == "" {
			f.Robots[i].Source = "yaml"
		}
	}
	return f.Robots, nil
}

// SyncRobotsYAML registers/updates robots from the file and deregisters yaml-sourced
// robots that disappeared from it.
func SyncRobotsYAML(path string, mgr *SessionManager, store *Store) {
	recs, err := LoadRobotsYAML(path)
	if err != nil {
		log.Printf("[robots.yaml] load error: %v", err)
		return
	}
	want := map[string]bool{}
	lastYAMLMu.Lock()
	for _, r := range recs {
		want[r.ID] = true
		prev, seen := lastYAMLRecords[r.ID]
		if !seen || robotRecordChanged(r, prev) {
			if err := mgr.Register(r); err != nil {
				log.Printf("[robots.yaml] register %s: %v", r.ID, err)
			}
		}
	}
	// Replace the cache with exactly what's in the file now.
	next := make(map[string]RobotRecord, len(recs))
	for _, r := range recs {
		next[r.ID] = r
	}
	lastYAMLRecords = next
	lastYAMLMu.Unlock()

	if store != nil {
		known, _ := store.ListActiveRobots()
		for _, k := range known {
			if k.Source == "yaml" && !want[k.ID] {
				mgr.Deregister(k.ID)
			}
		}
	}
}

// WatchRobotsYAML calls SyncRobotsYAML on startup and whenever the file changes.
// stopCh may be closed to stop the watcher goroutine and release the fsnotify watcher.
func WatchRobotsYAML(path string, mgr *SessionManager, store *Store, stopCh <-chan struct{}) {
	SyncRobotsYAML(path, mgr, store)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[robots.yaml] watcher disabled: %v", err)
		return
	}
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := w.Add(dir); err != nil {
		log.Printf("[robots.yaml] watch %s: %v", dir, err)
		_ = w.Close()
		return
	}
	base := filepath.Base(path)
	go func() {
		// Debounce: editors emit 2-3 fsnotify events per save. Coalesce a burst
		// into a single resync ~500ms after the last relevant event.
		const debounce = 500 * time.Millisecond
		timer := time.NewTimer(debounce)
		if !timer.Stop() {
			<-timer.C
		}
		for {
			select {
			case <-stopCh:
				w.Close()
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Base(ev.Name) == base {
					log.Printf("[robots.yaml] change detected (%s), debouncing", ev.Op)
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(debounce)
				}
			case <-timer.C:
				log.Printf("[robots.yaml] resyncing after debounce")
				SyncRobotsYAML(path, mgr, store)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("[robots.yaml] watch error: %v", err)
			}
		}
	}()
}
