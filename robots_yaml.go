package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

type robotsFile struct {
	Robots []RobotRecord `yaml:"robots"`
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
	for _, r := range recs {
		want[r.ID] = true
		if err := mgr.Register(r); err != nil {
			log.Printf("[robots.yaml] register %s: %v", r.ID, err)
		}
	}
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
func WatchRobotsYAML(path string, mgr *SessionManager, store *Store) {
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
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Base(ev.Name) == base {
					log.Printf("[robots.yaml] change detected (%s), resyncing", ev.Op)
					SyncRobotsYAML(path, mgr, store)
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("[robots.yaml] watch error: %v", err)
			}
		}
	}()
}
