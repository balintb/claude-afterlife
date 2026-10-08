package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func TestEncodeProjectPath(t *testing.T) {
	cases := map[string]string{
		"/Users/balint/code/theydo/theydo-journey-ai":                "-Users-balint-code-theydo-theydo-journey-ai",
		"/private/var/folders/fd/sddxh2y96fv8rh_28dyk02sw0000gn/T":   "-private-var-folders-fd-sddxh2y96fv8rh-28dyk02sw0000gn-T",
		"/Users/balint/Documents/Paradox Interactive/Hearts of Iron": "-Users-balint-Documents-Paradox-Interactive-Hearts-of-Iron",
		"/Users/me/.config/app":                                      "-Users-me--config-app",
		"/Users/me/café":                                             "-Users-me-caf-",
		"/Users/me/🚀":                                                "-Users-me---",
	}
	for path, want := range cases {
		if got := encodeProjectPath(path); got != want {
			t.Errorf("encodeProjectPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestResolveSlugByWalk(t *testing.T) {
	root := t.TempDir()
	unique := filepath.Join(root, "code", "my_project", "api")
	for _, dir := range []string{unique, filepath.Join(root, "x", "a_b", "c"), filepath.Join(root, "x", "a-b", "c"), filepath.Join(root, "x", "a", "b", "c")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := resolveSlugByWalk("/", encodeProjectPath(unique)); got != unique {
		real, _ := filepath.EvalSymlinks(unique)
		if got != real {
			t.Errorf("resolved %q, want %q", got, unique)
		}
	}
	if got := resolveSlugByWalk(root, encodeProjectPath(filepath.Join(root, "x", "a-b", "c"))); got != "" {
		t.Errorf("an ambiguous folder name resolved to %q", got)
	}
	if got := resolveSlugByWalk(root, encodeProjectPath(filepath.Join(root, "missing", "dir"))); got != "" {
		t.Errorf("a missing folder resolved to %q", got)
	}
}

func TestEncodingMatchesThisMachinesProjectFolders(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home folder")
	}
	entries, err := os.ReadDir(filepath.Join(home, ".claude", "projects"))
	if err != nil || len(entries) == 0 {
		t.Skip("no Claude Code projects on this machine")
	}
	app := &App{HomeDir: home, ClaudeDir: filepath.Join(home, ".claude"), StateDir: t.TempDir()}
	checked := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if path := cwdFromTranscripts(filepath.Join(app.projectsRoot(), entry.Name()), entry.Name()); path != "" {
			checked++
			if encodeProjectPath(path) != entry.Name() {
				t.Errorf("%s encodes to %s, but Claude Code named the folder %s", path, encodeProjectPath(path), entry.Name())
			}
		}
	}
	t.Logf("checked %d real project folders", checked)
}

func TestRoutes(t *testing.T) {
	f := newFixture(t)
	f.app.HomeDir = "/Users/me"
	config := &backupConfig{Routes: []backupRoute{
		{Pattern: "~/code/**", Destination: "personal"},
		{Pattern: "~/code/work/**", Destination: "work"},
		{Pattern: "/srv/*/app", Destination: "server"},
	}}
	cases := map[string]string{
		"/Users/me/code":              "personal",
		"/Users/me/code/misc/tool":    "personal",
		"/Users/me/code/work":         "work",
		"/Users/me/code/work/api/src": "work",
		"/Users/me/code/workshop":     "personal",
		"/Users/me/notes":             "",
		"/srv/one/app":                "server",
		"/srv/one/two/app":            "",
		"/Users/me/codex":             "",
		"":                            "",
	}
	for path, want := range cases {
		if got := f.app.destinationFor(config, path); got != want {
			t.Errorf("destinationFor(%q) = %q, want %q", path, got, want)
		}
	}
	if _, err := f.app.normalizePattern("code/**"); err == nil {
		t.Error("a relative route was accepted")
	}
}

func TestNormalizeRemote(t *testing.T) {
	same := []string{
		"git@github.com:balintb/claude-afterlife.git",
		"https://github.com/balintb/claude-afterlife.git",
		"https://github.com/balintb/claude-afterlife",
		"ssh://git@github.com/balintb/claude-afterlife.git",
		"ssh://git@github.com:22/balintb/claude-afterlife.git",
		"git+ssh://git@GitHub.com/balintb/claude-afterlife/",
		"https://user:token@github.com/balintb/claude-afterlife.git",
	}
	for _, url := range same {
		if got := normalizeRemote(url); got != "github.com/balintb/claude-afterlife" {
			t.Errorf("normalizeRemote(%q) = %q", url, got)
		}
	}
	if normalizeRemote("git@github.com:balintb/other.git") == normalizeRemote(same[0]) {
		t.Error("different repositories compared equal")
	}
	if got := normalizeRemote("/srv/git/project.git"); got != "/srv/git/project" {
		t.Errorf("local path normalized to %q", got)
	}
}

func testKey(t *testing.T) (backupKey, []age.Recipient) {
	t.Helper()
	secret, recipient, err := newBackupKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := parseBackupKey("# a comment\n" + secret + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if key.recipient != recipient {
		t.Fatalf("parsed recipient %s, want %s", key.recipient, recipient)
	}
	recipients, err := parseRecipients([]string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	return key, recipients
}

func TestSealRoundTripAndWrongKey(t *testing.T) {
	key, recipients := testKey(t)
	other, _ := testKey(t)
	plain := bytes.Repeat([]byte(`{"cwd":"/x","message":"hello"}`+"\n"), 1000)
	sealed, err := seal(plain, recipients)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hello")) {
		t.Fatal("sealed data contains plaintext")
	}
	if len(sealed) >= len(plain)/4 {
		t.Errorf("sealed %d bytes for %d bytes of repetitive text; compression is not working", len(sealed), len(plain))
	}
	opened, err := unseal(sealed, key.identities)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatalf("round trip failed: %v", err)
	}
	if _, err := unseal(sealed, other.identities); !errors.Is(err, errWrongKey) {
		t.Fatalf("wrong key gave %v, want errWrongKey", err)
	}
	corrupted := bytes.Clone(sealed)
	corrupted[len(corrupted)-5] ^= 0xff
	if _, err := unseal(corrupted, key.identities); err == nil || errors.Is(err, errWrongKey) {
		t.Fatalf("corrupted data gave %v, want a decryption error", err)
	}
}

func TestParseBackupKeyRejectsGarbage(t *testing.T) {
	for _, text := range []string{"", "not a key", "age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq"} {
		if _, err := parseBackupKey(text); err == nil {
			t.Errorf("parseBackupKey(%q) succeeded", text)
		}
	}
}

func TestIndexMACRejectsAForgedIndex(t *testing.T) {
	key, recipients := testKey(t)
	st := store{dir: t.TempDir()}
	macKey, _ := macKeyFor(key.secret)
	index := backupIndex{Format: backupFormat, MachineID: newMachineID(), UpdatedAt: time.Now()}
	if err := st.writeIndex(index, recipients, macKey); err != nil {
		t.Fatal(err)
	}
	if _, err := st.readIndex(index.MachineID, key); err != nil {
		t.Fatalf("genuine index rejected: %v", err)
	}

	forged := index
	forged.Hostname = "attacker"
	attackerMAC := make([]byte, 32)
	if err := st.writeIndex(forged, recipients, attackerMAC); err != nil {
		t.Fatal(err)
	}
	if _, err := st.readIndex(index.MachineID, key); !errors.Is(err, errTampered) {
		t.Fatalf("forged index gave %v, want errTampered", err)
	}

	_, macPath, _ := st.indexPaths(index.MachineID)
	os.Remove(macPath)
	if _, err := st.readIndex(index.MachineID, key); !errors.Is(err, errTampered) {
		t.Fatalf("index without a MAC gave %v, want errTampered", err)
	}
}

func TestStoreRejectsUnsafeNames(t *testing.T) {
	st := store{dir: t.TempDir()}
	for _, name := range []string{"../../etc/passwd", "ab/../../x", "AB/0123456789abcdef0123456789ab", ""} {
		if _, err := st.getObject(name); err == nil {
			t.Errorf("getObject(%q) did not refuse", name)
		}
	}
	if _, _, err := st.indexPaths("../x"); err == nil {
		t.Error("indexPaths accepted a path")
	}
}

func TestAssembleDetectsTampering(t *testing.T) {
	key, recipients := testKey(t)
	st := store{dir: t.TempDir()}
	data := []byte("line one\nline two\nline three\n")
	refs, err := st.putRange(bytes.NewReader(data), 0, int64(len(data)), recipients, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry := fileEntry{Path: "claude/x", Size: int64(len(data)), SHA256: sha256Hex(data), Segments: refs}
	var out bytes.Buffer
	if err := st.assemble(entry, key.identities, &out); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("assemble: %v", err)
	}

	lying := entry
	lying.SHA256 = sha256Hex([]byte("something else"))
	if err := st.assemble(lying, key.identities, &bytes.Buffer{}); !errors.Is(err, errCorruptedObject) {
		t.Errorf("wrong file hash gave %v", err)
	}
	wrongSegment := entry
	wrongSegment.Segments = []segmentRef{{Object: refs[0].Object, Offset: 0, Length: refs[0].Length, SHA256: sha256Hex([]byte("other"))}}
	if err := st.assemble(wrongSegment, key.identities, &bytes.Buffer{}); !errors.Is(err, errCorruptedObject) || !strings.Contains(err.Error(), refs[0].Object) {
		t.Errorf("a segment that does not match its hash gave %v, want an error naming the object", err)
	}
	gap := entry
	gap.Segments = []segmentRef{{Object: refs[0].Object, Offset: 5, Length: refs[0].Length, SHA256: refs[0].SHA256}}
	if err := st.assemble(gap, key.identities, &bytes.Buffer{}); !errors.Is(err, errCorruptedObject) {
		t.Errorf("segment gap gave %v", err)
	}
	path, _ := st.objectPath(refs[0].Object)
	os.Remove(path)
	if err := st.assemble(entry, key.identities, &bytes.Buffer{}); !errors.Is(err, errMissingObject) {
		t.Errorf("missing object gave %v", err)
	}
}

func TestPutRangeSplitsIntoSegments(t *testing.T) {
	defer func(limit int) { segmentLimit = limit }(segmentLimit)
	segmentLimit = 7
	key, recipients := testKey(t)
	st := store{dir: t.TempDir()}
	data := []byte("0123456789abcdefghij")
	refs, err := st.putRange(bytes.NewReader(data), 3, int64(len(data)), recipients, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantLengths := []int64{7, 7, 3}
	for i, ref := range refs {
		if ref.Length != wantLengths[i] || ref.Offset != 3+int64(7*i) {
			t.Errorf("segment %d = %+v", i, ref)
		}
	}
	plain, _ := unseal(mustRead(t, st, refs[1].Object), key.identities)
	if string(plain) != "abcdefg" {
		t.Errorf("segment 1 holds %q", plain)
	}
}

func mustRead(t *testing.T, st store, name string) []byte {
	t.Helper()
	data, err := st.getObject(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLastLineEnd(t *testing.T) {
	cases := []struct {
		data string
		from int64
		want int64
	}{
		{"", 0, 0},
		{"no newline", 0, 0},
		{"one\n", 0, 4},
		{"one\ntwo", 0, 4},
		{"one\ntwo\n", 4, 8},
		{"one\ntwo", 4, 4},
		{strings.Repeat("x", 200000) + "\n" + strings.Repeat("y", 100000), 0, 200001},
	}
	for _, c := range cases {
		got, err := lastLineEnd(strings.NewReader(c.data), c.from, int64(len(c.data)))
		if err != nil || got != c.want {
			t.Errorf("lastLineEnd(%.20q, %d) = %d, %v; want %d", c.data, c.from, got, err, c.want)
		}
	}
}

func testMapper() pathMapper {
	return newPathMapper([]projectMapping{
		{Old: "/old/home/code/api", New: "/new/src/api"},
		{Old: "/old/home/code", New: "/new/code"},
		{Old: "/old/home/skipped", New: ""},
	})
}

func TestPathMapperUsesTheLongestPrefix(t *testing.T) {
	mapper := testMapper()
	cases := map[string]string{
		"/old/home/code/api":     "/new/src/api",
		"/old/home/code/api/svc": "/new/src/api/svc",
		"/old/home/code/web":     "/new/code/web",
		"/old/home/code":         "/new/code",
		"/old/home/codex":        "/old/home/codex",
		"/old/home/skipped/x":    "/old/home/skipped/x",
		"/elsewhere":             "/elsewhere",
	}
	for path, want := range cases {
		if got, _ := mapper.mapPath(path); got != want {
			t.Errorf("mapPath(%q) = %q, want %q", path, got, want)
		}
	}
	if !mapper.movesAnything() || newPathMapper([]projectMapping{{Old: "/a", New: "/a"}}).movesAnything() {
		t.Error("movesAnything is wrong")
	}
}

func TestRewriteTopLevelString(t *testing.T) {
	mapper := testMapper()
	cases := []struct{ in, want string }{
		{`{"cwd":"/old/home/code/api","n":1}` + "\n", `{"cwd":"/new/src/api","n":1}` + "\n"},
		{`{"type":"user", "cwd" : "/old/home/code/api/svc" ,"x":[1,2]}` + "\n", `{"type":"user", "cwd" : "/new/src/api/svc" ,"x":[1,2]}` + "\n"},
		{`{"message":{"cwd":"/old/home/code/api"},"cwd":"/old/home/code/web"}` + "\n", `{"message":{"cwd":"/old/home/code/api"},"cwd":"/new/code/web"}` + "\n"},
		{`{"message":{"cwd":"/old/home/code/api"}}` + "\n", `{"message":{"cwd":"/old/home/code/api"}}` + "\n"},
		{`{"cwd":"/elsewhere"}` + "\n", `{"cwd":"/elsewhere"}` + "\n"},
		{`{"cwd":null}` + "\n", `{"cwd":null}` + "\n"},
		{`not json` + "\n", `not json` + "\n"},
		{`{"cwd":"/old/home/code"0`, `{"cwd":"/old/home/code"0`},
		{`{"cwd":"/old/home/code"} trailing`, `{"cwd":"/old/home/code"} trailing`},
		{`["cwd","/old/home/code"]` + "\n", `["cwd","/old/home/code"]` + "\n"},
		{`{"cwd":"\/old\/home\/code\/api","s":"<&>"}` + "\r\n", `{"cwd":"/new/src/api","s":"<&>"}` + "\r\n"},
		{`{"cwd":"/old/home/code/api"}`, `{"cwd":"/new/src/api"}`},
		{`{"a":"\"cwd\":\"/old/home/code\"","cwd":"/old/home/code"}` + "\n", `{"a":"\"cwd\":\"/old/home/code\"","cwd":"/new/code"}` + "\n"},
	}
	for _, c := range cases {
		if got := string(rewriteTopLevelString([]byte(c.in), "cwd", mapper.mapPath)); got != c.want {
			t.Errorf("rewrite(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func FuzzRewriteTopLevelString(f *testing.F) {
	for _, seed := range []string{
		`{"cwd":"/old/home/code/api/x","a":{"cwd":"/old/home/code"},"b":"é"}`,
		`{"x":1,"cwd":"/old/home/code"}`,
		`{"cwd":"/somewhere"}`,
		`[]`, `{}`, `{"cwd":1}`, `garbage`,
	} {
		f.Add(seed)
	}
	mapper := testMapper()
	f.Fuzz(func(t *testing.T, line string) {
		out := rewriteTopLevelString([]byte(line), "cwd", mapper.mapPath)
		if string(out) == line {
			return
		}
		var before, after map[string]any
		if err := json.Unmarshal([]byte(line), &before); err != nil {
			t.Fatalf("changed a line that is not a JSON object: %q -> %q", line, out)
		}
		if err := json.Unmarshal(out, &after); err != nil {
			t.Fatalf("produced invalid JSON: %q -> %q", line, out)
		}
		want, _ := mapper.mapPath(before["cwd"].(string))
		if after["cwd"] != want {
			t.Fatalf("cwd %q became %q, want %q", before["cwd"], after["cwd"], want)
		}
		delete(before, "cwd")
		delete(after, "cwd")
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("other fields changed: %q -> %q", line, out)
		}
	})
}

func TestMergeHistory(t *testing.T) {
	mapper := testMapper()
	local := []byte(`{"display":"new","project":"/new/code/web"}` + "\n")
	restored := []byte(`{"display":"old","project":"/old/home/code/web"}` + "\n" +
		`{"display":"new","project":"/old/home/code/web"}` + "\n" +
		`{"display":"half`)
	merged, added := mergeHistory(local, restored, mapper)
	want := `{"display":"old","project":"/new/code/web"}` + "\n" + `{"display":"new","project":"/new/code/web"}` + "\n"
	if string(merged) != want || added != 1 {
		t.Fatalf("merged %q (%d added), want %q", merged, added, want)
	}
	again, addedAgain := mergeHistory(merged, restored, mapper)
	if addedAgain != 0 || !bytes.Equal(again, merged) {
		t.Fatalf("merging twice added %d lines", addedAgain)
	}
}

func TestMergeSettings(t *testing.T) {
	local := []byte(`{"model":"opus","cleanupPeriodDays":10}`)
	restored := []byte(`{"model":"sonnet","hooks":{"Stop":[{"command":"bash /old/home/.claude/hooks/x.sh"}]},"statusLine":"/old/home","big":12345678901234567890}`)
	merged, added, changed, err := mergeSettings(local, restored, "/old/home", "/new/home", 3650)
	if err != nil || !changed {
		t.Fatalf("mergeSettings: %v, changed %v", err, changed)
	}
	var got map[string]any
	decoder := json.NewDecoder(bytes.NewReader(merged))
	decoder.UseNumber()
	if err := decoder.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "opus" {
		t.Errorf("local value lost: %v", got["model"])
	}
	if got["cleanupPeriodDays"] != json.Number("3650") {
		t.Errorf("cleanupPeriodDays = %v", got["cleanupPeriodDays"])
	}
	if got["statusLine"] != "/new/home" || !strings.Contains(string(merged), "bash /new/home/.claude/hooks/x.sh") {
		t.Errorf("home not remapped: %s", merged)
	}
	if got["big"] != json.Number("12345678901234567890") {
		t.Errorf("large number changed: %v", got["big"])
	}
	if !reflect.DeepEqual(added, []string{"big", "hooks", "statusLine"}) {
		t.Errorf("added = %v", added)
	}

	unchanged, _, changed, _ := mergeSettings(local, []byte(`{"model":"x"}`), "", "", 0)
	if changed || !bytes.Equal(unchanged, local) {
		t.Error("a merge that adds nothing rewrote the file")
	}
	if _, _, _, err := mergeSettings([]byte("{broken"), restored, "", "", 0); err == nil {
		t.Error("invalid local settings were overwritten")
	}
	higher, _, changed, _ := mergeSettings([]byte(`{"cleanupPeriodDays":99999}`), nil, "", "", 3650)
	if changed || !strings.Contains(string(higher), "99999") {
		t.Error("a higher cleanupPeriodDays was lowered")
	}
}

func TestMergeBoot(t *testing.T) {
	mapper := testMapper()
	t0 := time.Unix(1000, 0)
	restored := Boot{Format: storeFormat, BootID: "b", UpdatedAt: t0.Add(time.Hour), Running: []string{"s1"}, Sessions: map[string]Session{
		"s1": {ID: "s1", Cwd: "/old/home/code/api", LastSeen: t0.Add(time.Hour)},
		"s2": {ID: "s2", Cwd: "/old/home/code/web", LastSeen: t0},
	}}
	fresh := mergeBoot(nil, restored, mapper)
	if fresh.Sessions["s1"].Cwd != "/new/src/api" || fresh.Sessions["s2"].Cwd != "/new/code/web" {
		t.Fatalf("cwd not remapped: %+v", fresh.Sessions)
	}
	local := &Boot{Format: storeFormat, BootID: "b", UpdatedAt: t0, Running: []string{"s3"}, Sessions: map[string]Session{
		"s2": {ID: "s2", Cwd: "/new/code/web", LastSeen: t0.Add(2 * time.Hour)},
		"s3": {ID: "s3", Cwd: "/new/x", LastSeen: t0},
	}}
	merged := mergeBoot(local, restored, mapper)
	if len(merged.Sessions) != 3 || !merged.Sessions["s2"].LastSeen.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("merge kept the wrong sessions: %+v", merged.Sessions)
	}
	if !reflect.DeepEqual(merged.Running, []string{"s1", "s3"}) || !merged.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("running %v, updated %v", merged.Running, merged.UpdatedAt)
	}
}

func TestPrefixRelation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	big := strings.Repeat("z", 200000)
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"abc", "abcd", -1},
		{"abcd", "abc", 1},
		{"abX", "abc", 2},
		{big, big + "more", -1},
		{big + "x", big + "y", 2},
		{big, big, 0},
	}
	for i, c := range cases {
		got, err := prefixRelation(write("a", c.a), write("b", c.b))
		if err != nil || got != c.want {
			t.Errorf("case %d: prefixRelation = %d, %v; want %d", i, got, err, c.want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	if compareVersions("2.1.292", "2.1.29") <= 0 || compareVersions("2.1.9", "2.1.10") >= 0 || compareVersions("1.0", "1.0.0") != 0 {
		t.Error("compareVersions is wrong")
	}
}

func TestIsSessionPath(t *testing.T) {
	for path, want := range map[string]bool{
		"claude/projects/-a/1234.jsonl":                 true,
		"claude/projects/-a/1234/subagents/agent.jsonl": false,
		"claude/projects/-a/memory/MEMORY.md":           false,
		"claude/history.jsonl":                          false,
	} {
		if isSessionPath(path) != want {
			t.Errorf("isSessionPath(%q) != %v", path, want)
		}
	}
}

func TestMacKeyIsDerivedFromThePrivateKey(t *testing.T) {
	a, _ := testKey(t)
	b, _ := testKey(t)
	ka, _ := macKeyFor(a.secret)
	kb, _ := macKeyFor(b.secret)
	again, _ := macKeyFor(a.secret)
	if hex.EncodeToString(ka) != hex.EncodeToString(again) || bytes.Equal(ka, kb) {
		t.Error("MAC keys are not a stable function of the private key")
	}
}
