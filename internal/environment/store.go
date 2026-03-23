package environment

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	// ErrNotFound is returned when an environment ID does not exist in the store.
	ErrNotFound = errors.New("environment not found")
	bucketName  = []byte("environments")
)

// Store is a BoltDB-backed persistence layer for Environments.
// All methods are safe for concurrent use — BoltDB serialises writes internally.
type Store struct {
	db *bolt.DB
}

// Open opens (or creates) the BoltDB file at path and ensures the environments
// bucket exists.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bolt db: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	}); err != nil {
		return nil, fmt.Errorf("create bucket: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the BoltDB file lock.
func (s *Store) Close() error {
	return s.db.Close()
}

// Put persists or overwrites an Environment, stamping UpdatedAt.
func (s *Store) Put(env *Environment) error {
	env.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal environment: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Put([]byte(env.ID), data)
	})
}

// Get retrieves an Environment by ID. Returns ErrNotFound if absent.
func (s *Store) Get(id string) (*Environment, error) {
	var env Environment
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketName).Get([]byte(id))
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &env)
	})
	if err != nil {
		return nil, err
	}
	return &env, nil
}

// Delete removes an Environment by ID. No-ops if absent.
func (s *Store) Delete(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Delete([]byte(id))
	})
}

// List returns all stored Environments, optionally filtered by status.
// Pass an empty string to return all.
func (s *Store) List(statusFilter string) ([]*Environment, error) {
	var envs []*Environment
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).ForEach(func(_, data []byte) error {
			var env Environment
			if err := json.Unmarshal(data, &env); err != nil {
				return err
			}
			if statusFilter == "" || env.Status == statusFilter {
				envs = append(envs, &env)
			}
			return nil
		})
	})
	return envs, err
}
