package gitworktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
)

// Program is a running program the busy check looks at.
type Program struct {
	PID     int    `json:"pid"`
	Command string `json:"command"`
	// Dir is the program's working folder, when the system let this process read it.
	Dir string `json:"dir,omitempty"`
}

// ProgramLister lists the running programs, with their command lines and working folders.
type ProgramLister interface {
	Programs(ctx context.Context) ([]Program, error)
}

// SystemPrograms lists the running programs on macOS and Linux with no outside tool, except /bin/ps on macOS.
type SystemPrograms struct{}

// Programs lists every running program with its command line and working folder. It reads working folders with
// gopsutil (libproc on macOS, /proc on Linux), which sees only this user's programs, as lsof does without root.
// Command lines come from commandLines, which on macOS also sees root's and other users' programs.
func (SystemPrograms) Programs(ctx context.Context) ([]Program, error) {
	if ctx == nil {
		return nil, errors.New("gitworktree: nil context")
	}
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("gitworktree: list processes: %w", err)
	}
	commands, err := commandLines(ctx, procs)
	if err != nil {
		return nil, err
	}
	list := make([]Program, 0, len(commands))
	for pid, command := range commands {
		list = append(list, Program{PID: pid, Command: command})
	}
	dirs := make(map[int]string, len(procs))
	for _, p := range procs {
		// Another user's working folder is unreadable without root, so the error only means "not visible".
		if dir, err := p.CwdWithContext(ctx); err == nil {
			dirs[int(p.Pid)] = dir
		}
	}
	for i := range list {
		list[i].Dir = dirs[list[i].PID]
		delete(dirs, list[i].PID)
	}
	// A program whose command line was unreadable still counts by its working folder.
	for pid, dir := range dirs {
		list = append(list, Program{PID: pid, Dir: dir})
	}
	return list, nil
}

// programs is every running program, read once per method.
type programs []Program

// using returns the programs, other than this one, whose command line names dir or a path inside it, or whose
// working folder is inside it.
func (ps programs) using(dir string) []Program {
	dirs := []string{dir}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
		dirs = append(dirs, resolved)
	}
	self := os.Getpid()
	var found []Program
	for _, p := range ps {
		if p.PID == self {
			continue
		}
		for _, d := range dirs {
			if mentions(p.Command, d) || inside(p.Dir, d) {
				found = append(found, p)
				break
			}
		}
	}
	return found
}

// mentions reports whether a command line names dir, or a path inside it, as a whole path.
func mentions(command, dir string) bool {
	for rest := command; ; {
		i := strings.Index(rest, dir)
		if i < 0 {
			return false
		}
		after := rest[i+len(dir):]
		if after == "" || after[0] == '/' || after[0] == ' ' || after[0] == '"' || after[0] == '\'' {
			return true
		}
		rest = after
	}
}

// inside reports whether path is dir or below it.
func inside(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}
