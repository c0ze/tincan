package packet

import (
	"context"
	"testing"
)

func TestRepoIDNormalizes(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:c0ze/tincan.git":              "github.com/c0ze/tincan",
		"ssh://git@GitHub.com/c0ze/tincan.git":        "github.com/c0ze/tincan",
		"ssh://git@github.com:22/c0ze/tincan":         "github.com/c0ze/tincan",
		"https://user:tok@github.com/c0ze/tincan.git": "github.com/c0ze/tincan",
		"https://github.com:443/c0ze/tincan/":         "github.com/c0ze/tincan",
		"https://git.example.com:8443/team/repo.git":  "git.example.com:8443/team/repo",
		"ssh://git@git.example.com:2222/team/repo":    "git.example.com:2222/team/repo",
		"":            "",
		"not a url":   "",
		"/local/path": "",
	} {
		if got := RepoID(in); got != want {
			t.Errorf("RepoID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoIDOfReadsOrigin(t *testing.T) {
	dir, git := testRepo(t)
	if got := RepoIDOf(context.Background(), dir); got != "" {
		t.Fatalf("no origin: %q", got)
	}
	git("remote", "add", "origin", "git@github.com:c0ze/tincan.git")
	if got := RepoIDOf(context.Background(), dir); got != "github.com/c0ze/tincan" {
		t.Fatalf("origin: %q", got)
	}
}
