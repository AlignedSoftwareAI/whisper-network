package program

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zenizh/go-capturer"
)

func TestOptions_Run(t *testing.T) {
	var program Options

	exitValue := -1
	exitFunc = func(x int) {
		exitValue = x
	}
	defer func() { exitFunc = os.Exit }()

	out := capturer.CaptureStdout(func() {

		// Version output and exit happen during parsing. Because exitFunc is
		// faked and returns, parsing then continues and reports the missing
		// subcommand; that error is expected here.
		_, _ = program.Parse([]string{"--version"})
	})

	assert.Equal(t, exitValue, 0)
	assert.Equal(t, "unknown\n", out)
}
