package snapshot2

import (
	"encoding/json"
	"expvar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/raft"
)

const (
	persistSize             = "latest_persist_size"
	persistDuration         = "latest_persist_duration"
	reap_snapshots_duration = "reap_snapshots_duration"
	numSnapshotsReaped      = "num_snapshots_reaped"
)

const (
	metaFileName = "meta.json"
	tmpSuffix    = ".tmp"
)

// stats captures stats for the Store.
var stats *expvar.Map

// ResetStats resets the expvar stats for this module. Mostly for test purposes.
func ResetStats() {
	stats.Init()
	stats.Add(persistSize, 0)
	stats.Add(persistDuration, 0)
	stats.Add(reap_snapshots_duration, 0)
	stats.Add(numSnapshotsReaped, 0)
}

// Store stores Snapshots.
type Store struct {
	dir string
}

// NewStore returns a new Snapshot Store.
func NewStore(dir string) (*Store, error) {
	return &Store{dir: dir}, nil
}

// Create creates a new Sink object, ready for writing a snapshot. Sinks make certain assumptions about
// the state of the store, and if those assumptions were changed by another Sink writing to the store
// it could cause failures. Therefore we only allow 1 Sink to be in existence at a time. This shouldn't
// be a problem, since snapshots are taken infrequently in one at a time.
func (s *Store) Create(version raft.SnapshotVersion, index, term uint64, configuration raft.Configuration,
	configurationIndex uint64, trans raft.Transport) (raft.SnapshotSink, error) {

	meta := &raft.SnapshotMeta{
		ID:                 snapshotName(term, index),
		Index:              index,
		Term:               term,
		Configuration:      configuration,
		ConfigurationIndex: configurationIndex,
		Version:            version,
	}
	sink := NewSink(s, meta)
	if err := sink.Open(); err != nil {
		return nil, err
	}
	return sink, nil
}

// List returns a list of all the snapshots in the Store. It returns the snapshots
// in newest to oldest order.
func (s *Store) List() ([]*raft.SnapshotMeta, error) {
	snapshots, err := s.getSnapshots()
	if err != nil {
		return nil, err
	}
	var snapMeta []*raft.SnapshotMeta
	if len(snapshots) > 0 {
		snapshotDir := filepath.Join(s.dir, snapshots[0])
		meta, err := readMeta(snapshotDir)
		if err != nil {
			return nil, err
		}
		snapMeta = append(snapMeta, meta)
	}
	return snapMeta, nil
}

// Open opens the snapshot with the given ID.
func (s *Store) Open(id string) (*raft.SnapshotMeta, io.ReadCloser, error) {
	meta, err := readMeta(filepath.Join(s.dir, id))
	if err != nil {
		return nil, nil, err
	}
	fd, err := os.Open(filepath.Join(s.dir, id+".db"))
	if err != nil {
		return nil, nil, err
	}
	return meta, fd, nil
}

// Stats returns stats about the Snapshot Store.
func (s *Store) Stats() (map[string]interface{}, error) {
	return nil, nil
}

// Reap reaps all snapshots, except the most recent one. Returns the number of
// snapshots reaped.
func (s *Store) Reap() (int, error) {
	snapshots, err := s.getSnapshots()
	if err != nil {
		return 0, err
	}
	if len(snapshots) <= 1 {
		return 0, nil
	}
	// Remove all snapshots, and all associated data, except the newest one.
	n := 0
	for _, snap := range snapshots[:len(snapshots)-1] {
		if err := removeAllPrefix(s.dir, snap); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Dir returns the directory where the snapshots are stored.
func (s *Store) Dir() string {
	return s.dir
}

// getSnapshots returns a list of all snapshots in the store, sorted
// from oldest to newest.
func (s *Store) getSnapshots() ([]string, error) {
	directories, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var snapshots []string
	for _, d := range directories {
		if !isTmpName(d.Name()) {
			snapshots = append(snapshots, d.Name())
		}
	}
	return snapshots, nil
}

// RemoveAllTmpSnapshotData removes all temporary Snapshot data from the directory.
// This process is defined as follows: for every directory in dir, if the directory
// is a temporary directory, remove the directory. Then remove all other files
// that contain the name of a temporary directory, minus the temporary suffix,
// as prefix.
func RemoveAllTmpSnapshotData(dir string) error {
	// List all directories in the snapshot directory.
	directories, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, d := range directories {
		// If the directory is a temporary directory, remove it.
		if d.IsDir() && isTmpName(d.Name()) {
			files, err := filepath.Glob(filepath.Join(dir, nonTmpName(d.Name())) + "*")
			if err != nil {
				return err
			}

			fullTmpDirPath := filepath.Join(dir, d.Name())
			for _, f := range files {
				if f == fullTmpDirPath {
					// Only delete directory after all files have been deleted.
					continue
				}
				if err := os.Remove(f); err != nil {
					return err
				}
			}
			if err := os.RemoveAll(fullTmpDirPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// snapshotName generates a name for the snapshot.
func snapshotName(term, index uint64) string {
	now := time.Now()
	msec := now.UnixNano() / int64(time.Millisecond)
	return fmt.Sprintf("%d-%d-%d", term, index, msec)
}

func tmpName(path string) string {
	return path + tmpSuffix
}

func nonTmpName(path string) string {
	return strings.TrimSuffix(path, tmpSuffix)
}

func isTmpName(name string) bool {
	return filepath.Ext(name) == tmpSuffix
}

func syncDir(dir string) error {
	fh, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer fh.Close()
	return fh.Sync()
}

// syncDirParentMaybe syncsthe given directory, but only on non-Windows platforms.
func syncDirMaybe(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return syncDir(dir)
}

// removeAllPrefix removes all files in the given directory that have the given prefix.
func removeAllPrefix(path, prefix string) error {
	files, err := filepath.Glob(filepath.Join(path, prefix) + "*")
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := os.RemoveAll(f); err != nil {
			return err
		}
	}
	return nil
}

// readMeta is used to read the meta data in a given snapshot directory.
func readMeta(dir string) (*raft.SnapshotMeta, error) {
	metaPath := filepath.Join(dir, metaFileName)
	fh, err := os.Open(metaPath)
	if err != nil {
		return nil, err
	}
	defer fh.Close()

	meta := &raft.SnapshotMeta{}
	dec := json.NewDecoder(fh)
	if err := dec.Decode(meta); err != nil {
		return nil, err
	}
	return meta, nil
}

func writeMeta(dir string, meta *raft.SnapshotMeta) error {
	fh, err := os.Create(filepath.Join(dir, metaFileName))
	if err != nil {
		return fmt.Errorf("error creating meta file: %v", err)
	}
	defer fh.Close()

	// Write out as JSON
	enc := json.NewEncoder(fh)
	if err = enc.Encode(meta); err != nil {
		return fmt.Errorf("failed to encode meta: %v", err)
	}

	if err := fh.Sync(); err != nil {
		return err
	}
	return fh.Close()
}
