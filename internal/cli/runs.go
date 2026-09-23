package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/marcioapm/lux/internal/client"
	"github.com/marcioapm/lux/internal/server"
	"github.com/marcioapm/lux/internal/spec"
)

type Run = server.Run

func (a *app) runCmd() *cobra.Command {
	var file, idem, secretsFrom string
	var follow, wait bool
	var labels []string
	cmd := &cobra.Command{
		Use:   "run -f spec.yaml [-- command...]",
		Short: "Submit a Run",
		Long: `Submit a Run from a spec file (YAML or JSON), or a quick generic Run:

  lux run -f spec.yaml --follow
  lux run --image alpine -- echo hello

Secret values in the spec can be read from the environment with
value: ${NAME}, or from a .env file with --secrets-from.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var sp spec.RunSpec
			if file != "" {
				b, err := readFileOrStdin(file)
				if err != nil {
					return err
				}
				if err := yaml.Unmarshal(b, &sp); err != nil {
					return fmt.Errorf("%s: %w", file, err)
				}
			}
			img, _ := cmd.Flags().GetString("image")
			if img != "" {
				sp.Image = spec.Image{Ref: img}
			}
			if len(args) > 0 {
				sp.Workload.Command = args
				if sp.Workload.Adapter == "" {
					sp.Workload.Adapter = "generic"
				}
			}
			if name, _ := cmd.Flags().GetString("name"); name != "" {
				sp.Name = name
			}
			for _, l := range labels {
				k, v, _ := strings.Cut(l, "=")
				if sp.Labels == nil {
					sp.Labels = map[string]string{}
				}
				sp.Labels[k] = v
			}
			if err := fillSecrets(sp.Secrets, secretsFrom); err != nil {
				return err
			}
			var run Run
			var hdr []string
			if idem != "" {
				hdr = []string{"Idempotency-Key", idem}
			}
			if err := a.c.Do(ctxOf(cmd), "POST", "/v1/runs", sp, &run, hdr...); err != nil {
				return err
			}
			if !follow && !wait {
				if a.output == "json" {
					return a.json(run)
				}
				fmt.Fprintln(a.stdout, run.ID)
				return nil
			}
			fmt.Fprintln(a.stderr, run.ID)
			if follow {
				if _, err := a.followLogs(ctxOf(cmd), run.ID, "", logOpts{stderr: true}); err != nil {
					return err
				}
			}
			return a.waitExit(ctxOf(cmd), run.ID)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "spec file (YAML or JSON; - for stdin)")
	cmd.Flags().String("image", "", "image ref, for a quick generic Run")
	cmd.Flags().String("name", "", "a name for humans")
	cmd.Flags().StringArrayVarP(&labels, "label", "l", nil, "label key=value (repeatable)")
	cmd.Flags().StringVar(&idem, "idempotency-key", "", "make the submission safe to retry")
	cmd.Flags().StringVar(&secretsFrom, "secrets-from", "", ".env file supplying secret values")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream output until the Run ends; exit with its code")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the Run ends; exit with its code")
	return cmd
}

func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		var b strings.Builder
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			b.WriteString(sc.Text() + "\n")
		}
		return []byte(b.String()), sc.Err()
	}
	return os.ReadFile(path)
}

// fillSecrets resolves secret values: ${NAME} from the environment, or
// from a .env file for secrets without a value.
func fillSecrets(secrets []spec.Secret, envFile string) error {
	fileVals := map[string]string{}
	if envFile != "" {
		b, err := os.ReadFile(envFile)
		if err != nil {
			return err
		}
		for _, l := range strings.Split(string(b), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			k, v, ok := strings.Cut(strings.TrimPrefix(l, "export "), "=")
			if ok {
				fileVals[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	for i := range secrets {
		s := &secrets[i]
		if strings.HasPrefix(s.Value, "${") && strings.HasSuffix(s.Value, "}") {
			name := s.Value[2 : len(s.Value)-1]
			v, ok := os.LookupEnv(name)
			if !ok {
				return fmt.Errorf("secret %s: environment variable %s is not set", s.Name, name)
			}
			s.Value = v
		}
		if s.Value == "" {
			if v, ok := fileVals[s.Name]; ok {
				s.Value = v
			} else if v, ok := os.LookupEnv(s.Name); ok {
				s.Value = v
			}
		}
	}
	return nil
}

func (a *app) lsCmd() *cobra.Command {
	var state string
	var labels []string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List Runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if state != "" {
				q.Set("state", state)
			}
			for _, l := range labels {
				q.Add("label", l)
			}
			var resp struct {
				Runs []Run `json:"runs"`
			}
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/runs?"+q.Encode(), nil, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp.Runs)
			}
			var rows [][]string
			for _, r := range resp.Runs {
				state := r.State
				if r.Activity == "idle" && r.State == "running" {
					state += " (waiting for input)"
				}
				rows = append(rows, []string{r.ID, orDash(r.Name), state, orDash(r.Host), r.Spec.Workload.Adapter, ago(&r.CreatedAt)})
			}
			a.table("ID\tNAME\tSTATE\tHOST\tADAPTER\tCREATED", rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "filter by state (comma-separated)")
	cmd.Flags().StringArrayVarP(&labels, "label", "l", nil, "filter by label key=value")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *app) getRun(ctx context.Context, id string) (*Run, error) {
	var run Run
	if err := a.c.Do(ctx, "GET", "/v1/runs/"+id, nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (a *app) getCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <run>",
		Short: "Show a Run, its placements and resource usage",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := a.getRun(ctxOf(cmd), args[0])
			if err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(run)
			}
			w := a.stdout
			fmt.Fprintf(w, "id:        %s\n", run.ID)
			if run.Name != "" {
				fmt.Fprintf(w, "name:      %s\n", run.Name)
			}
			fmt.Fprintf(w, "state:     %s", run.State)
			if run.StateReason != "" {
				fmt.Fprintf(w, " (%s)", run.StateReason)
			}
			if run.Activity != "" {
				fmt.Fprintf(w, " [%s]", run.Activity)
			}
			fmt.Fprintln(w)
			if run.ExitCode != nil {
				fmt.Fprintf(w, "exit code: %d\n", *run.ExitCode)
			}
			fmt.Fprintf(w, "adapter:   %s\n", run.Spec.Workload.Adapter)
			if run.SessionID != "" {
				fmt.Fprintf(w, "session:   %s\n", run.SessionID)
			}
			fmt.Fprintf(w, "created:   %s\n", run.CreatedAt.Format(time.RFC3339))
			if len(run.Placements) > 0 {
				fmt.Fprintln(w, "placements:")
				for _, p := range run.Placements {
					code := "-"
					if p.ExitCode != nil {
						code = fmt.Sprint(*p.ExitCode)
					}
					fmt.Fprintf(w, "  %d  %-12s %-8s exit=%s %s\n", p.Epoch, p.HostName, p.State, code, p.ExitReason)
				}
			}
			if u := run.Usage; u != nil && u.Placements > 0 {
				fmt.Fprintf(w, "usage:     peak memory %s, peak disk %s, cpu %.1fs\n",
					bytesHuman(u.PeakMemoryBytes), bytesHuman(u.PeakDiskBytes), u.CPUSeconds)
			}
			return nil
		},
	}
}

type logOpts struct {
	stderr, events bool
}

func (a *app) logsCmd() *cobra.Command {
	var follow bool
	var since string
	var o logOpts
	cmd := &cobra.Command{
		Use:   "logs <run>",
		Short: "Print a Run's output",
		Long:  "Print a Run's output, across every placement, from wherever it lives.\nWith -o json, prints one JSON record per line (cursor, channel, data).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if follow {
				_, err := a.followLogs(ctxOf(cmd), args[0], since, o)
				return err
			}
			_, err := a.printLogs(ctxOf(cmd), args[0], since, false, o)
			return err
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming until the Run stops or ends")
	cmd.Flags().StringVar(&since, "since", "", "resume from a cursor")
	cmd.Flags().BoolVar(&o.stderr, "stderr", true, "include stderr")
	cmd.Flags().BoolVar(&o.events, "events", false, "include structured events and lifecycle")
	return cmd
}

// followLogs streams until the Run stops or ends, reconnecting if the
// stream drops. Returns the last cursor.
func (a *app) followLogs(ctx context.Context, id, since string, o logOpts) (string, error) {
	cur := since
	for {
		c, err := a.printLogs(ctx, id, cur, true, o)
		if c != "" {
			cur = c
		}
		if err == nil || ctx.Err() != nil {
			return cur, err
		}
		var ae *client.APIError
		if errors.As(err, &ae) {
			return cur, err
		}
		time.Sleep(time.Second)
	}
}

func (a *app) printLogs(ctx context.Context, id, since string, follow bool, o logOpts) (string, error) {
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if follow {
		q.Set("follow", "true")
	}
	if o.events {
		q.Set("events", "true")
	}
	cur := since
	ended := false
	err := a.c.Stream(ctx, "/v1/runs/"+id+"/output", q, func(ev client.SSEEvent) error {
		switch ev.Event {
		case "record":
			var r server.OutputRecord
			if err := json.Unmarshal(ev.Data, &r); err != nil {
				return nil
			}
			cur = r.Cursor
			if a.output == "json" {
				if r.Ch == "event" && !o.events {
					return nil
				}
				return json.NewEncoder(a.stdout).Encode(r)
			}
			switch r.Ch {
			case "stdout":
				fmt.Fprint(a.stdout, r.Data)
			case "stderr":
				if o.stderr {
					fmt.Fprint(a.stderr, r.Data)
				}
			case "event":
				if o.events {
					fmt.Fprintf(a.stderr, "· %s\n", r.Event)
				}
			}
		case "lux":
			if a.output == "json" {
				fmt.Fprintf(a.stdout, "{\"lux\":%s}\n", ev.Data)
			} else {
				var e server.Event
				_ = json.Unmarshal(ev.Data, &e)
				fmt.Fprintf(a.stderr, "── %s %v\n", e.Type, e.Data)
			}
		case "gap":
			var g map[string]any
			_ = json.Unmarshal(ev.Data, &g)
			fmt.Fprintf(a.stderr, "── output missing for placement %v: %v\n", g["epoch"], g["reason"])
		case "end":
			var e struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(ev.Data, &e)
			if e.Cursor != "" {
				cur = e.Cursor
			}
			ended = true
		case "error":
			return fmt.Errorf("%s", ev.Data)
		}
		return nil
	})
	if err == nil && !ended {
		err = errors.New("output stream ended early")
	}
	return cur, err
}

func (a *app) eventsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "events <run>",
		Short: "Print a Run's lifecycle events",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Events []server.Event `json:"events"`
			}
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/runs/"+args[0]+"/events", nil, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp.Events)
			}
			for _, e := range resp.Events {
				d, _ := json.Marshal(e.Data)
				ep := "-"
				if e.Epoch != nil {
					ep = fmt.Sprint(*e.Epoch)
				}
				fmt.Fprintf(a.stdout, "%s  %-3s %-20s %s\n", e.Time.Format("15:04:05.000"), ep, e.Type, d)
			}
			return nil
		},
	}
}

func (a *app) steerCmd() *cobra.Command {
	var interrupt bool
	var reqID string
	cmd := &cobra.Command{
		Use:   "steer <run> <message>",
		Short: "Send a message to a running workload",
		Long:  "Send input to a running Run. Agents get it as a message (queued until the\ncurrent turn ends if the agent cannot take it mid-turn); generic workloads\nget it on stdin. --interrupt stops the current turn first.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp map[string]string
			in := map[string]any{"text": args[1], "interrupt": interrupt}
			if reqID != "" {
				in["requestId"] = reqID
			}
			if err := a.c.Do(ctxOf(cmd), "POST", "/v1/runs/"+args[0]+"/input", in, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp)
			}
			fmt.Fprintln(a.stdout, resp["requestId"])
			return nil
		},
	}
	cmd.Flags().BoolVar(&interrupt, "interrupt", false, "stop the current turn first")
	cmd.Flags().StringVar(&reqID, "request-id", "", "id that makes a retry safe")
	return cmd
}

func (a *app) interruptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "interrupt <run>",
		Short: "Interrupt the workload's current turn",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.c.Do(ctxOf(cmd), "POST", "/v1/runs/"+args[0]+"/input", map[string]any{"interrupt": true}, nil)
		},
	}
}

func (a *app) lifecycle(use, short, path string, wantStates ...string) *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:   use + " <run>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var run Run
			if err := a.c.Do(ctxOf(cmd), "POST", "/v1/runs/"+args[0]+"/"+path, nil, &run); err != nil {
				return err
			}
			if wait {
				r, err := a.waitState(ctxOf(cmd), args[0], wantStates...)
				if err != nil {
					return err
				}
				run = *r
			}
			if a.output == "json" {
				return a.json(run)
			}
			fmt.Fprintf(a.stdout, "%s %s\n", run.ID, run.State)
			return nil
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until it has taken effect")
	return cmd
}

func (a *app) stopCmd() *cobra.Command {
	return a.lifecycle("stop", "Stop a Run gracefully (resumable)", "stop", "stopped", "succeeded", "failed", "cancelled", "lost")
}

func (a *app) cancelCmd() *cobra.Command {
	return a.lifecycle("cancel", "Stop a Run and make it final", "cancel", "cancelled", "succeeded", "failed")
}

func (a *app) resumeCmd() *cobra.Command {
	var input, secretsFrom, fromSnapshot string
	var secretArgs []string
	var follow, wait bool
	cmd := &cobra.Command{
		Use:   "resume <run>",
		Short: "Resume a stopped, lost or failed Run on any host",
		Long: `Resume a Run. Its secrets must be supplied again (lux never stores them):
from the environment (by name), a .env file (--secrets-from), or --secret NAME=VALUE.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctxOf(cmd)
			run, err := a.getRun(ctx, args[0])
			if err != nil {
				return err
			}
			var secrets []spec.Secret
			for _, ref := range run.Secrets {
				secrets = append(secrets, spec.Secret{Name: ref.Name})
			}
			for _, kv := range secretArgs {
				k, v, _ := strings.Cut(kv, "=")
				for i := range secrets {
					if secrets[i].Name == k {
						secrets[i].Value = v
					}
				}
			}
			if err := fillSecrets(secrets, secretsFrom); err != nil {
				return err
			}
			req := map[string]any{"secrets": secrets}
			if input != "" {
				req["input"] = map[string]string{"text": input}
			}
			if fromSnapshot != "" {
				req["fromSnapshot"] = fromSnapshot
			}
			var out Run
			if err := a.c.Do(ctx, "POST", "/v1/runs/"+args[0]+"/resume", req, &out); err != nil {
				return err
			}
			if follow {
				if _, err := a.followLogs(ctx, args[0], "", logOpts{stderr: true}); err != nil {
					return err
				}
				return a.waitExit(ctx, args[0])
			}
			if wait {
				r, err := a.waitState(ctx, args[0], "running", "succeeded", "failed", "cancelled", "stopped", "lost")
				if err != nil {
					return err
				}
				out = *r
			}
			if a.output == "json" {
				return a.json(out)
			}
			fmt.Fprintf(a.stdout, "%s %s\n", out.ID, out.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "message to deliver once it is running")
	cmd.Flags().StringVar(&secretsFrom, "secrets-from", "", ".env file supplying secret values")
	cmd.Flags().StringArrayVar(&secretArgs, "secret", nil, "NAME=VALUE (repeatable)")
	cmd.Flags().StringVar(&fromSnapshot, "from-snapshot", "", "resume from an older snapshot")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream output until it ends")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until it is running (or has ended)")
	return cmd
}

func (a *app) waitCmd() *cobra.Command {
	var states []string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait <run>",
		Short: "Wait for a Run to reach a state (default: until it ends); exit with its code",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctxOf(cmd)
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			if len(states) == 0 {
				return a.waitExit(ctx, args[0])
			}
			run, err := a.waitState(ctx, args[0], states...)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(run)
			}
			fmt.Fprintf(a.stdout, "%s %s\n", run.ID, run.State)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&states, "state", nil, "states to wait for (comma-separated)")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long")
	return cmd
}

func (a *app) waitState(ctx context.Context, id string, states ...string) (*Run, error) {
	want := map[string]bool{}
	for _, s := range states {
		want[s] = true
	}
	for {
		run, err := a.getRun(ctx, id)
		if err != nil {
			return nil, err
		}
		if want[run.State] {
			return run, nil
		}
		select {
		case <-ctx.Done():
			return run, fmt.Errorf("timed out waiting: run is %s", run.State)
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// waitExit waits for a Run to stop or end and returns its exit code as the
// command's.
func (a *app) waitExit(ctx context.Context, id string) error {
	run, err := a.waitState(ctx, id, "succeeded", "failed", "cancelled", "stopped", "lost")
	if err != nil {
		return err
	}
	if a.output == "json" {
		_ = a.json(run)
	} else if run.State != "succeeded" {
		msg := run.State
		if run.StateReason != "" {
			msg += ": " + run.StateReason
		}
		fmt.Fprintln(a.stderr, "lux:", id, msg)
	}
	switch {
	case run.State == "succeeded":
		return nil
	case run.ExitCode != nil && *run.ExitCode != 0:
		return exitCode(*run.ExitCode)
	}
	return exitCode(1)
}
