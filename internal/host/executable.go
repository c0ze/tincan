package host

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// UserBinDirs lists per-user and package-manager bin directories where agent
// CLIs are commonly installed. GUI-launched MCP clients and detached hosts
// often inherit a minimal PATH that omits them, so tincan searches them after
// PATH. Only existing directories are returned.
func UserBinDirs() []string {
	home, _ := os.UserHomeDir()
	var dirs []string
	add := func(parts ...string) {
		if parts[0] == "" {
			return
		}
		dirs = append(dirs, filepath.Join(parts...))
	}
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		add(gobin)
	}
	if mise := os.Getenv("MISE_DATA_DIR"); mise != "" {
		add(mise, "shims")
	}
	if home != "" {
		add(home, ".local", "bin")
		add(home, ".local", "share", "mise", "shims")
		add(home, ".asdf", "shims")
		add(home, ".volta", "bin")
		add(home, ".bun", "bin")
		add(home, ".npm-global", "bin")
		add(home, ".cargo", "bin")
		add(home, "go", "bin")
	}
	switch runtime.GOOS {
	case "windows":
		add(os.Getenv("APPDATA"), "npm")
		if home != "" {
			add(home, "scoop", "shims")
		}
	default:
		add("/opt/homebrew/bin")
		add("/usr/local/bin")
		add("/home/linuxbrew/.linuxbrew/bin")
	}
	out := dirs[:0]
	for _, d := range dirs {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// AugmentPATH appends the existing UserBinDirs that PATH does not already
// contain, so agent CLIs and the tools they spawn (node, git, …) resolve the
// same way under a GUI client or a detached host as in a login shell. Entries
// already on PATH keep their precedence.
func AugmentPATH() {
	current := os.Getenv("PATH")
	seen := map[string]bool{}
	for _, d := range filepath.SplitList(current) {
		seen[filepath.Clean(d)] = true
	}
	parts := []string{}
	if current != "" {
		parts = append(parts, current)
	}
	for _, d := range UserBinDirs() {
		if !seen[filepath.Clean(d)] {
			parts = append(parts, d)
			seen[filepath.Clean(d)] = true
		}
	}
	os.Setenv("PATH", strings.Join(parts, string(os.PathListSeparator)))
}

// ResolveExecutable finds the program for an exec template's argv[0], as run
// from dir. A bare name is looked up on PATH, then in UserBinDirs. A relative
// path is resolved against dir. An absolute path that no longer exists —
// typically a config copied from another machine or written before a
// version-manager switch — falls back to looking up its base name, so a stale
// path does not take a preset offline. An existing but unusable file (a
// wrapper without execute permission, say) is reported, never substituted.
func ResolveExecutable(name, dir string) (string, error) {
	hasSep := strings.ContainsAny(name, `/\`)
	if hasSep && !filepath.IsAbs(name) {
		if dir == "" {
			dir = "."
		}
		return exec.LookPath(filepath.Join(dir, name))
	}
	path, err := exec.LookPath(name)
	if err == nil {
		return path, nil
	}
	base := name
	if hasSep {
		if _, serr := os.Stat(name); !errors.Is(serr, fs.ErrNotExist) {
			return "", err
		}
		base = filepath.Base(name)
		if p, lerr := exec.LookPath(base); lerr == nil {
			return p, nil
		}
	}
	for _, d := range UserBinDirs() {
		if p, lerr := exec.LookPath(filepath.Join(d, base)); lerr == nil {
			return p, nil
		}
	}
	return "", err
}
