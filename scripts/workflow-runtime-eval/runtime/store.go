package workflowruntime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type EventStore interface {
	Events(runID string) []Event
	CommandApplied(runID, commandID string) bool
	Append(runID string, expectedVersion uint64, commandID string, proposed ...Event) (bool, error)
}

// MemoryEventStore is a deterministic reference implementation of the
// append-only persistence contract. A PostgreSQL adapter can implement the
// same append semantics transactionally without changing Engine behavior.
type MemoryEventStore struct {
	mu       sync.RWMutex
	events   map[string][]Event
	commands map[string]map[string]struct{}
}

func NewMemoryEventStore() *MemoryEventStore {
	return &MemoryEventStore{
		events:   make(map[string][]Event),
		commands: make(map[string]map[string]struct{}),
	}
}

func (s *MemoryEventStore) Events(runID string) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()

	events := s.events[runID]
	result := make([]Event, len(events))
	copy(result, events)
	return result
}

func (s *MemoryEventStore) CommandApplied(runID, commandID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, ok := s.commands[runID][commandID]
	return ok
}

func (s *MemoryEventStore) Append(runID string, expectedVersion uint64, commandID string, proposed ...Event) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.commands[runID][commandID]; ok {
		return true, nil
	}

	current := uint64(len(s.events[runID]))
	if current != expectedVersion {
		return false, ErrVersionConflict
	}

	if s.commands[runID] == nil {
		s.commands[runID] = make(map[string]struct{})
	}
	for i := range proposed {
		proposed[i].Version = current + uint64(i) + 1
		s.events[runID] = append(s.events[runID], proposed[i])
	}
	s.commands[runID][commandID] = struct{}{}
	return false, nil
}

type persistedEventStore struct {
	Events   map[string][]Event         `json:"events"`
	Commands map[string]map[string]bool `json:"commands"`
}

// FileEventStore is a small durable adapter used by the executable acceptance
// harness. It writes a complete next snapshot to a temporary file, fsyncs it,
// and atomically renames it before publishing the new in-memory view.
type FileEventStore struct {
	mu   sync.RWMutex
	path string
	data persistedEventStore
}

func OpenFileEventStore(path string) (*FileEventStore, error) {
	store := &FileEventStore{
		path: path,
		data: persistedEventStore{
			Events:   make(map[string][]Event),
			Commands: make(map[string]map[string]bool),
		},
	}
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return store, nil
	}
	if err := json.Unmarshal(payload, &store.data); err != nil {
		return nil, err
	}
	if store.data.Events == nil {
		store.data.Events = make(map[string][]Event)
	}
	if store.data.Commands == nil {
		store.data.Commands = make(map[string]map[string]bool)
	}
	return store, nil
}

func (s *FileEventStore) Events(runID string) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()

	events := s.data.Events[runID]
	result := make([]Event, len(events))
	copy(result, events)
	return result
}

func (s *FileEventStore) CommandApplied(runID, commandID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.data.Commands[runID][commandID]
}

func (s *FileEventStore) Append(runID string, expectedVersion uint64, commandID string, proposed ...Event) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.data.Commands[runID][commandID] {
		return true, nil
	}
	current := uint64(len(s.data.Events[runID]))
	if current != expectedVersion {
		return false, ErrVersionConflict
	}

	next := clonePersistedStore(s.data)
	if next.Commands[runID] == nil {
		next.Commands[runID] = make(map[string]bool)
	}
	for i := range proposed {
		proposed[i].Version = current + uint64(i) + 1
		next.Events[runID] = append(next.Events[runID], proposed[i])
	}
	next.Commands[runID][commandID] = true
	if err := writePersistedStore(s.path, next); err != nil {
		return false, err
	}
	s.data = next
	return false, nil
}

func clonePersistedStore(source persistedEventStore) persistedEventStore {
	result := persistedEventStore{
		Events:   make(map[string][]Event, len(source.Events)),
		Commands: make(map[string]map[string]bool, len(source.Commands)),
	}
	for runID, events := range source.Events {
		result.Events[runID] = append([]Event(nil), events...)
	}
	for runID, commands := range source.Commands {
		result.Commands[runID] = make(map[string]bool, len(commands))
		for commandID, applied := range commands {
			result.Commands[runID][commandID] = applied
		}
	}
	return result
}

func writePersistedStore(path string, data persistedEventStore) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".workflow-events-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
