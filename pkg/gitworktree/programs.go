package gitworktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Program is a running program the busy check looks at.
type Program struct {
	PID     int    `json:"pid"`
	Command string `json:"command"`
	// Dir is the program's working folder, when the system reported it.
	Dir string `json:"dir,omitempty"`
}

// ProgramLister lists the running programs, with their command lines and working folders.
type ProgramLister interface {
	Programs(ctx context.Context) ([]Program, error)
}

// SystemPrograms lists the running programs with ps and lsof, on macOS and Linux.
type SystemPrograms struct{}

// Programs reads every program's command line with ps and its working folder with lsof. When lsof cannot read some
// programs, such as other users', it exits 1 and still prints the rest, so its output is kept.
func (SystemPrograms) Programs(ctx context.Context) ([]Program, error) {
	if ctx == nil {
		return nil, errors.New("gitworktree: nil context")
	}
	ps, err := systemTool("ps", "/bin/ps", "/usr/bin/ps")
	if err != nil {
		return nil, err
	}
	lsofPath, err := systemTool("lsof", "/usr/sbin/lsof", "/usr/bin/lsof", "/sbin/lsof")
	if err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, ps, "-A", "-ww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	byPID := map[int]*Program{}
	var list []Program
	for line := range strings.SplitSeq(string(out), "\n") {
		pidText, command, ok := strings.Cut(strings.TrimSpace(line), " ")
		if pid, perr := strconv.Atoi(pidText); ok && perr == nil {
			list = append(list, Program{PID: pid, Command: strings.TrimSpace(command)})
		}
	}
	for i := range list {
		byPID[list[i].PID] = &list[i]
	}
	var stdout bytes.Buffer
	lsof := exec.CommandContext(ctx, lsofPath, "-n", "-P", "-w", "-d", "cwd", "-F", "pn")
	lsof.Stdout = &stdout
	if err := lsof.Run(); err != nil && stdout.Len() == 0 {
		return nil, fmt.Errorf("lsof: %w", err)
	}
	pid := 0
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n") && pid != 0:
			if p := byPID[pid]; p != nil {
				p.Dir = line[1:]
			}
		}
	}
	return list, nil
}

// systemTool returns the first of paths that exists. The busy check runs ps and lsof from the system's own folders,
// never through PATH, because a program named ps earlier in PATH could hide a busy worktree from Remove.
func systemTool(name string, paths ...string) (string, error) {
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not in %s; pass a ProgramLister instead", name, strings.Join(paths, ", "))
}

// programs is every running program, read once per method.
type programs []Program

// using returns the programs, other than this one, whose command line names dir or a path inside it, or whose
// working folder is inside it.
func (ps programs) using(dir string) []Program {
	dirs := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		dirs = append(dirs, real)
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
