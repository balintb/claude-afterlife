package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"filippo.io/age"
)

var (
	fuzzKeyOnce    sync.Once
	fuzzKey        backupKey
	fuzzRecipients []age.Recipient
)

func fuzzKeys(t testing.TB) (backupKey, []age.Recipient) {
	fuzzKeyOnce.Do(func() {
		secret, recipient, err := newBackupKey()
		if err != nil {
			t.Fatal(err)
		}
		fuzzKey, _ = parseBackupKey(secret)
		fuzzRecipients, _ = parseRecipients([]string{recipient})
	})
	return fuzzKey, fuzzRecipients
}

// FuzzAppendBackupRoundTrip writes data to a transcript in random pieces, backing
// up after each one. Whatever is reassembled must equal the file up to its last
// complete line.
func FuzzAppendBackupRoundTrip(f *testing.F) {
	f.Add([]byte("one\ntwo\nthree\n"), []byte{3, 8})
	f.Add([]byte("no newline at all"), []byte{1})
	f.Add([]byte("\n\n\n"), []byte{0, 1, 2, 3})
	f.Add([]byte(strings.Repeat("{\"cwd\":\"/x\"}\n", 300)), []byte{10, 200, 255})
	f.Fuzz(func(t *testing.T, data, cuts []byte) {
		if len(data) > 8<<10 || len(cuts) > 16 {
			return
		}
		defer func(limit int) { segmentLimit = limit }(segmentLimit)
		segmentLimit = 997
		key, recipients := fuzzKeys(t)
		dir := t.TempDir()
		st := store{dir: filepath.Join(dir, "store")}
		path := filepath.Join(dir, "transcript.jsonl")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		points := []int{}
		for _, cut := range cuts {
			points = append(points, int(cut)*len(data)/255)
		}
		sort.Ints(points)
		points = append(points, len(data))
		app := &App{Now: time.Now}
		var entry *fileEntry
		written := 0
		for _, point := range points {
			if point < written {
				continue
			}
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			file.Write(data[written:point])
			file.Close()
			written = point
			next, _, err := app.backupAppend(st, recipients, sourceFile{storePath: "claude/x.jsonl", localPath: path, kind: kindAppend}, entry)
			if err != nil {
				t.Fatal(err)
			}
			entry = &next
			var out bytes.Buffer
			if err := st.assemble(next, key.identities, &out); err != nil {
				t.Fatal(err)
			}
			want := data[:written]
			want = want[:bytes.LastIndexByte(want, '\n')+1]
			if !bytes.Equal(out.Bytes(), want) {
				t.Fatalf("reassembled %q, want %q", out.Bytes(), want)
			}
		}
	})
}

// FuzzReadIndex feeds damaged and arbitrary indexes to the reader: it must never
// crash, and must never accept an index whose MAC does not check out.
func FuzzReadIndex(f *testing.F) {
	key, recipients := fuzzKeys(f)
	macKey, _ := macKeyFor(key.secret)
	st := store{dir: f.TempDir()}
	index := backupIndex{Format: backupFormat, MachineID: "0123456789abcdef0123456789abcdef", Files: []fileEntry{{Path: "claude/x"}}}
	if err := st.writeIndex(index, recipients, macKey); err != nil {
		f.Fatal(err)
	}
	indexPath, macPath, _ := st.indexPaths(index.MachineID)
	sealed, _ := os.ReadFile(indexPath)
	mac, _ := os.ReadFile(macPath)
	f.Add(sealed, string(mac))
	f.Add([]byte("garbage"), "00")
	f.Add([]byte{}, "")
	f.Fuzz(func(t *testing.T, sealed []byte, mac string) {
		dir := store{dir: t.TempDir()}
		indexPath, macPath, _ := dir.indexPaths(index.MachineID)
		if err := writeFileAtomic(indexPath, sealed, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeFileAtomic(macPath, []byte(mac), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := dir.readIndex(index.MachineID, key); err == nil && !validMAC(macKey, sealed, mac) {
			t.Fatal("accepted an index with an invalid MAC")
		}
	})
}

// FuzzMergeSettings checks that merging never changes a local value and always
// produces valid settings.
func FuzzMergeSettings(f *testing.F) {
	f.Add(`{"model":"opus"}`, `{"model":"sonnet","hooks":{"a":"/old/x"}}`, uint16(3650))
	f.Add(`null`, `{"a":1}`, uint16(0))
	f.Add(``, `{"cleanupPeriodDays":"x"}`, uint16(30))
	f.Add(`{} {}`, `{}`, uint16(0))
	f.Fuzz(func(t *testing.T, local, restored string, days uint16) {
		merged, _, changed, err := mergeSettings([]byte(local), []byte(restored), "/old", "/new", int(days))
		if err != nil {
			return
		}
		localSettings, err := decodeSettings([]byte(local))
		if err != nil {
			t.Fatalf("merged into invalid local settings %q", local)
		}
		if !changed {
			if !bytes.Equal(merged, []byte(local)) {
				t.Fatal("reported no change but rewrote the settings")
			}
			return
		}
		out, err := decodeSettings(merged)
		if err != nil {
			t.Fatalf("produced invalid settings %q", merged)
		}
		for key, value := range localSettings {
			if key != "cleanupPeriodDays" && !reflect.DeepEqual(out[key], value) {
				t.Fatalf("local %s changed from %v to %v", key, value, out[key])
			}
		}
	})
}

// FuzzGlobRegexp checks that any route pattern compiles or fails cleanly, and that
// a pattern without wildcards matches exactly itself.
func FuzzGlobRegexp(f *testing.F) {
	for _, seed := range []string{"/Users/me/code/**", "/a/*/b", "/x?y", "/[weird](chars)", "/**/**"} {
		f.Add(seed, "/Users/me/code/x")
	}
	f.Fuzz(func(t *testing.T, pattern, path string) {
		re, err := globRegexp(pattern)
		if err != nil {
			return
		}
		re.MatchString(path)
		if !strings.ContainsAny(pattern, "*?") && !re.MatchString(pattern) {
			t.Fatalf("literal pattern %q does not match itself", pattern)
		}
	})
}

// FuzzEncodeProjectPath checks that folder names only ever contain letters, digits
// and dashes, one character per UTF-16 code unit of the path.
func FuzzEncodeProjectPath(f *testing.F) {
	for _, seed := range []string{"/Users/me/code", "/tmp/é/🚀", "", "\x00\xff"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		encoded := encodeProjectPath(path)
		for _, r := range encoded {
			if !(r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				t.Fatalf("encoded %q to %q, which contains %q", path, encoded, r)
			}
		}
		if want := len(utf16.Encode([]rune(path))); len(encoded) != want {
			t.Fatalf("encoded %q to %d characters, want %d", path, len(encoded), want)
		}
	})
}
