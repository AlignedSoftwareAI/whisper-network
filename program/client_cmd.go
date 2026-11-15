package program

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/client"
)

// ClientFlags are shared by the client subcommands.
type ClientFlags struct {
	Server string `required:"" env:"WHISPER_SERVER" help:"Server URL, e.g. https://wn.example"`
	Token  string `required:"" env:"WHISPER_TOKEN" help:"Client API token (wnc_…)"`
}

func (f ClientFlags) client() (*client.Client, error) {
	return client.New(f.Server, client.WithToken(f.Token), client.WithHTTPClient(&http.Client{}),
		client.WithUserAgent("whisper-network-cli/"+Version))
}

func stdout(w io.Writer) io.Writer {
	if w == nil {
		return os.Stdout
	}
	return w
}

// SubmitCmd uploads a media file as a new job.
type SubmitCmd struct {
	ClientFlags    `embed:""`
	Queue          string `default:"public" help:"Queue to submit to"`
	Model          string `default:"small.en" help:"Model, e.g. small.en or large-v3-turbo"`
	Language       string `help:"Language code, or auto (server default: en)"`
	Weight         string `enum:",low,medium,high" default:"" help:"Weight class within the queue: low, medium or high"`
	Prompt         string `help:"Initial prompt (names, vocabulary); at most 800 characters"`
	Label          string `help:"Free-form label"`
	Source         string `help:"Where the media came from (recorded, never fetched)"`
	Metadata       string `help:"Opaque JSON object stored with the job"`
	IdempotencyKey string `help:"Retry-safe key: resubmitting with it returns the original job"`
	JSON           bool   `name:"json" help:"Print the job document instead of its ID"`
	File           string `arg:"" type:"existingfile" help:"Media file (any audio or video)"`

	out io.Writer
}

// Run submits the file.
func (c *SubmitCmd) Run(ctx context.Context) error {
	params := api.JobParams{Model: c.Model, Language: c.Language, Weight: api.Weight(c.Weight),
		Prompt: c.Prompt, Label: c.Label, Source: c.Source}
	if len([]rune(c.Prompt)) > api.MaxPromptLen {
		return fmt.Errorf("--prompt is %d characters; the limit is %d", len([]rune(c.Prompt)), api.MaxPromptLen)
	}
	if c.Metadata != "" {
		if !json.Valid([]byte(c.Metadata)) || !strings.HasPrefix(strings.TrimSpace(c.Metadata), "{") {
			return errors.New("--metadata must be a JSON object")
		}
		params.Metadata = json.RawMessage(c.Metadata)
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	f, err := os.Open(c.File)
	if err != nil {
		return err
	}
	defer f.Close()
	job, _, err := cl.Submit(ctx, c.Queue, params, f, client.SubmitOptions{
		IdempotencyKey: c.IdempotencyKey, Filename: filepath.Base(c.File)})
	if err != nil {
		return err
	}
	out := stdout(c.out)
	if c.JSON {
		return printJSON(out, job)
	}
	_, err = fmt.Fprintln(out, job.ID)
	return err
}

// StatusCmd shows one job, or the visible jobs of a queue.
type StatusCmd struct {
	ClientFlags `embed:""`
	Queue       string `default:"public" help:"Queue to list when no job ID is given"`
	State       string `enum:",preparing,queued,running,done,failed" default:"" help:"Only list jobs in this state"`
	Limit       int    `default:"50" help:"Maximum jobs to list"`
	JobID       string `arg:"" optional:"" help:"Job ID: print its status document"`

	out io.Writer
}

// Run prints the status.
func (c *StatusCmd) Run(ctx context.Context) error {
	cl, err := c.client()
	if err != nil {
		return err
	}
	out := stdout(c.out)
	if c.JobID != "" {
		job, err := cl.Job(ctx, c.JobID)
		if err != nil {
			return err
		}
		return printJSON(out, job)
	}
	var jobs []api.Job
	opt := client.ListOptions{State: api.JobState(c.State), Limit: c.Limit}
	for len(jobs) < c.Limit || c.Limit <= 0 {
		page, err := cl.ListJobs(ctx, c.Queue, opt)
		if err != nil {
			return err
		}
		jobs = append(jobs, page.Jobs...)
		if page.NextCursor == "" || len(page.Jobs) == 0 {
			break
		}
		opt.Cursor = page.NextCursor
	}
	if c.Limit > 0 && len(jobs) > c.Limit {
		jobs = jobs[:c.Limit]
	}
	return printJobTable(out, jobs)
}

func printJobTable(out io.Writer, jobs []api.Job) error {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tWEIGHT\tMODEL\tTASKS\tAUDIO\tCREATED\tLABEL")
	for _, j := range jobs {
		audio := "-"
		if j.Media.DurationS > 0 {
			audio = (time.Duration(j.Media.DurationS) * time.Second).String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s\t%s\t%s\n", j.ID, j.State, j.Weight, j.Model,
			j.Tasks.Done, j.Tasks.Total, audio, j.CreatedAt.Local().Format("2006-01-02 15:04"), j.Label)
	}
	return tw.Flush()
}

// FetchCmd downloads a finished job's transcript.
type FetchCmd struct {
	ClientFlags `embed:""`
	Output      string `short:"o" type:"path" help:"Write to this file instead of stdout"`
	JSON        bool   `name:"json" help:"Fetch result.json (segments with absolute times) instead of the text transcript"`
	JobID       string `arg:"" help:"Job ID"`

	out io.Writer
}

// Run downloads the transcript.
func (c *FetchCmd) Run(ctx context.Context) error {
	cl, err := c.client()
	if err != nil {
		return err
	}
	var body io.Reader
	if c.JSON {
		res, err := cl.Result(ctx, c.JobID)
		if err != nil {
			return notDoneHint(err, c.JobID)
		}
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		body = strings.NewReader(string(b) + "\n")
	} else {
		rc, err := cl.Transcript(ctx, c.JobID)
		if err != nil {
			return notDoneHint(err, c.JobID)
		}
		defer rc.Close()
		body = rc
	}
	if c.Output == "" {
		_, err := io.Copy(stdout(c.out), body)
		return err
	}
	return writeFileFrom(c.Output, body)
}

func notDoneHint(err error, id string) error {
	if client.IsNotDone(err) {
		return fmt.Errorf("job %s is not done yet; check it with `status %s`: %w", id, id, err)
	}
	return err
}

// writeFileFrom writes r to path via a temporary file, so a failed download
// never leaves a truncated transcript under the final name. The file keeps
// os.CreateTemp's mode 0600: a transcript can carry meeting content, so it
// is not made readable to other local users.
func writeFileFrom(path string, r io.Reader) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".part-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func printJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
