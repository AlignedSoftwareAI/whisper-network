package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/deweysasser/whisper-network/internal/api"
)

// ProbeResult is the measured strength of the sandbox on this machine.
type ProbeResult struct {
	// Level is what the worker reports to the server as its sandbox_level.
	Level api.SandboxLevel
	// LandlockABI is the kernel's Landlock ABI version, 0 when absent.
	LandlockABI int
	// FSBlocked says the forbidden file was unreadable. NetBlocked says
	// every network and IPC check was refused.
	FSBlocked, NetBlocked bool
	// Checks maps each check of [Checks] to its outcome.
	Checks map[string]string
}

// Blocked lists the checks that held, in [Checks] order. A check whose
// protocol the kernel lacks counts as held.
func (r ProbeResult) Blocked() []string { return r.filter(true) }

// Open lists the checks that did not hold, in [Checks] order.
func (r ProbeResult) Open() []string { return r.filter(false) }

func (r ProbeResult) filter(held bool) []string {
	var out []string
	for _, c := range Checks {
		if o, ok := r.Checks[c]; ok && isHeld(o) == held {
			out = append(out, c)
		}
	}
	return out
}

func isHeld(outcome string) bool { return outcome == OutcomeBlocked || outcome == OutcomeUnsupported }

// Probe measures what the sandbox really enforces by running a self-test child
// under the spec of a prepare task (read-only input directory, read-write
// work directory, no network). The child tries every escape in [Checks]: read
// a file outside the allowed paths, reach local TCP, MPTCP and UDP
// listeners, connect to pathname and abstract Unix sockets, send a datagram
// from a socketpair to a pathname Unix socket, and signal its parent. self is the path of a binary whose sandbox-exec subcommand calls
// [ExecArgs]. The level reflects observed behaviour, not the kernel version:
// "full" only when every check held.
func Probe(ctx context.Context, self string) (ProbeResult, error) {
	if runtime.GOOS != "linux" {
		return ProbeResult{Level: api.SandboxNone}, nil
	}
	dir, err := os.MkdirTemp("", "wn-probe-")
	if err != nil {
		return ProbeResult{}, err
	}
	defer os.RemoveAll(dir)

	allowedDir := filepath.Join(dir, "allowed")
	rwDir := filepath.Join(dir, "work")
	allowed := filepath.Join(allowedDir, "ok")
	forbidden := filepath.Join(dir, "secret")
	for _, d := range []string{allowedDir, rwDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return ProbeResult{}, err
		}
	}
	for _, f := range []string{allowed, forbidden} {
		if err := os.WriteFile(f, []byte("probe"), 0o600); err != nil {
			return ProbeResult{}, err
		}
	}

	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	abstract := "wn-probe-" + hex.EncodeToString(nonce[:])
	unixPath := filepath.Join(dir, "probe.sock")
	tcp, err := acceptAndClose("tcp", "127.0.0.1:0")
	if err != nil {
		return ProbeResult{}, err
	}
	defer tcp.Close()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return ProbeResult{}, fmt.Errorf("probe listener: %w", err)
	}
	defer udp.Close()
	ux, err := acceptAndClose("unix", unixPath)
	if err != nil {
		return ProbeResult{}, err
	}
	defer ux.Close()
	abs, err := acceptAndClose("unix", "@"+abstract)
	if err != nil {
		return ProbeResult{}, err
	}
	defer abs.Close()
	dgramPath := filepath.Join(dir, "probe.dgram")
	dgram, err := net.ListenPacket("unixgram", dgramPath)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("probe listener: %w", err)
	}
	defer dgram.Close()

	spec := Spec{ReadOnly: []string{allowedDir}, ReadWrite: []string{rwDir}, DenyNet: true}
	args := append([]string{SubcommandName}, spec.Args()...)
	args = append(args, "--"+flagSelfTest, "--", forbidden, allowed,
		tcp.Addr().String(), udp.LocalAddr().String(), unixPath, abstract, dgramPath)
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Env = []string{} // the self-test needs nothing from the worker's environment
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ProbeResult{}, fmt.Errorf("sandbox self-test failed: %w: %s", err, ee.Stderr)
		}
		return ProbeResult{}, fmt.Errorf("sandbox self-test: %w", err)
	}
	return interpret(out)
}

// acceptAndClose listens on addr and closes every accepted connection.
func acceptAndClose(network, addr string) (net.Listener, error) {
	lis, err := net.Listen(network, addr)
	if err != nil {
		return nil, fmt.Errorf("probe listener: %w", err)
	}
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return lis, nil
}

// interpret turns the child's JSON report into a ProbeResult.
func interpret(out []byte) (ProbeResult, error) {
	var rep selfTestReport
	if err := json.Unmarshal(out, &rep); err != nil {
		return ProbeResult{}, fmt.Errorf("sandbox self-test output %q: %w", out, err)
	}
	if !rep.ControlOK {
		return ProbeResult{}, errors.New("sandbox self-test inconclusive: allowed file was unreadable")
	}
	var missing []string
	for _, c := range Checks {
		if _, ok := rep.Checks[c]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return ProbeResult{}, fmt.Errorf("sandbox self-test incomplete: no result for %s", strings.Join(missing, ", "))
	}
	res := ProbeResult{LandlockABI: rep.ABI, Checks: rep.Checks}
	res.FSBlocked = isHeld(rep.Checks[CheckFSRead])
	res.NetBlocked = !slices.ContainsFunc(Checks[1:], func(c string) bool { return !isHeld(rep.Checks[c]) })
	switch {
	case res.FSBlocked && res.NetBlocked:
		res.Level = api.SandboxFull
	case res.FSBlocked:
		res.Level = api.SandboxFSOnly
	default:
		res.Level = api.SandboxNone
	}
	return res, nil
}
