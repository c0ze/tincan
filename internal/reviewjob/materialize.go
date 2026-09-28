package reviewjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/c0ze/tincan/v2/internal/packet"
)

// writeFileExclusive creates root/rel as a new 0600 regular file. Every
// directory on the way is created here or must be a real directory (never a
// symlink), and the file itself must not exist (committees §6.6).
func writeFileExclusive(root, rel string, data []byte) error {
	parts := strings.Split(rel, "/")
	dir := root
	for _, c := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, c)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(dir, 0o700); err != nil {
				return err
			}
		case err != nil:
			return err
		case !info.IsDir() || info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%s is not a plain directory", dir)
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, parts[len(parts)-1]), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// materializePacket builds a packet-only workspace and returns its note.
func materializePacket(ws string, p *packet.Packet, reason string) (string, error) {
	if err := p.Validate(); err != nil {
		return "", invalid(err.Error())
	}
	if err := os.MkdirAll(ws, 0o700); err != nil {
		return "", err
	}
	manifest, _ := json.MarshalIndent(p.Manifest, "", "  ")
	files := map[string][]byte{"packet/manifest.json": manifest, "packet/question.md": []byte(p.Question)}
	if len(p.Patch) > 0 {
		files["packet/diff.patch"] = p.Patch
	}
	for path, b := range p.Files {
		files["packet/files/"+path] = b
	}
	for rel, b := range files {
		if err := packet.ValidPath(rel); err != nil {
			return "", invalid(err.Error())
		}
		if err := writeFileExclusive(ws, rel, b); err != nil {
			return "", err
		}
	}
	note := "Your working directory holds a packet describing the change; this is not a checkout"
	if reason != "" {
		note += " (" + reason + ")"
	}
	note += ". Read packet/manifest.json (changed paths, omissions), packet/diff.patch (the change, when present) and packet/files/ (post-change contents of changed text files)."
	if !p.Manifest.Complete {
		note += " The packet is partial: the full patch was too large."
	}
	if len(note) > 4096 {
		note = note[:4096]
	}
	return note, nil
}
