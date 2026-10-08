package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func runGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return "", errors.New(message)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (a *App) initStoreRepo(dir, remote string) error {
	if _, err := a.Git(dir, "init", "-q"); err != nil {
		return err
	}
	if _, err := a.Git(dir, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return err
	}
	if remote != "" {
		if _, err := a.Git(dir, "remote", "add", "origin", remote); err != nil {
			return err
		}
	}
	return nil
}

// commitStore commits everything in the store with a fixed identity, without
// signing or hooks, so the user's git configuration cannot get in the way.
func (a *App) commitStore(st store, message string) (bool, error) {
	status, err := a.Git(st.dir, "status", "--porcelain")
	if err != nil || status == "" {
		return false, err
	}
	if _, err := a.Git(st.dir, "add", "-A"); err != nil {
		return false, err
	}
	_, err = a.Git(st.dir,
		"-c", "user.name=claude-afterlife", "-c", "user.email=claude-afterlife@localhost", "-c", "commit.gpgsign=false",
		"commit", "-q", "--no-verify", "-m", message)
	return err == nil, err
}

func (a *App) pushStore(st store) error {
	_, err := a.Git(st.dir, "push", "-q", "origin", "HEAD:refs/heads/main")
	return err
}

func (a *App) cloneStore(remote string) (store, func(), error) {
	parent, err := os.MkdirTemp("", "claude-afterlife-")
	if err != nil {
		return store{}, nil, err
	}
	cleanup := func() { os.RemoveAll(parent) }
	if _, err := a.Git(parent, "clone", "-q", "--depth", "1", remote, "store"); err != nil {
		cleanup()
		return store{}, nil, err
	}
	return store{dir: filepath.Join(parent, "store")}, cleanup, nil
}

func (a *App) originURL(repo string) string {
	if url, err := a.Git(repo, "config", "--get", "remote.origin.url"); err == nil && url != "" {
		return url
	}
	names, err := a.Git(repo, "remote")
	if err != nil || names == "" {
		return ""
	}
	url, _ := a.Git(repo, "config", "--get", "remote."+strings.Fields(names)[0]+".url")
	return url
}

// repoInfo records where a project's code came from, never the code itself.
func (a *App) repoInfo(path string) projectInfo {
	info := projectInfo{Path: path}
	if !isDir(path) {
		return info
	}
	top, err := a.Git(path, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return info
	}
	info.RepoRoot = sameFormRoot(path, top)
	info.Remote = a.originURL(top)
	info.Branch, _ = a.Git(top, "rev-parse", "--abbrev-ref", "HEAD")
	info.Head, _ = a.Git(top, "rev-parse", "HEAD")
	status, _ := a.Git(top, "status", "--porcelain")
	info.Dirty = status != ""
	return info
}

// sameFormRoot returns the repository root spelled like path, so that a project at
// /var/folders/... is not compared against git's /private/var/folders/... answer.
func sameFormRoot(path, top string) string {
	realPath, err1 := filepath.EvalSymlinks(path)
	realTop, err2 := filepath.EvalSymlinks(top)
	if err1 != nil || err2 != nil {
		return top
	}
	rel, err := filepath.Rel(realTop, realPath)
	if err != nil || !filepath.IsLocal(rel) && rel != "." {
		return top
	}
	if rel == "." {
		return path
	}
	if strings.HasSuffix(path, string(filepath.Separator)+rel) {
		return strings.TrimSuffix(path, string(filepath.Separator)+rel)
	}
	return top
}

func (a *App) unpushedBranches(repo string) []string {
	output, err := a.Git(repo, "for-each-ref", "--format=%(refname:short)%09%(upstream:short)%09%(upstream:track)", "refs/heads")
	if err != nil {
		return nil
	}
	var branches []string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 || fields[0] == "" {
			continue
		}
		switch {
		case fields[1] == "":
			branches = append(branches, fields[0]+" (never pushed)")
		case strings.Contains(fields[2], "ahead"):
			branches = append(branches, fields[0]+" "+fields[2])
		}
	}
	return branches
}

// normalizeRemote makes the SSH and HTTPS spellings of one repository compare equal.
func normalizeRemote(url string) string {
	u := strings.TrimSpace(url)
	for _, scheme := range []string{"git+ssh://", "ssh://", "https://", "http://", "git://", "file://"} {
		if strings.HasPrefix(strings.ToLower(u), scheme) {
			u = u[len(scheme):]
			break
		}
	}
	slash := strings.Index(u, "/")
	if at := strings.Index(u, "@"); at >= 0 && (slash < 0 || at < slash) {
		u = u[at+1:]
		slash = strings.Index(u, "/")
	}
	if colon := strings.Index(u, ":"); colon >= 0 && (slash < 0 || colon < slash) {
		rest := u[colon+1:]
		port := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
		if port > 0 && rest[port] == '/' {
			u = u[:colon] + rest[port:]
		} else {
			u = u[:colon] + "/" + rest
		}
	}
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	return strings.ToLower(strings.TrimSuffix(u, "/"))
}

var skippedSearchDirs = map[string]bool{
	"node_modules": true, "Library": true, "Applications": true, "Pictures": true, "Movies": true,
	"Music": true, "vendor": true, "Pods": true, "target": true, "dist": true, "build": true,
}

// findRepos maps normalized remote URLs to the repositories found under roots.
func (a *App) findRepos(roots []string, maxDepth int) map[string][]string {
	repos := map[string][]string{}
	for _, root := range roots {
		root = filepath.Clean(a.expandHome(root))
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if path == root {
					return err
				}
				return fs.SkipDir
			}
			if !entry.IsDir() {
				return nil
			}
			if path != root && (strings.HasPrefix(entry.Name(), ".") || skippedSearchDirs[entry.Name()]) {
				return fs.SkipDir
			}
			if depth := strings.Count(strings.TrimPrefix(path, root), string(filepath.Separator)); depth > maxDepth {
				return fs.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
				if url := a.originURL(path); url != "" {
					key := normalizeRemote(url)
					repos[key] = append(repos[key], path)
				}
			}
			return nil
		})
	}
	for key := range repos {
		sort.Strings(repos[key])
	}
	return repos
}
