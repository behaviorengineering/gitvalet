//go:build darwin

package gitworktree

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// cloneTree makes dst an APFS clone of the folder src, with every file under it, in one call. The target must not
// exist yet. A volume that is not APFS, or a target on another volume, returns ErrUnsupported.
func cloneTree(src, dst string) error {
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	return err
}
