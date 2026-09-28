package packet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const (
	MaxPatch       = 3 << 20
	MaxFileContent = 1 << 20
	MaxFilesTotal  = 2 << 20
	MaxPaths       = 5000
	MaxManifest    = 512 << 10
	MaxQuestion    = 64 << 10
	MaxEncoded     = 8 << 20
)

type Change struct {
	Status   string `json:"status"`
	Path     string `json:"path"`
	From     string `json:"from,omitempty"`
	OldMode  string `json:"old_mode,omitempty"`
	NewMode  string `json:"new_mode,omitempty"`
	Included bool   `json:"included"`
	Reason   string `json:"reason,omitempty"`
}

type Manifest struct {
	RepoID       string    `json:"repo_id,omitempty"`
	Scope        string    `json:"scope"`
	BaseKind     string    `json:"base_kind"`
	BaseCommit   string    `json:"base_commit,omitempty"`
	TargetTree   string    `json:"target_tree,omitempty"`
	IncludedTree string    `json:"included_tree,omitempty"`
	Captured     time.Time `json:"captured"`
	Complete     bool      `json:"complete"`
	Notes        []string  `json:"notes,omitempty"`
	Changes      []Change  `json:"changes"`
	FilesOmitted []string  `json:"files_omitted,omitempty"`
}

// Packet is everything a reviewer receives about a change.
type Packet struct {
	Manifest Manifest          `json:"manifest"`
	Patch    []byte            `json:"patch,omitempty"`
	Files    map[string][]byte `json:"files,omitempty"`
	Question string            `json:"question"`
}

// Hash identifies a packet in job identity: SHA-256 of its JSON encoding
// (encoding/json sorts map keys, so equal packets hash equally).
func Hash(p *Packet) string {
	data, _ := json.Marshal(p)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Validate applies the limits and path policy a receiver enforces before
// materializing anything.
func (p *Packet) Validate() error {
	if len(p.Question) > MaxQuestion {
		return fmt.Errorf("question exceeds %d bytes", MaxQuestion)
	}
	if len(p.Patch) > MaxPatch {
		return fmt.Errorf("patch exceeds %d bytes", MaxPatch)
	}
	if len(p.Manifest.Changes) > MaxPaths {
		return fmt.Errorf("more than %d changed paths", MaxPaths)
	}
	if data, _ := json.Marshal(p.Manifest); len(data) > MaxManifest {
		return fmt.Errorf("manifest exceeds %d bytes", MaxManifest)
	}
	var included []string
	for _, c := range p.Manifest.Changes {
		if !c.Included {
			continue
		}
		if err := ValidPath(c.Path); err != nil {
			return err
		}
		included = append(included, c.Path)
		if c.From != "" {
			if err := ValidPath(c.From); err != nil {
				return err
			}
		}
	}
	if col := FoldCollisions(included); len(col) > 0 {
		for a, b := range col {
			return fmt.Errorf("paths %q and %q collide when case is ignored", a, b)
		}
	}
	total := 0
	var files []string
	for path, b := range p.Files {
		if err := ValidPath(path); err != nil {
			return err
		}
		if len(b) > MaxFileContent {
			return fmt.Errorf("file %s exceeds %d bytes", path, MaxFileContent)
		}
		total += len(b)
		files = append(files, path)
	}
	if total > MaxFilesTotal {
		return fmt.Errorf("file contents exceed %d bytes", MaxFilesTotal)
	}
	if col := FoldCollisions(files); len(col) > 0 {
		return fmt.Errorf("packet files collide when case is ignored")
	}
	return nil
}
