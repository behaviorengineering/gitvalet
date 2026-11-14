//go:build darwin

package gitworktree

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
)

// ps is part of macOS and setuid root, so unlike libproc it reads every program's command line.
const ps = "/bin/ps"

// commandLines reads every program's command line with /bin/ps, by process ID.
func commandLines(ctx context.Context, _ []*process.Process) (map[int]string, error) {
	out, err := exec.CommandContext(ctx, ps, "-A", "-ww", "-o", "pid=,command=").Output() // #nosec G204 -- a fixed system path and fixed arguments.
	if err != nil {
		return nil, fmt.Errorf("gitworktree: %s: %w", ps, err)
	}
	commands := map[int]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		pidText, command, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			continue // not a process line
		}
		commands[pid] = strings.TrimSpace(command)
	}
	return commands, nil
}
