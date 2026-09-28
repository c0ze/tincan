package packet

import (
	"strings"
	"testing"
)

func TestValidPath(t *testing.T) {
	for _, ok := range []string{"a.go", "dir/sub/file.txt", "a/.github/workflows/ci.yml", ".gitignore", "x/.gitkeep"} {
		if err := ValidPath(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/abs", "../x", "a/../b", "a//b", "./a", "a/", ".git/config", "x/.GIT/hooks/pre-commit",
		".tincan/requests/x", "a/.TinCan/b", "con/a", "a:b", strings.Repeat("a", 1025), "a\\b", "a/b\x00"} {
		if err := ValidPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFoldCollisions(t *testing.T) {
	got := FoldCollisions([]string{"README.md", "src/a.go", "readme.MD", "SRC/A.go", "b"})
	if got["readme.MD"] != "README.md" || got["SRC/A.go"] != "src/a.go" || len(got) != 2 {
		t.Fatalf("collisions: %v", got)
	}
}
