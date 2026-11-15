package worker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChildEnvIsAnAllowList(t *testing.T) {
	env := childEnv([]string{"PATH=/usr/bin", "WHISPER_SERVER=x", "WHISPER_TOKEN=wnc_x", "GITHUB_TOKEN=y",
		"DB_PASSWORD=z", "aws_secret_access_key=k", "LANG=C", "TMPDIR=/tmp", "HOME=/home/u",
		"SSH_AUTH_SOCK=/run/user/1000/ssh", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
		"XDG_RUNTIME_DIR=/run/user/1000", "OPENAI_API_KEY=sk", "AWS_ACCESS_KEY_ID=AK", "AWS_PROFILE=p",
		"LC_ALL=C.UTF-8", "TZ=UTC", "GGML_NTHREADS=2", "OMP_NUM_THREADS=4", "CUDA_VISIBLE_DEVICES=0",
		"LD_PRELOAD=/tmp/evil.so", "NOEQUALS"}, "/work/l1")
	assert.Equal(t, []string{"PATH=/usr/bin", "LANG=C", "LC_ALL=C.UTF-8", "TZ=UTC", "GGML_NTHREADS=2",
		"OMP_NUM_THREADS=4", "CUDA_VISIBLE_DEVICES=0", "HOME=/work/l1", "TMPDIR=/work/l1"}, env)
	for _, kv := range env {
		for _, bad := range []string{"SSH_AUTH_SOCK", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "_API_KEY", "AWS_", "LD_PRELOAD"} {
			assert.NotContains(t, kv, bad)
		}
	}
}

func TestTailBufferKeepsTail(t *testing.T) {
	b := &tailBuffer{max: 10}
	_, _ = b.Write([]byte("0123456789"))
	_, _ = b.Write([]byte("abc"))
	assert.Equal(t, "3456789abc", b.String())
	_, _ = b.Write([]byte(strings.Repeat("z", 50)))
	assert.Equal(t, strings.Repeat("z", 10), b.String())
}

func TestFFmpegInputError(t *testing.T) {
	assert.True(t, ffmpegInputError("x.mp4: Invalid data found when processing input"))
	assert.True(t, ffmpegInputError("Stream map '0:a:0' matches no streams."))
	assert.False(t, ffmpegInputError("Killed"))
	assert.False(t, ffmpegInputError(""))
}

func TestVersionParsing(t *testing.T) {
	assert.Equal(t, "whisper.cpp 1.9.5", parseWhisperVersion("whisper.cpp version: 1.9.5\n"))
	assert.Equal(t, UnknownEngine, parseWhisperVersion("error: unknown argument: --version"))
	assert.Equal(t, "7.1.1", parseFFmpegVersion("ffmpeg version 7.1.1 Copyright (c) 2000-2025\n"))
	assert.Equal(t, "7.1", parseFFmpegVersion("ffmpeg version n7.1 Copyright\n"))
	assert.Equal(t, "6.1.1-3ubuntu5", parseFFmpegVersion("ffmpeg version 6.1.1-3ubuntu5 Copyright\n"))
	assert.Equal(t, "unknown", parseFFmpegVersion(""))
}
