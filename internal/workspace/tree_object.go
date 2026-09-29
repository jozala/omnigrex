package workspace

import (
	"context"
	"encoding/hex"
	"strings"
)

// validatePublicationTree checks the raw objects, rather than ls-tree's
// formatted output, so duplicate names, unsorted entries, and noncanonical
// mode encodings in intermediate commits cannot be hidden by a clean tip.
func (lifecycle *Lifecycle) validatePublicationTree(ctx context.Context, directory, commit string, validated map[string]bool) error {
	root, err := lifecycle.gitOutput(ctx, "resolve outgoing commit tree", directory, "", nil,
		"rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return err
	}
	root, err = normalizeObjectID(root)
	if err != nil {
		return ErrInvalidPublicationHistory
	}
	return lifecycle.validateRawTree(ctx, directory, root, len(root)/2, validated)
}

func (lifecycle *Lifecycle) validateRawTree(ctx context.Context, directory, id string, objectIDBytes int, validated map[string]bool) error {
	if validated[id] {
		return nil
	}
	raw, err := lifecycle.gitOutput(ctx, "inspect raw publication tree", directory, "", nil,
		"cat-file", "tree", id)
	if err != nil {
		return ErrInvalidPublicationHistory
	}
	seen := make(map[string]bool)
	previous := ""
	for raw != "" {
		space := strings.IndexByte(raw, ' ')
		if space < 1 {
			return ErrInvalidPublicationHistory
		}
		mode := raw[:space]
		end := strings.IndexByte(raw[space+1:], 0)
		if end < 0 {
			return ErrInvalidPublicationHistory
		}
		end += space + 1
		name := raw[space+1 : end]
		raw = raw[end+1:]
		if len(raw) < objectIDBytes || name == "" || name == "." || name == ".." || name == ".git" ||
			strings.ContainsRune(name, '/') || seen[name] {
			return ErrInvalidPublicationHistory
		}
		seen[name] = true
		directoryEntry := mode == "40000"
		if !directoryEntry && mode != "100644" && mode != "100755" && mode != "120000" {
			return ErrInvalidPublicationHistory
		}
		orderKey := name
		if directoryEntry {
			orderKey += "/"
		}
		if previous != "" && orderKey <= previous {
			return ErrInvalidPublicationHistory
		}
		previous = orderKey
		child := raw[:objectIDBytes]
		raw = raw[objectIDBytes:]
		if strings.Trim(child, "\x00") == "" {
			return ErrInvalidPublicationHistory
		}
		if directoryEntry {
			if err := lifecycle.validateRawTree(ctx, directory, hex.EncodeToString([]byte(child)), objectIDBytes, validated); err != nil {
				return err
			}
		}
	}
	validated[id] = true
	return nil
}
