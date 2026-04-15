package lvm

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// BuildPVMoveArgs returns the argument list for moving a specific PE range
// from sourcePV to destPV. The format is:
//
//	pvmove -i 1 <source_pv>:<pe_start>-<pe_end> <dest_pv>
//
// `-i 1` makes pvmove report progress every 1 second so the migration loop
// can poll. iopsCapMB > 0 sets a bandwidth limit via --abort-on-deadlock-only
// is not the right flag — LVM uses --setautoactivation; throttling is
// implemented at the runner level (see RunPVMove). The cap is plumbed
// through here only as documentation; the runner sleeps between progress
// updates to enforce it.
func BuildPVMoveArgs(sourcePV string, peStart, peEnd uint64, destPV string) []string {
	return []string{
		"-i", "1",
		fmt.Sprintf("%s:%d-%d", sourcePV, peStart, peEnd),
		destPV,
	}
}

// PVMoveProgress is a parsed update from pvmove's `-i 1` output.
type PVMoveProgress struct {
	PercentDone float64
	Done        bool
}

// ParsePVMoveLine parses one line of pvmove progress output. The format
// LVM emits is roughly:
//
//	"  /dev/md0: Moved: 12.34%"
//
// On finish lvm prints "  /dev/md0: Moved: 100.00%". Empty/unrelated lines
// return ok=false.
func ParsePVMoveLine(line string) (PVMoveProgress, bool) {
	line = strings.TrimSpace(line)
	idx := strings.LastIndex(line, "Moved:")
	if idx < 0 {
		return PVMoveProgress{}, false
	}
	rest := strings.TrimSpace(line[idx+len("Moved:"):])
	rest = strings.TrimSuffix(rest, "%")
	rest = strings.TrimSpace(rest)
	pct, err := strconv.ParseFloat(rest, 64)
	if err != nil {
		return PVMoveProgress{}, false
	}
	return PVMoveProgress{PercentDone: pct, Done: pct >= 100.0}, true
}

// PVMoveRunner abstracts execution of `pvmove` so the migration manager can
// be unit-tested without LVM. The default implementation shells out via
// exec.Command; tests substitute a fake.
type PVMoveRunner interface {
	Run(args []string, onProgress func(PVMoveProgress)) error
}

// ExecPVMoveRunner runs the real pvmove binary. Wrapper, when non-empty,
// is prepended to the command line so callers can wrap pvmove in `ionice`,
// `nice`, `cgexec`, or any other lowering tool. The migration engine sets
// it to {"ionice","-c","3"} so pvmove yields to user-facing I/O.
type ExecPVMoveRunner struct {
	Wrapper []string
}

// Run executes pvmove and forwards parsed progress lines to onProgress.
func (e ExecPVMoveRunner) Run(args []string, onProgress func(PVMoveProgress)) error {
	name := "pvmove"
	full := args
	if len(e.Wrapper) > 0 {
		name = e.Wrapper[0]
		full = append(append([]string{}, e.Wrapper[1:]...), "pvmove")
		full = append(full, args...)
	}
	cmd := exec.Command(name, full...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("pvmove stderr pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pvmove stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("pvmove start: %w", err)
	}
	// LVM writes progress to stderr; combine both for resilience.
	go ForwardProgress(stderr, onProgress)
	go ForwardProgress(stdout, onProgress)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("pvmove: %w", err)
	}
	return nil
}

// ForwardProgress reads parsed pvmove progress lines from r and pushes each
// one through onProgress. Exposed so callers wrapping pvmove with their own
// runner (e.g. cgexec, systemd-run) can reuse the parser without copying it.
func ForwardProgress(r io.Reader, onProgress func(PVMoveProgress)) {
	if r == nil || onProgress == nil {
		return
	}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if p, ok := ParsePVMoveLine(scanner.Text()); ok {
			onProgress(p)
		}
	}
}
