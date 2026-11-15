//go:build integration

package e2e

// Real-engine integration test (DESIGN.md §12, "Integration"): a server
// container and a worker container on their own Docker network, the real
// whisper-cli 1.9.5 and ffmpeg 7.1 from the worker image, the tiny.en model
// and whisper.cpp's jfk.wav sample, converted to 44.1 kHz stereo AAC in an
// MP4 container so the job takes the prepare path.
//
// Layout: both containers run the image (default whisper-network:dev, set
// WN_IT_IMAGE to override) with a freshly built static whisper-network
// binary bind-mounted over the image's, so the test exercises this tree's
// code with the image's engines. The test itself talks to the server from
// the host through a published loopback port. Some Docker installs (e.g.
// snap) cannot see /tmp, so every bind mount lives under the repository's
// .cache/ directory (gitignored):
//
//	.cache/models/ggml-tiny.en.bin, jfk.wav   inputs (provided)
//	.cache/integration/<run>/                 binary, data dir, media; removed afterwards
//
// Run with `make integration` (which builds the image first if it is
// missing) or: go test -tags integration -run TestIntegration -v ./internal/e2e/

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/client"
	"github.com/deweysasser/whisper-network/internal/e2e/e2etest"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/obs/obstest"
)

// docker runs a docker command and returns its stdout; stderr goes into the
// error.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := dockerErr(args...)
	require.NoError(t, err, "docker %s", strings.Join(args, " "))
	return out
}

func dockerErr(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// stopwatch records named phase durations for the test log.
type stopwatch struct {
	t     *testing.T
	start time.Time
	last  time.Time
	lines []string
}

func (s *stopwatch) lap(name string) {
	now := time.Now()
	s.lines = append(s.lines, fmt.Sprintf("%-28s %6.1fs", name, now.Sub(s.last).Seconds()))
	s.last = now
}

func (s *stopwatch) report() {
	s.t.Logf("timings:\n  %s\n  %-28s %6.1fs", strings.Join(s.lines, "\n  "), "total",
		time.Since(s.start).Seconds())
}

func TestIntegrationRealEngines(t *testing.T) {
	image := os.Getenv("WN_IT_IMAGE")
	if image == "" {
		image = "whisper-network:dev"
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if _, err := dockerErr("image", "inspect", image); err != nil {
		t.Fatalf("image %s is missing; build it with `make image IMAGE=%s`", image, image)
	}
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	inputs := filepath.Join(root, ".cache", "models")
	for _, f := range []string{"ggml-tiny.en.bin", "jfk.wav"} {
		if _, err := os.Stat(filepath.Join(inputs, f)); err != nil {
			t.Fatalf("missing %s in %s: %v", f, inputs, err)
		}
	}

	sw := &stopwatch{t: t, start: time.Now(), last: time.Now()}
	defer sw.report()
	suffix := randHex(4)
	work := filepath.Join(root, ".cache", "integration", "run-"+suffix)
	data := filepath.Join(work, "data")
	require.NoError(t, os.MkdirAll(data, 0o755))
	t.Cleanup(func() { _ = os.RemoveAll(work) })

	// This tree's binary, static, over the image's.
	bin := filepath.Join(work, "whisper-network")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	out, err := build.CombinedOutput()
	require.NoError(t, err, "go build: %s", out)
	sw.lap("build binary")

	// jfk.wav (16 kHz mono) → 44.1 kHz stereo AAC in MP4: not canonical.
	docker(t, "run", "--rm", "--user", uid(), "--entrypoint", "ffmpeg",
		"-v", inputs+":/in:ro", "-v", work+":/out", image,
		"-nostdin", "-hide_banner", "-loglevel", "error", "-i", "/in/jfk.wav",
		"-ar", "44100", "-ac", "2", "-c:a", "aac", "-b:a", "128k", "/out/jfk.mp4")
	media, err := os.ReadFile(filepath.Join(work, "jfk.mp4"))
	require.NoError(t, err)
	sw.lap("make mp4 with ffmpeg")

	network := "wn-it-" + suffix
	serverName := "wn-it-server-" + suffix
	workerName := "wn-it-worker-" + suffix
	docker(t, "network", "create", network)
	t.Cleanup(func() {
		_, _ = dockerErr("rm", "-f", "-v", workerName, serverName)
		_, _ = dockerErr("network", "rm", network)
	})

	// Server: tiny.en allowlisted and already in its model cache (the
	// models directory is mounted read-only); no VAD model; fast reaper
	// defaults are fine for one job.
	mount := func(src, dst, mode string) string { return src + ":" + dst + ":" + mode }
	docker(t, "run", "-d", "--name", serverName, "--network", network, "--user", uid(),
		"-v", mount(bin, "/usr/local/bin/whisper-network", "ro"),
		"-v", mount(data, "/data", "rw"),
		"-v", mount(inputs, "/data/models", "ro"),
		"-p", "127.0.0.1::8080",
		"-e", "WHISPER_OUTPUT_FORMAT=jsonl",
		image, "server", "--data", "/data", "--listen", ":8080", "--metrics-listen", "",
		"--models", "tiny.en", "--vad-model=", "--heartbeat-interval", "5s")
	t.Cleanup(func() { dumpLogsOnFailure(t, serverName) })
	hostPort := strings.TrimSpace(docker(t, "port", serverName, "8080/tcp"))
	hostPort = strings.Split(hostPort, "\n")[0]
	base := "http://" + hostPort
	waitHTTP(t, base+"/readyz", 30*time.Second)
	sw.lap("start server")

	// An admin token, minted offline against the live data directory.
	created := docker(t, "exec", serverName, "whisper-network", "admin", "create-client", "--data", "/data",
		"--name", "integration", "--grant", "admin=*")
	m := regexp.MustCompile(`token (wnc_\S+)`).FindStringSubmatch(created)
	require.NotNil(t, m, created)
	admin, err := client.New(base, client.WithToken(m[1]), client.WithHTTPClient(&http.Client{}))
	require.NoError(t, err)

	// Worker: default seccomp profile, state in an anonymous volume.
	docker(t, "run", "-d", "--name", workerName, "--network", network,
		"-v", mount(bin, "/usr/local/bin/whisper-network", "ro"),
		"-e", "WHISPER_OUTPUT_FORMAT=jsonl",
		image, "worker", "--server", "http://"+serverName+":8080", "--description", "integration-worker",
		"--slots", "1", "--threads-per-slot", "4")
	t.Cleanup(func() { dumpLogsOnFailure(t, workerName) })
	var wk api.WorkerSummary
	require.Eventually(t, func() bool {
		var ws []api.WorkerSummary
		if err := adminGet(base, m[1], "/admin/workers", &ws); err != nil {
			return false
		}
		for _, w := range ws {
			if w.Facts.Description == "integration-worker" {
				wk = w
				return true
			}
		}
		return false
	}, 60*time.Second, 200*time.Millisecond, "worker never registered")
	t.Logf("worker facts: sandbox_level=%s engine=%q ffmpeg=%q cores=%d mem=%d MiB slots=%d kinds=%v models=%v",
		wk.Facts.SandboxLevel, wk.Facts.EngineVersion, wk.Facts.FFmpegVersion, wk.Facts.Cores,
		wk.Facts.MemBytes>>20, wk.Facts.Slots, wk.Facts.Kinds, wk.Facts.Models)
	for _, ev := range jsonl(t, containerStdout(t, workerName)) {
		if ev[obs.FieldEvent] == string(obs.EventWorkerSandbox) {
			t.Logf("worker.sandbox event: level=%v landlock_abi=%v blocked=%v open=%v", ev[obs.FieldSandboxLevel],
				ev[obs.FieldLandlockABI], ev["sandbox_blocked"], ev["sandbox_open"])
		}
	}
	require.Equal(t, api.SandboxFull, wk.Facts.SandboxLevel,
		"Landlock and the seccomp filter should hold under Docker's default seccomp profile on this kernel")
	require.NoError(t, adminPost(base, m[1], "/admin/workers/"+wk.ID+"/approve", api.ApproveWorkerRequest{Queues: []string{"public"}}))
	sw.lap("register + approve worker")

	ctx := context.Background()
	job, _, err := admin.Submit(ctx, "public", api.JobParams{Model: "tiny.en", Label: "jfk",
		Prompt: "Inaugural address of President Kennedy.", Source: "whisper.cpp samples/jfk.wav"},
		bytes.NewReader(media), client.SubmitOptions{Filename: "jfk.mp4"})
	require.NoError(t, err)
	assert.Equal(t, api.JobPreparing, job.State, "an MP4 upload takes the prepare path")
	assert.Equal(t, "video/mp4", job.Media.OriginalType)
	sw.lap("submit")

	var j *api.Job
	require.Eventually(t, func() bool {
		j, err = admin.Job(ctx, job.ID)
		if err != nil {
			return false
		}
		return j.State == api.JobDone || j.State == api.JobFailed
	}, 5*time.Minute, 250*time.Millisecond)
	require.Equal(t, api.JobDone, j.State, "job error: %v", j.Error)
	sw.lap("prepare + transcribe")
	t.Logf("job: duration %.2fs, tasks %d, wait %.2fs, elapsed %.2fs, compute %.2fs", j.Media.DurationS,
		j.Tasks.Total, ptr(j.WaitS), ptr(j.ElapsedS), j.ComputeS)
	assert.InDelta(t, 11.0, j.Media.DurationS, 0.2, "jfk.wav is 11 s")
	assert.True(t, j.Media.OriginalPurged)

	rc, err := admin.Transcript(ctx, job.ID)
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	rc.Close()
	require.NoError(t, err)
	txt := string(b)
	t.Logf("transcript:\n%s", txt)
	header, body, _ := strings.Cut(txt, "\n")
	assert.Regexp(t, `^# transcript-source: whisper; whisper\.cpp=1\.9\.5\S*; model=tiny\.en; vad=none; language=en; `+
		`transcribed=\S+; job=`+job.ID+`; source=whisper\.cpp samples/jfk\.wav; ffmpeg=7\.1\S*; tasks=1; workers=1$`, header)
	assert.Contains(t, normalize(body), "ask not what your country can do for you")

	// The real processes' logs obey the never-log rules too.
	for _, name := range []string{serverName, workerName} {
		logs := containerStdout(t, name)
		obstest.AssertClean(t, logs)
		assert.NotContains(t, string(logs), "Kennedy", "%s logged the prompt", name)
		assert.NotContains(t, strings.ToLower(string(logs)), "fellow americans", "%s logged transcript text", name)
	}
	sw.lap("checks")
}

var timestampRE = regexp.MustCompile(`(?m)^\[\d+:\d{2}\.\d{2}\] `)

// normalize joins transcript lines into lowercase words without punctuation.
func normalize(body string) string {
	s := strings.ToLower(timestampRE.ReplaceAllString(body, ""))
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r == '\'' {
			return r
		}
		return ' '
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func ptr(f *float64) float64 {
	if f == nil {
		return -1
	}
	return *f
}

func uid() string { return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func waitHTTP(t *testing.T, url string, d time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		resp, err := http.Get(url)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, d, 100*time.Millisecond, "%s never answered 200", url)
}

// containerStdout returns a container's stdout (the JSONL log; the
// bootstrap notice goes to stderr).
func containerStdout(t *testing.T, name string) []byte {
	t.Helper()
	cmd := exec.Command("docker", "logs", name)
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, io.Discard
	require.NoError(t, cmd.Run())
	return stdout.Bytes()
}

func jsonl(t *testing.T, b []byte) []map[string]any {
	var sb e2etest.SyncBuffer
	_, _ = sb.Write(b)
	return sb.Events(t, "")
}

func dumpLogsOnFailure(t *testing.T, name string) {
	if !t.Failed() {
		return
	}
	out, _ := exec.Command("docker", "logs", "--tail", "60", name).CombinedOutput()
	t.Logf("--- docker logs %s ---\n%s", name, out)
}

func adminGet(base, token, path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func adminPost(base, token, path string, in any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: %s: %s", path, resp.Status, msg)
	}
	return nil
}
