package packet

import (
	"context"
	"net/url"
	"strings"
)

// RepoID normalizes a remote URL into a credential-free repository identity
// (committees §6.6): lower-case host, non-default port kept, path without
// ".git" and trailing "/"; scp-style, ssh:// and https:// forms agree.
func RepoID(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	var host, port, path string
	if u, err := url.Parse(remote); err == nil && u.Scheme != "" && u.Host != "" {
		host, port, path = strings.ToLower(u.Hostname()), u.Port(), u.Path
		if (port == "22" && u.Scheme == "ssh") || (port == "443" && u.Scheme == "https") || (port == "80" && u.Scheme == "http") {
			port = ""
		}
	} else if at, colon := strings.Index(remote, "@"), strings.Index(remote, ":"); colon > 0 && !strings.Contains(remote[:colon], "/") && (at < 0 || at < colon) {
		// scp-style: [user@]host:path
		host, path = strings.ToLower(remote[at+1:colon]), remote[colon+1:]
	} else {
		return ""
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	path = strings.Trim(path, "/")
	if host == "" || path == "" || strings.ContainsAny(host, " /") {
		return ""
	}
	if port != "" {
		host += ":" + port
	}
	return host + "/" + path
}

// RepoIDOf is RepoID of root's origin remote, or "" without one.
func RepoIDOf(ctx context.Context, root string) string {
	remote, err := Git{Dir: root}.Out(ctx, "config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return RepoID(remote)
}
