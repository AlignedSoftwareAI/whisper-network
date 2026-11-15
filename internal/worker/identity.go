package worker

import (
	"errors"
	"fmt"
	"os"

	"github.com/deweysasser/whisper-network/internal/sandbox"
)

// shareWithChildren makes the state directory traversable for a separate
// child uid: the state and work directories become 0711, so the child can
// reach its own lease directory and the (world-readable) model files by name
// but list nothing, and credentials.json stays 0600. Without a child uid the
// directories stay 0700.
func (w *Worker) shareWithChildren() error {
	if w.cfg.ChildUID == 0 {
		return nil
	}
	for _, d := range []string{w.cfg.StateDir, w.workRoot()} {
		if err := os.Chmod(d, 0o711); err != nil {
			return err
		}
	}
	return nil
}

// createForChild creates a new file at path for a child to read or write:
// O_EXCL and O_NOFOLLOW, so the worker (possibly root) never writes through
// a link or into a file someone else placed in the child-owned lease
// directory; mode 0600; and, with a child uid, owned by that uid (fchown on
// the open file, not a second lookup of the path).
func (w *Worker) createForChild(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|createNoFollow, 0o600)
	if err != nil {
		return nil, err
	}
	if w.cfg.ChildUID != 0 {
		if err := f.Chown(w.cfg.ChildUID, w.cfg.ChildGID); err != nil {
			f.Close()
			return nil, fmt.Errorf("handing %s to the child uid: %w", path, err)
		}
	}
	return f, nil
}

// writeForChild writes data to a new file at path made by createForChild.
func (w *Worker) writeForChild(path string, data []byte) error {
	f, err := w.createForChild(path)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

// limitProcesses sets the RLIMIT_NPROC of a child spec (DESIGN.md §7.4).
// RLIMIT_NPROC counts every task of the child's uid, and the children of
// all slots share that uid, so the bound is one allowance of
// Limits.Processes per slot, shared by all running children rather than a
// per-child snapshot that another slot's child could already have used up.
// With a dedicated child uid (checked by checkDedicatedChildUID) it is the
// absolute limit; without one the children run as the worker's own uid,
// whose other tasks are unknown, so it is headroom over what that uid runs
// at launch.
func (w *Worker) limitProcesses(s *sandbox.Spec) {
	allowance := uint64(w.cfg.Limits.Processes) * uint64(max(w.facts.Slots, 1))
	if w.cfg.ChildUID != 0 {
		s.ProcessLimit = allowance
		return
	}
	s.Processes = allowance
}

// countUIDTasks is sandbox.CountTasks, replaceable in tests.
var countUIDTasks = sandbox.CountTasks

// checkDedicatedChildUID refuses a child uid that already runs tasks. The
// children's RLIMIT_NPROC is an absolute count over everything the uid runs,
// and a uid shared with another service would also share files the children
// can reach and processes they could signal or trace, so the uid must belong
// to whisper-network alone. Tasks in other PID namespaces (another container
// using the same uid) are invisible here but still count against the limit.
func (w *Worker) checkDedicatedChildUID() error {
	if w.cfg.ChildUID == 0 {
		return nil
	}
	n, err := countUIDTasks(w.cfg.ChildUID)
	if err != nil {
		return fmt.Errorf("worker: checking that child uid %d is dedicated: %w", w.cfg.ChildUID, err)
	}
	if n > 0 {
		return fmt.Errorf("worker: child uid %d already runs %d processes or threads; "+
			"--child-uid must name a user dedicated to whisper-network's children, with nothing else running as it",
			w.cfg.ChildUID, n)
	}
	return nil
}

// giveToChild hands a file or directory the worker created for a lease to
// the child uid, when one is configured.
func (w *Worker) giveToChild(path string) error {
	if w.cfg.ChildUID == 0 {
		return nil
	}
	if err := os.Lchown(path, w.cfg.ChildUID, w.cfg.ChildGID); err != nil {
		return fmt.Errorf("handing %s to the child uid: %w", path, err)
	}
	return nil
}
