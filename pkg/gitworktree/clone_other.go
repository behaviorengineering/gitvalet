//go:build !darwin

package gitworktree

// cloneTree needs APFS clones, which only macOS has. Btrfs and XFS reflinks could stand in later.
func cloneTree(_, _ string) error {
	return ErrUnsupported
}

// canCloneInto is false: cloneTree cannot clone here.
func canCloneInto(_, _ string) bool { return false }
