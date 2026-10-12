// SPDX-License-Identifier: MIT

package vm

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// record is the on-disk trace of one VM. It is written before the first
// resource is created and rewritten as resources appear, so that Reap can
// remove them after a crash of the runner. The record is deleted last.
type record struct {
	ID    string   `json:"id"`
	Slot  int      `json:"slot"`
	Job   string   `json:"job,omitempty"`
	PID   int      `json:"pid,omitempty"`
	Tap   string   `json:"tap,omitempty"`
	Gate  string   `json:"gateway,omitempty"`
	Dir   string   `json:"dir,omitempty"` // the jail directory of this VM (removed whole)
	Files []string `json:"files,omitempty"`
}

type store struct{ dir string }

func (s store) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s store) save(r record) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := s.path(r.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(r.ID))
}

func (s store) remove(id string) error {
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// list returns every readable record; unreadable or corrupt files are returned
// as ID-only records so that Reap still deletes them.
func (s store) list() ([]record, error) {
	ents, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []record
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			// A *.tmp left by a crash mid-write.
			if strings.HasSuffix(name, ".tmp") {
				_ = os.Remove(filepath.Join(s.dir, name))
			}
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		var r record
		if b, err := os.ReadFile(filepath.Join(s.dir, name)); err == nil && json.Unmarshal(b, &r) == nil && r.ID == id {
			out = append(out, r)
			continue
		}
		out = append(out, record{ID: id, Slot: -1})
	}
	return out, nil
}
