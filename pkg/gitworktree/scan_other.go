//go:build !darwin

package gitworktree

import "context"

// scanTree needs APFS, which only macOS has, to say which blocks a file shares.
func scanTree(context.Context, string, func(fileBlocks)) error {
	return ErrUnsupported
}
