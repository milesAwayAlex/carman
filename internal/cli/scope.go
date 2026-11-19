package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// A repository's scope has to be the same wherever the repository is checked
// out: on the laptop, in a worktree, in a worker's fresh clone under some
// other directory name. So it is taken from where the repository comes from,
// its `origin` remote, and not from the directory it happens to be in:
//
//	git@github.com:Acme/Billing.git      ->  repo:github.com/acme/billing
//	https://github.com/acme/billing      ->  repo:github.com/acme/billing
//
// A repository with no remote has nothing of the kind, and falls back to the
// directory name of its main checkout, which is at least the same for all of
// its worktrees. Outside any repository it is the directory's own name.
//
// The git files are read directly. The hook runs many times a session, and
// it has to work where git is not installed or refuses a directory it does
// not own.

// repositoryScope returns the scope tag of the repository dir belongs to.
func repositoryScope(dir string) string {
	common, ok := gitCommonDir(dir)
	if !ok {
		return "repo:" + filepath.Base(dir)
	}
	if name, ok := canonicalRemote(remoteURL(common)); ok {
		return "repo:" + name
	}
	if filepath.Base(common) == ".git" {
		return "repo:" + filepath.Base(filepath.Dir(common))
	}
	// A bare repository, or a submodule kept under its parent's .git/modules.
	return "repo:" + strings.TrimSuffix(filepath.Base(common), ".git")
}

// gitCommonDir finds the directory holding the git data shared by every
// worktree of the repository that dir is in.
func gitCommonDir(dir string) (string, bool) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		dotGit := filepath.Join(dir, ".git")
		if info, err := os.Stat(dotGit); err == nil {
			if info.IsDir() {
				return dotGit, true
			}
			// A linked worktree or a submodule: .git is a file that says
			// where its own git data is, and that in turn may say where the
			// shared data is.
			data, err := os.ReadFile(dotGit)
			gitDir, isPointer := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
			if err != nil || !isPointer {
				return "", false
			}
			gitDir = strings.TrimSpace(gitDir)
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(dir, gitDir)
			}
			if shared, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
				sharedDir := strings.TrimSpace(string(shared))
				if !filepath.IsAbs(sharedDir) {
					sharedDir = filepath.Join(gitDir, sharedDir)
				}
				return filepath.Clean(sharedDir), true
			}
			return filepath.Clean(gitDir), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// remoteURL reads the URL of the `origin` remote from a repository's config,
// or of its only other remote when there is no origin.
func remoteURL(commonDir string) string {
	data, err := os.ReadFile(filepath.Join(commonDir, "config"))
	if err != nil {
		return ""
	}
	urls := map[string]string{}
	var order []string
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = ""
			if rest, ok := strings.CutPrefix(line, `[remote "`); ok {
				section, _, _ = strings.Cut(rest, `"`)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if section != "" && ok && strings.TrimSpace(key) == "url" {
			if _, seen := urls[section]; !seen {
				order = append(order, section)
			}
			urls[section] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	if u := urls["origin"]; u != "" {
		return u
	}
	if len(order) == 1 {
		return urls[order[0]]
	}
	return ""
}

// canonicalRemote turns a remote's URL into host/path, lower-cased, with no
// scheme, user, password, port or .git suffix, so that the ssh and https
// forms of one repository agree. A remote that is a path on this machine is
// not a name anything else would share, and is refused.
func canonicalRemote(remote string) (string, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", false
	}
	var host, path string
	switch {
	case strings.Contains(remote, "://"):
		u, err := url.Parse(remote)
		if err != nil || u.Hostname() == "" || u.Scheme == "file" {
			return "", false
		}
		host, path = u.Hostname(), u.Path
	case strings.HasPrefix(remote, "/") || strings.HasPrefix(remote, "."):
		return "", false
	default:
		// user@host:path, as ssh takes it.
		at, rest, ok := strings.Cut(remote, ":")
		if !ok {
			return "", false
		}
		if _, h, hasUser := strings.Cut(at, "@"); hasUser {
			at = h
		}
		host, path = at, rest
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || path == "" || strings.ContainsAny(host, "/ ") {
		return "", false
	}
	return strings.ToLower(host + "/" + path), true
}

// scope prints the scope tag of the repository a directory belongs to, for
// use where a scope has to be typed: `cmem import --scope "$(cmem scope DIR)"`.
func (c *command) scope(args []string) error {
	if _, err := c.parse(args); err != nil {
		return err
	}
	dir := c.flags.Arg(0)
	if dir == "" {
		var err error
		if dir, err = c.workingDir(); err != nil {
			return err
		}
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return c.print(c.sessionScope(dir), func() {
		for _, tag := range c.sessionScope(dir) {
			fmt.Fprintln(c.stdout, tag)
		}
	})
}

// workingDir is where the command was run. CMEM_DIR stands in for it when a
// harness runs commands from somewhere other than the session's directory.
func (c *command) workingDir() (string, error) {
	if dir := c.getenv("CMEM_DIR"); dir != "" {
		return dir, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", errors.New("cannot tell which directory this is; name it")
	}
	return dir, nil
}
