package store

import (
	json "encoding/json/v2"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"path/filepath"
)

// Store handles session persistence to JSON files.
type Store struct {
	dir string
}

// New creates a new Store with the given data directory.
func New(dir string) (*Store, error) {
	sessDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating sessions dir: %w", err)
	}
	return &Store{dir: sessDir}, nil
}

// Save persists a value to disk as JSON.
func (s *Store) Save(id string, v any) error {
	data, err := json.Marshal(v, jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("marshaling: %w", err)
	}
	return s.SaveBytes(id, data)
}

// SaveBytes writes pre-marshaled JSON bytes to disk.
// persist 等でロック下で事前にマーシャルした場合に使う。
func (s *Store) SaveBytes(id string, data []byte) error {
	path := filepath.Join(s.dir, id+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}
	return nil
}

// Load reads raw JSON data for the given id.
func (s *Store) Load(id string) ([]byte, error) {
	path := filepath.Join(s.dir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}
	return data, nil
}

// LoadAll reads all JSON files from the store directory and returns their raw data with IDs.
func (s *Store) LoadAll() (map[string][]byte, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading sessions dir: %w", err)
	}

	result := make(map[string][]byte)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := entry.Name()[:len(entry.Name())-5]
		data, err := s.Load(id)
		if err != nil {
			continue // skip corrupt files
		}
		result[id] = data
	}

	return result, nil
}

// Delete removes a session file from disk.
func (s *Store) Delete(id string) error {
	path := filepath.Join(s.dir, id+".json")
	return os.Remove(path)
}
