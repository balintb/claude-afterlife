package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"filippo.io/age"
)

const (
	backupFormat = 1
	storeKind    = "claude-afterlife-backup"
	kindAppend   = "append"
	kindWhole    = "whole"
)

var segmentLimit = 8 << 20

var (
	objectNamePattern  = regexp.MustCompile(`^[0-9a-f]{2}/[0-9a-f]{30}$`)
	machineIDPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	errMissingObject   = errors.New("missing object")
	errCorruptedObject = errors.New("corrupted object")
)

type storeMeta struct {
	Format     int       `json:"format"`
	Kind       string    `json:"kind"`
	Recipients []string  `json:"recipients"`
	CreatedAt  time.Time `json:"created_at"`
}

type segmentRef struct {
	Object string `json:"object"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	SHA256 string `json:"sha256"`
}

// fileEntry is one backed-up file. Size and SHA256 describe bytes [0, Size) of the
// file at the time of the last backup that changed it.
type fileEntry struct {
	Path     string       `json:"path"`
	Kind     string       `json:"kind"`
	Project  string       `json:"project,omitempty"`
	Size     int64        `json:"size"`
	SHA256   string       `json:"sha256"`
	ModTime  time.Time    `json:"mtime"`
	Mode     uint32       `json:"mode,omitempty"`
	Deleted  bool         `json:"deleted_locally,omitempty"`
	Segments []segmentRef `json:"segments"`
}

type projectInfo struct {
	Path     string `json:"path"`
	RepoRoot string `json:"repo_root,omitempty"`
	Remote   string `json:"remote,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Head     string `json:"head,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
}

// backupIndex is what one machine has backed up. The local copy is plain JSON next
// to the store; the copy in the store is encrypted and authenticated.
type backupIndex struct {
	Format        int           `json:"format"`
	MachineID     string        `json:"machine_id"`
	Hostname      string        `json:"hostname"`
	Home          string        `json:"home"`
	ClaudeVersion string        `json:"claude_version,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	Projects      []projectInfo `json:"projects"`
	Files         []fileEntry   `json:"files"`
}

func (index *backupIndex) project(path string) (projectInfo, bool) {
	for _, info := range index.Projects {
		if info.Path == path {
			return info, true
		}
	}
	return projectInfo{}, false
}

type store struct {
	dir string
}

func (s store) objectPath(name string) (string, error) {
	if !objectNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid object name %q", name)
	}
	return filepath.Join(s.dir, "objects", filepath.FromSlash(name)+".age"), nil
}

func (s store) putObject(sealed []byte) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random)
	name := id[:2] + "/" + id[2:]
	path, err := s.objectPath(name)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, sealed, 0o600); err != nil {
		return "", err
	}
	return name, nil
}

func (s store) getObject(name string) ([]byte, error) {
	path, err := s.objectPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w %s", errMissingObject, name)
	}
	return data, err
}

func (s store) hasObject(name string) bool {
	path, err := s.objectPath(name)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func (s store) metaPath() string {
	return filepath.Join(s.dir, "store.json")
}

func (s store) readMeta() (storeMeta, error) {
	var meta storeMeta
	data, err := os.ReadFile(s.metaPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return meta, fmt.Errorf("%s is not a claude-afterlife backup (no store.json)", s.dir)
		}
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil || meta.Kind != storeKind {
		return meta, fmt.Errorf("%s is not a claude-afterlife backup", s.dir)
	}
	if meta.Format != backupFormat {
		return meta, fmt.Errorf("backup format %d is not supported by this version (expected %d)", meta.Format, backupFormat)
	}
	return meta, nil
}

func (s store) writeMeta(meta storeMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.metaPath(), append(data, '\n'), 0o644)
}

func (s store) indexPaths(machineID string) (string, string, error) {
	if !machineIDPattern.MatchString(machineID) {
		return "", "", fmt.Errorf("invalid machine id %q", machineID)
	}
	dir := filepath.Join(s.dir, "machines", machineID)
	return filepath.Join(dir, "index.age"), filepath.Join(dir, "index.mac"), nil
}

func (s store) machineIDs() []string {
	entries, _ := os.ReadDir(filepath.Join(s.dir, "machines"))
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() && machineIDPattern.MatchString(entry.Name()) {
			ids = append(ids, entry.Name())
		}
	}
	sort.Strings(ids)
	return ids
}

func (s store) writeIndex(index backupIndex, recipients []age.Recipient, macKey []byte) error {
	plain, err := json.Marshal(index)
	if err != nil {
		return err
	}
	sealed, err := seal(plain, recipients)
	if err != nil {
		return err
	}
	indexPath, macPath, err := s.indexPaths(index.MachineID)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(indexPath, sealed, 0o600); err != nil {
		return err
	}
	return writeFileAtomic(macPath, []byte(indexMAC(macKey, sealed)+"\n"), 0o600)
}

func (s store) readIndex(machineID string, key backupKey) (backupIndex, error) {
	var index backupIndex
	indexPath, macPath, err := s.indexPaths(machineID)
	if err != nil {
		return index, err
	}
	sealed, err := os.ReadFile(indexPath)
	if err != nil {
		return index, fmt.Errorf("no backup from machine %s in this store", shortID(machineID))
	}
	plain, err := unseal(sealed, key.identities)
	if err != nil {
		return index, err
	}
	mac, err := os.ReadFile(macPath)
	if err != nil {
		return index, errTampered
	}
	macKey, err := macKeyFor(key.secret)
	if err != nil {
		return index, err
	}
	if !validMAC(macKey, sealed, string(mac)) {
		return index, errTampered
	}
	if err := json.Unmarshal(plain, &index); err != nil {
		return index, fmt.Errorf("reading the backup index: %w", err)
	}
	if index.Format != backupFormat || index.MachineID != machineID {
		return index, fmt.Errorf("unexpected backup index for machine %s", shortID(machineID))
	}
	return index, nil
}

// putRange encrypts bytes [from, to) of r as segments of at most segmentLimit bytes,
// feeding them into running so the caller can keep a hash of the whole file.
func (s store) putRange(r io.ReaderAt, from, to int64, recipients []age.Recipient, running hash.Hash) ([]segmentRef, error) {
	var refs []segmentRef
	buffer := make([]byte, segmentLimit)
	for offset := from; offset < to; {
		length := min(int64(segmentLimit), to-offset)
		chunk := buffer[:length]
		if _, err := r.ReadAt(chunk, offset); err != nil && !(errors.Is(err, io.EOF) && int64(len(chunk)) == length) {
			return nil, err
		}
		sealed, err := seal(chunk, recipients)
		if err != nil {
			return nil, err
		}
		name, err := s.putObject(sealed)
		if err != nil {
			return nil, err
		}
		if running != nil {
			running.Write(chunk)
		}
		refs = append(refs, segmentRef{Object: name, Offset: offset, Length: length, SHA256: sha256Hex(chunk)})
		offset += length
	}
	return refs, nil
}

// assemble writes the backed-up bytes of entry to w, checking every segment and the
// whole against the hashes in the index.
func (s store) assemble(entry fileEntry, identities []age.Identity, w io.Writer) error {
	total := sha256.New()
	var next int64
	for _, segment := range entry.Segments {
		if segment.Offset != next {
			return fmt.Errorf("%w: gap at byte %d", errCorruptedObject, next)
		}
		sealed, err := s.getObject(segment.Object)
		if err != nil {
			return err
		}
		plain, err := unseal(sealed, identities)
		if err != nil {
			if errors.Is(err, errWrongKey) {
				return err
			}
			return fmt.Errorf("%w %s: %v", errCorruptedObject, segment.Object, err)
		}
		if int64(len(plain)) != segment.Length || sha256Hex(plain) != segment.SHA256 {
			return fmt.Errorf("%w %s: content does not match the index", errCorruptedObject, segment.Object)
		}
		total.Write(plain)
		if _, err := w.Write(plain); err != nil {
			return err
		}
		next += segment.Length
	}
	if next != entry.Size || hex.EncodeToString(total.Sum(nil)) != entry.SHA256 {
		return fmt.Errorf("%w: reassembled file does not match the index", errCorruptedObject)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func newMachineID() string {
	random := make([]byte, 16)
	_, _ = rand.Read(random)
	return hex.EncodeToString(random)
}
