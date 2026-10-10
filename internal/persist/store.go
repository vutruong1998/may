package persist

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// Service is a service whose state can be saved. SaveState holds the
// service's lock while it encodes; LoadState runs before serving requests.
type Service interface {
	SaveState(w io.Writer, blobs Blobs) error
	LoadState(r io.Reader, blobs Blobs) error
}

// Store keeps the state of services in a directory:
//
//	<dir>/<service>.json  one snapshot per service
//	<dir>/blobs/<sha256>  large byte slices (S3 object bodies, ...)
//
// Files are written to a temporary name and renamed, so a crash leaves the
// previous snapshot intact.
type Store struct {
	dir      string
	services []named
	changes  atomic.Int64 // bumped by Touch
	saved    int64        // changes at the last successful save

	mu    sync.Mutex // serializes saves
	blobs *fileBlobs
}

type named struct {
	name string
	svc  Service
}

// Open creates dir if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir, blobs: &fileBlobs{dir: filepath.Join(dir, "blobs"), cache: map[blobKey]cached{}}}, nil
}

// Dir returns the data directory.
func (s *Store) Dir() string { return s.dir }

// Add registers a service under a file name.
func (s *Store) Add(name string, svc Service) { s.services = append(s.services, named{name, svc}) }

// Touch records that state may have changed, so the next Save writes it.
func (s *Store) Touch() { s.changes.Add(1) }

// Load restores every service that has a snapshot. It returns the names of
// the services loaded.
func (s *Store) Load() ([]string, error) {
	var loaded []string
	for _, n := range s.services {
		f, err := os.Open(s.file(n.name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return loaded, err
		}
		err = n.svc.LoadState(f, s.blobs)
		f.Close()
		if err != nil {
			return loaded, fmt.Errorf("loading %s: %w", s.file(n.name), err)
		}
		loaded = append(loaded, n.name)
	}
	return loaded, nil
}

func (s *Store) file(name string) string { return filepath.Join(s.dir, name+".json") }

// Save writes every service's state if anything changed since the last
// save (or always, with force), then deletes blobs no snapshot uses.
func (s *Store) Save(force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changes := s.changes.Load()
	if !force && changes == s.saved {
		return nil
	}
	s.blobs.begin()
	for _, n := range s.services {
		var buf bytes.Buffer
		if err := n.svc.SaveState(&buf, s.blobs); err != nil {
			return fmt.Errorf("saving %s: %w", n.name, err)
		}
		if err := writeFile(s.file(n.name), buf.Bytes()); err != nil {
			return err
		}
	}
	s.blobs.collect()
	s.saved = changes
	return nil
}

// Run saves every interval until ctx is done, then saves a last time.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Save(false); err != nil {
				log.Printf("persist: %v", err)
			}
		}
	}
}

// writeFile replaces path atomically.
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// fileBlobs stores blobs as files named by their SHA-256.
type fileBlobs struct {
	dir string

	mu    sync.Mutex
	cache map[blobKey]cached // hashes of slices already written
	used  map[string]bool    // blobs referenced by the save in progress
	keep  map[blobKey]cached
}

// blobKey identifies a byte slice by its backing array. Services never
// modify stored bodies in place, and the cache holds a reference to each
// slice, so an address can't be reused for other contents while cached.
type blobKey struct {
	p unsafe.Pointer
	n int
}

type cached struct {
	data []byte
	hash string
}

func (b *fileBlobs) begin() {
	b.mu.Lock()
	b.used, b.keep = map[string]bool{}, map[blobKey]cached{}
	b.mu.Unlock()
}

func (b *fileBlobs) Put(data []byte) (string, error) {
	k := blobKey{unsafe.Pointer(&data[0]), len(data)}
	b.mu.Lock()
	c, ok := b.cache[k]
	b.mu.Unlock()
	if !ok {
		c = cached{data, Hash(data)}
		path := filepath.Join(b.dir, c.hash)
		if _, err := os.Stat(path); err != nil {
			if err := writeFile(path, data); err != nil {
				return "", err
			}
		}
	}
	b.mu.Lock()
	if b.used != nil {
		b.used[c.hash] = true
		b.keep[k] = c
	}
	b.cache[k] = c
	b.mu.Unlock()
	return c.hash, nil
}

func (b *fileBlobs) Get(hash string) ([]byte, error) {
	if len(hash) != 64 || filepath.Base(hash) != hash {
		return nil, fmt.Errorf("persist: bad blob name %q", hash)
	}
	data, err := os.ReadFile(filepath.Join(b.dir, hash))
	if err == nil && len(data) > 0 {
		// Loaded slices are cached too, so the first save doesn't rehash them.
		b.mu.Lock()
		b.cache[blobKey{unsafe.Pointer(&data[0]), len(data)}] = cached{data, hash}
		b.mu.Unlock()
	}
	return data, err
}

// collect deletes blobs the finished save didn't reference and forgets
// cached slices that are gone.
func (b *fileBlobs) collect() {
	b.mu.Lock()
	used := b.used
	b.cache, b.used, b.keep = b.keep, nil, nil
	b.mu.Unlock()
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !used[e.Name()] {
			os.Remove(filepath.Join(b.dir, e.Name()))
		}
	}
}
