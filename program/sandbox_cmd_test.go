package program

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/sandbox"
)

// The kong command passes its arguments through verbatim, so what the worker
// encodes must arrive intact and decode to the same spec.
func TestSandboxExecCmd_ArgsRoundTrip(t *testing.T) {
	spec := sandbox.Spec{
		ReadOnly: []string{"/usr", "/lib"}, ReadWrite: []string{"/tmp/task"},
		DenyNet: true, AddressSpace: 1 << 30, FileSize: 1 << 20, OpenFiles: 64,
		Nice: 19, SchedIdle: true, IOPrioIdle: true, UID: 1234, GID: 1234,
	}
	argv := []string{"sandbox-exec"}
	argv = append(argv, spec.Args()...)
	argv = append(argv, "--", "ffmpeg", "-i", "in.mp3", "--weird")

	var opts Options
	_, err := opts.Parse(argv)
	require.NoError(t, err)

	inv, err := sandbox.ParseArgs(opts.SandboxExec.Args)
	require.NoError(t, err)
	assert.Equal(t, spec, inv.Spec)
	assert.Equal(t, []string{"ffmpeg", "-i", "in.mp3", "--weird"}, inv.Argv)
}
