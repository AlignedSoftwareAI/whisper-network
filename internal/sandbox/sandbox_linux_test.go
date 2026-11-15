//go:build linux

package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

// systemRO are the read-only locations a dynamically linked test binary needs.
var systemRO = []string{"/usr", "/lib", "/lib64", "/etc/ld.so.cache"}

func requireLandlock(t *testing.T, minABI int) {
	t.Helper()
	if abi := landlockABI(); abi < minABI {
		t.Skipf("Landlock ABI %d available, test needs >= %d", abi, minABI)
	}
}

// runHelperUnder runs the helper command under sandbox-exec with spec and
// returns its trimmed stdout.
func runHelperUnder(t *testing.T, spec Spec, helperArgs ...string) string {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := Command(ctx, self, spec, append([]string{self}, helperArgs...)...)
	cmd.Env = append(os.Environ(), helperEnv+"=1", fastExit)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "stderr: %s", stderr.String())
	return strings.TrimSpace(string(out))
}

type fsFixture struct {
	allowedFile, forbiddenFile, rwDir string
	spec                              Spec
}

func newFSFixture(t *testing.T) fsFixture {
	t.Helper()
	root := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(p, 0o755))
		return p
	}
	f := fsFixture{
		allowedFile:   filepath.Join(mk("ro"), "in.txt"),
		forbiddenFile: filepath.Join(mk("other"), "secret.txt"),
		rwDir:         mk("rw"),
	}
	require.NoError(t, os.WriteFile(f.allowedFile, []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(f.forbiddenFile, []byte("s"), 0o644))
	f.spec = Spec{
		ReadOnly:  append([]string{filepath.Dir(f.allowedFile)}, systemRO...),
		ReadWrite: []string{f.rwDir},
	}
	return f
}

func TestFilesystemRestriction(t *testing.T) {
	requireLandlock(t, 1)
	f := newFSFixture(t)

	assert.Equal(t, "ok", runHelperUnder(t, f.spec, "read", f.allowedFile), "allowed read-only file")
	assert.Equal(t, "denied", runHelperUnder(t, f.spec, "read", f.forbiddenFile), "file outside allowed paths")
	assert.Equal(t, "denied", runHelperUnder(t, f.spec, "write", f.allowedFile), "write to read-only path")
	assert.Equal(t, "denied", runHelperUnder(t, f.spec, "write", f.forbiddenFile+".new"), "write outside rw paths")
	assert.Equal(t, "ok", runHelperUnder(t, f.spec, "write", filepath.Join(f.rwDir, "out")), "write inside rw path")
	assert.Equal(t, "ok", runHelperUnder(t, f.spec, "read", filepath.Join(f.rwDir, "out")), "rw path is readable")
}

func TestNoFilesystemRestrictionWhenNoPaths(t *testing.T) {
	f := newFSFixture(t)
	assert.Equal(t, "ok", runHelperUnder(t, Spec{}, "read", f.forbiddenFile))
}

func TestNetworkDenied(t *testing.T) {
	requireLandlock(t, 4)
	lis := listen(t)
	assert.Equal(t, "ok", runHelperUnder(t, Spec{}, "dial", lis), "control: connect works unrestricted")
	assert.Equal(t, "denied", runHelperUnder(t, Spec{DenyNet: true}, "dial", lis))
}

func TestNetworkNotDeniedWithoutFlag(t *testing.T) {
	requireLandlock(t, 4)
	f := newFSFixture(t)
	assert.Equal(t, "ok", runHelperUnder(t, f.spec, "dial", listen(t)), "fs-only spec leaves the network alone")
}

func TestFileSizeLimit(t *testing.T) {
	f := newFSFixture(t)
	out := filepath.Join(f.rwDir, "big")
	assert.Equal(t, "toobig", runHelperUnder(t, Spec{FileSize: 4096}, "fsize", out))
	fi, err := os.Stat(out)
	require.NoError(t, err)
	assert.EqualValues(t, 4096, fi.Size())
}

func TestOtherRlimits(t *testing.T) {
	out := runHelperUnder(t, Spec{OpenFiles: 321, AddressSpace: 64 << 30}, "limits")
	assert.Regexp(t, `Max open files\s+321\s+321`, out)
	assert.Regexp(t, `Max address space\s+68719476736\s+68719476736`, out)
}

func TestPriority(t *testing.T) {
	spec := Spec{}.WithLowestPriority()
	assert.Equal(t, "nice=19 policy=5 ioclass=3", runHelperUnder(t, spec, "prio"),
		"nice 19, SCHED_IDLE (5), idle I/O class (3)")
}

func TestPriorityUnchangedByDefault(t *testing.T) {
	out := runHelperUnder(t, Spec{}, "prio")
	assert.NotContains(t, out, "policy=5")
}

func TestRestrictionDoesNotLeakToParent(t *testing.T) {
	requireLandlock(t, 1)
	f := newFSFixture(t)
	runHelperUnder(t, f.spec, "read", f.forbiddenFile)
	_, err := os.ReadFile(f.forbiddenFile)
	assert.NoError(t, err, "the test process itself must stay unrestricted")
}

func TestMissingReadOnlyPathIgnoredButMissingRWFails(t *testing.T) {
	requireLandlock(t, 1)
	f := newFSFixture(t)
	spec := f.spec
	spec.ReadOnly = append(spec.ReadOnly, "/nonexistent-ro")
	assert.Equal(t, "ok", runHelperUnder(t, spec, "read", f.allowedFile))

	spec.ReadWrite = append(spec.ReadWrite, "/nonexistent-rw")
	self, _ := os.Executable()
	cmd := Command(context.Background(), self, spec, self, "read", f.allowedFile)
	cmd.Env = append(os.Environ(), helperEnv+"=1", fastExit)
	err := cmd.Run()
	var ee *exec.ExitError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, 126, ee.ExitCode())
}

func TestProbe(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	res, err := Probe(context.Background(), self)
	require.NoError(t, err)
	t.Logf("probe: level=%s abi=%d blocked=%v open=%v checks=%v", res.Level, res.LandlockABI,
		res.Blocked(), res.Open(), res.Checks)

	assert.Equal(t, landlockABI(), res.LandlockABI)
	assert.Len(t, res.Checks, len(Checks))
	// With Landlock for the filesystem and seccomp for sockets and (below
	// ABI 6) signals, every check holds wherever Landlock exists.
	if landlockABI() >= 1 {
		assert.Equal(t, api.SandboxFull, res.Level, "open: %v", res.Open())
		assert.Empty(t, res.Open())
	} else {
		assert.Equal(t, api.SandboxNone, res.Level)
	}
}

func TestProbeFailsOnBadBinary(t *testing.T) {
	_, err := Probe(context.Background(), "/bin/false")
	assert.Error(t, err)
}

func TestInterpret(t *testing.T) {
	all := func(o string, except ...string) string {
		m := map[string]string{}
		for _, c := range Checks {
			m[c] = o
		}
		for i := 0; i+1 < len(except); i += 2 {
			m[except[i]] = except[i+1]
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	cases := map[string]struct {
		json  string
		level api.SandboxLevel
		err   bool
	}{
		"full": {`{"control_ok":true,"landlock_abi":8,"checks":` + all("blocked") + `}`, api.SandboxFull, false},
		"mptcp unsupported is fine": {`{"control_ok":true,"checks":` + all("blocked", CheckMPTCPConnect, "unsupported") + `}`,
			api.SandboxFull, false},
		"udp open":      {`{"control_ok":true,"checks":` + all("blocked", CheckUDPSend, "open") + `}`, api.SandboxFSOnly, false},
		"signal open":   {`{"control_ok":true,"checks":` + all("blocked", CheckSignalParent, "open") + `}`, api.SandboxFSOnly, false},
		"unix error":    {`{"control_ok":true,"checks":` + all("blocked", CheckUnixConnect, "error: ECONNREFUSED") + `}`, api.SandboxFSOnly, false},
		"fs open":       {`{"control_ok":true,"checks":` + all("blocked", CheckFSRead, "open") + `}`, api.SandboxNone, false},
		"nothing":       {`{"control_ok":true,"checks":` + all("open") + `}`, api.SandboxNone, false},
		"no control":    {`{"checks":` + all("blocked") + `}`, "", true},
		"missing check": {`{"control_ok":true,"checks":{"fs_read":"blocked"}}`, "", true},
		"not json":      {`oops`, "", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := interpret([]byte(c.json))
			if c.err {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.level, res.Level)
		})
	}
}

// confinedSpec is the shape of a prepare task's spec: system libraries
// read-only, one read-write directory, no network.
func confinedSpec(t *testing.T) Spec {
	return Spec{ReadOnly: systemRO, ReadWrite: []string{t.TempDir(), "/dev/null"}, DenyNet: true}
}

// unixListener listens on a Unix socket (a leading "@" makes it abstract).
func unixListener(t *testing.T, name string) string {
	t.Helper()
	lis, err := net.Listen("unix", name)
	require.NoError(t, err)
	t.Cleanup(func() { lis.Close() })
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return name
}

// unixDgramListener binds a pathname Unix datagram socket, like /dev/log or
// the journald socket.
func unixDgramListener(t *testing.T, name string) string {
	t.Helper()
	c, err := net.ListenPacket("unixgram", name)
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	return name
}

func udpListener(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	return c.LocalAddr().String()
}

// Each escape that worked against the V4-only sandbox: the control run shows
// the vector is live on this machine, the confined run that it is closed.
func TestEscapeVectorsClosed(t *testing.T) {
	requireLandlock(t, 1)
	sock := unixListener(t, filepath.Join(t.TempDir(), "bus"))
	abstract := unixListener(t, fmt.Sprintf("@wn-test-%d", os.Getpid()))
	tcp, udp := listen(t), udpListener(t)
	dgram := unixDgramListener(t, filepath.Join(t.TempDir(), "log"))
	for _, c := range []struct {
		name string
		args []string
	}{
		{"tcp", []string{"dial", tcp}},
		{"mptcp", []string{"mptcp", tcp}},
		{"udp sendto", []string{"udp", udp}},
		{"pathname unix socket", []string{"unix", sock}},
		{"abstract unix socket", []string{"unix", abstract}},
		{"unix datagram from a socketpair", []string{"dgram", dgram}},
		{"signal outside the sandbox", []string{"kill-parent"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			control := runHelperUnder(t, Spec{}, c.args...)
			if control == "unsupported" {
				t.Skipf("kernel does not support %s", c.name)
			}
			require.Equal(t, "ok", control, "control: works unconfined")
			assert.Equal(t, "denied", runHelperUnder(t, confinedSpec(t), c.args...))
		})
	}
}

func TestDenyNetAloneClosesSockets(t *testing.T) {
	sock := unixListener(t, filepath.Join(t.TempDir(), "s"))
	assert.Equal(t, "denied", runHelperUnder(t, Spec{DenyNet: true}, "unix", sock))
	assert.Equal(t, "denied", runHelperUnder(t, Spec{DenyNet: true}, "udp", udpListener(t)))
}

func TestSocketpairDeniedWithNetwork(t *testing.T) {
	assert.Equal(t, "ok", runHelperUnder(t, Spec{}, "socketpair"), "control")
	assert.Equal(t, "denied", runHelperUnder(t, confinedSpec(t), "socketpair"))
	assert.Equal(t, "denied", runHelperUnder(t, Spec{DenyNet: true}, "socketpair"))
	fsOnly := Spec{ReadOnly: systemRO, ReadWrite: []string{t.TempDir()}}
	assert.Equal(t, "ok", runHelperUnder(t, fsOnly, "socketpair"), "only --deny-net removes socketpair")
}

// A confined child may not create or join namespaces, but ordinary fork,
// exec and threads keep working.
func TestConfinedChildCannotCreateNamespaces(t *testing.T) {
	spec := confinedSpec(t)
	newUser := strconv.Itoa(unix.CLONE_NEWUSER)
	t.Run("unshare user namespace", func(t *testing.T) {
		// Unfiltered, a multithreaded process gets EINVAL here; EPERM is
		// the filter's answer.
		assert.Equal(t, "error: invalid argument", runHelperUnder(t, Spec{}, "unshare", newUser), "control")
		assert.Equal(t, "denied", runHelperUnder(t, spec, "unshare", newUser))
	})
	t.Run("unshare other namespaces", func(t *testing.T) {
		for _, f := range []int{unix.CLONE_NEWNET, unix.CLONE_NEWNS, unix.CLONE_NEWPID, unix.CLONE_NEWIPC,
			unix.CLONE_NEWUTS, unix.CLONE_NEWCGROUP, unix.CLONE_NEWTIME} {
			assert.Equal(t, "denied", runHelperUnder(t, spec, "unshare", strconv.Itoa(f)), "flag %#x", f)
		}
	})
	t.Run("unshare without namespace flags", func(t *testing.T) {
		assert.Equal(t, "ok", runHelperUnder(t, spec, "unshare", strconv.Itoa(unix.CLONE_FS|unix.CLONE_FILES)))
	})
	t.Run("clone into a user namespace", func(t *testing.T) {
		if control := runHelperUnder(t, Spec{}, "clone-newuser"); control != "ok" {
			t.Logf("control: unprivileged user namespaces unavailable here (%s)", control)
		}
		assert.Equal(t, "denied", runHelperUnder(t, spec, "clone-newuser"))
	})
	t.Run("clone3 reports ENOSYS", func(t *testing.T) {
		assert.Equal(t, "error: invalid argument", runHelperUnder(t, Spec{}, "clone3"), "control")
		assert.Equal(t, "nosys", runHelperUnder(t, spec, "clone3"))
	})
	t.Run("setns", func(t *testing.T) {
		assert.Equal(t, "error: bad file descriptor", runHelperUnder(t, Spec{}, "setns"), "control")
		assert.Equal(t, "denied", runHelperUnder(t, spec, "setns"))
	})
	t.Run("fork and exec still work", func(t *testing.T) {
		assert.Equal(t, "ok", runHelperUnder(t, spec, "fork"))
	})
}

func TestConfinedChildCannotLeaveItsGroupOrUseIOUring(t *testing.T) {
	assert.Equal(t, "ok", runHelperUnder(t, Spec{}, "setpgid"), "control")
	assert.Equal(t, "denied", runHelperUnder(t, confinedSpec(t), "setpgid"))
	assert.Equal(t, "denied", runHelperUnder(t, confinedSpec(t), "io_uring"))
	assert.NotEqual(t, "denied", runHelperUnder(t, Spec{}, "io_uring"), "control")
}

func TestSeccompSignalFallback(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(self, "seccomp-signals")
	cmd.Env = append(os.Environ(), helperEnv+"=1", fastExit)
	out, err := cmd.Output()
	require.NoError(t, err, "%s", out)
	assert.Equal(t, "denied ok ok ok", strings.TrimSpace(string(out)), "parent, self, own group, own thread")
}

func TestSeccompFilterShape(t *testing.T) {
	for _, r := range []seccompRules{{}, {denyNet: true}, {denyNet: true, signalPid: 12345}} {
		prog, err := buildSeccomp(r)
		require.NoError(t, err)
		assert.Less(t, len(prog), 255, "jump offsets are 8 bits")
		last := prog[len(prog)-1]
		assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW), last.K, "default allow")
	}
}

func TestChildHasNoCapabilities(t *testing.T) {
	// No filesystem restriction, so the child can read /proc/self/status.
	out := runHelperUnder(t, Spec{DenyNet: true}, "caps")
	for _, set := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb"} {
		assert.Regexp(t, set+`:\s+0+\n?`, out)
	}
}

// TestRootChildLosesCapabilities runs sandbox-exec as root in a user
// namespace, where it holds every capability, and checks the exec'd child
// holds none and has an empty bounding set.
func TestRootChildLosesCapabilities(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	cmd := Command(context.Background(), self, Spec{DenyNet: true}, self, "caps")
	cmd.Env = append(os.Environ(), helperEnv+"=1", fastExit)
	cmd.SysProcAttr.Cloneflags = syscall.CLONE_NEWUSER
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("cannot run as root in a user namespace here: %v: %s", err, stderr.String())
	}
	for _, set := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb", "CapBnd"} {
		assert.Regexp(t, set+`:\s+0+\n?`, string(out))
	}
}

// gone reports whether pid has exited (or is a zombie awaiting its reaper).
func gone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	s := string(b)
	return strings.HasPrefix(strings.Fields(s[strings.LastIndexByte(s, ')')+1:])[0], "Z")
}

// startTree runs the helper "spawn-tree" under spec: it orphans a sleeper
// into its process group, prints the sleeper's pid, then hangs or exits.
func startTree(t *testing.T, ctx context.Context, spec Spec, mode string) (*exec.Cmd, int) {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	cmd := Command(ctx, self, spec, self, "spawn-tree", mode)
	cmd.Env = append(os.Environ(), helperEnv+"=1", fastExit)
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	require.NoError(t, err, "helper said %q", line)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return cmd, pid
}

func TestCancelKillsWholeProcessTree(t *testing.T) {
	for name, spec := range map[string]Spec{"unconfined": {}, "confined": confinedSpec(t)} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cmd, sleeper := startTree(t, ctx, spec, "hang")
			require.False(t, gone(sleeper), "sleeper runs before the cancel")
			cancel()
			_ = Wait(cmd)
			assert.Eventually(t, func() bool { return gone(sleeper) }, 5*time.Second, 10*time.Millisecond,
				"the orphaned grandchild survived the cancel")
		})
	}
}

func TestWaitKillsLeftoverDescendants(t *testing.T) {
	cmd, sleeper := startTree(t, context.Background(), confinedSpec(t), "exit")
	require.NoError(t, Wait(cmd))
	assert.Eventually(t, func() bool { return gone(sleeper) }, 5*time.Second, 10*time.Millisecond,
		"a descendant outlived the child")
}

func TestProcessLimit(t *testing.T) {
	// With the filesystem restricted too: the count must happen before
	// Landlock hides /proc.
	spec := Spec{Processes: 64, ReadOnly: append([]string{"/proc/self/limits"}, systemRO...)}
	out := runHelperUnder(t, spec, "limits")
	m := regexp.MustCompile(`Max processes\s+(\d+)\s+(\d+)`).FindStringSubmatch(out)
	require.NotNil(t, m, out)
	n, _ := strconv.Atoi(m[1])
	running := int(countTasks(os.Getuid()))
	assert.InDelta(t, running+64, n, 64, "RLIMIT_NPROC is the uid's task count plus the headroom")
	assert.Equal(t, m[1], m[2], "soft and hard limit")
}

func TestAbsoluteProcessLimit(t *testing.T) {
	// The absolute limit is used as is, without counting the uid's tasks,
	// and wins over the headroom form.
	out := runHelperUnder(t, Spec{ProcessLimit: 4321, Processes: 64}, "limits")
	assert.Regexp(t, `Max processes\s+4321\s+4321`, out)
}
