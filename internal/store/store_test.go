package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New(%q) error: %v", dir, err)
	}
	return st
}

type testData struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

func TestStore_SaveAndLoad(t *testing.T) {
	st := setupTestStore(t)

	data := testData{Name: "test", Value: 42}
	if err := st.Save("abc123", data); err != nil {
		t.Fatalf("Save error: %v", err)
	}

	loaded, err := st.Load("abc123")
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("expected non-empty data")
	}
	// Check that JSON contains expected fields
	s := string(loaded)
	if !strings.Contains(s, `"name": "test"`) {
		t.Errorf("loaded JSON missing name field: %s", s)
	}
	if !strings.Contains(s, `"value": 42`) {
		t.Errorf("loaded JSON missing value field: %s", s)
	}
}

func TestStore_LoadNonExistent(t *testing.T) {
	st := setupTestStore(t)

	_, err := st.Load("nonexistent")
	if err == nil {
		t.Error("expected error for non-existent ID")
	}
}

func TestStore_LoadAll(t *testing.T) {
	st := setupTestStore(t)

	st.Save("id1", testData{Name: "one", Value: 1})
	st.Save("id2", testData{Name: "two", Value: 2})

	all, err := st.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll error: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 entries, got %d", len(all))
	}
	if _, ok := all["id1"]; !ok {
		t.Error("missing id1")
	}
	if _, ok := all["id2"]; !ok {
		t.Error("missing id2")
	}
}

func TestStore_LoadAll_EmptyDir(t *testing.T) {
	st := setupTestStore(t)

	all, err := st.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll error: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected 0 entries, got %d", len(all))
	}
}

func TestStore_Delete(t *testing.T) {
	st := setupTestStore(t)

	st.Save("to-delete", testData{Name: "bye", Value: 0})

	if err := st.Delete("to-delete"); err != nil {
		t.Fatalf("Delete error: %v", err)
	}

	_, err := st.Load("to-delete")
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestStore_Delete_NonExistent(t *testing.T) {
	st := setupTestStore(t)

	err := st.Delete("nonexistent")
	if err == nil {
		t.Error("expected error for non-existent delete")
	}
}

func TestStore_CreatesSessionsSubdir(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	sessDir := filepath.Join(dir, "sessions")
	info, err := os.Stat(sessDir)
	if err != nil {
		t.Fatalf("sessions dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("sessions is not a directory")
	}
	_ = st
}
