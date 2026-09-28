package packet

import (
	"errors"
	"fmt"
	"strings"

	"github.com/c0ze/tincan/v2/internal/envelope"
)

// ValidPath applies the committees §6.6 path policy to one
// repository-relative path, on both requester and receiver.
func ValidPath(p string) error {
	if p == "" || len(p) > 1024 {
		return errors.New("path is empty or longer than 1024 bytes")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return fmt.Errorf("path %q is not relative", p)
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." {
			return fmt.Errorf("path %q is not clean", p)
		}
		if f := strings.ToLower(c); f == ".git" || f == ".tincan" {
			return fmt.Errorf("path %q enters %s", p, c)
		}
		if err := envelope.ValidComponent(c); err != nil {
			return fmt.Errorf("path %q: %w", p, err)
		}
	}
	return nil
}

// FoldCollisions returns, for every path that case-folds to the same string
// as an earlier one, that earlier path.
func FoldCollisions(paths []string) map[string]string {
	seen := map[string]string{}
	out := map[string]string{}
	for _, p := range paths {
		k := strings.ToLower(p)
		if first, ok := seen[k]; ok {
			out[p] = first
			continue
		}
		seen[k] = p
	}
	return out
}
