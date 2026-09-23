package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marcioapm/lux/internal/server"
)

func (a *app) pushCmd() *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:   "push <run>",
		Short: "Push the Run's repositories to its git.push branch",
		Long: `Push each repository's current commit to the spec's git.push branch,
with credentials only the runner holds. A branch that moved since this Run
last pushed it is not overwritten. --wait prints each repository's outcome
and fails unless every one was pushed or already up to date.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp map[string]string
			if err := a.c.Do(ctxOf(cmd), "POST", "/v1/runs/"+args[0]+"/push", map[string]any{}, &resp); err != nil {
				return err
			}
			if !wait {
				if a.output == "json" {
					return a.json(resp)
				}
				fmt.Fprintln(a.stdout, resp["requestId"])
				return nil
			}
			return a.waitPush(cmd, args[0], resp["requestId"])
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for the outcome")
	return cmd
}

// waitPush waits for every repository's git.push event for a request.
func (a *app) waitPush(cmd *cobra.Command, runID, requestID string) error {
	ctx := ctxOf(cmd)
	run, err := a.getRun(ctx, runID)
	if err != nil {
		return err
	}
	want := 0
	if run.Spec.Git != nil {
		want = len(run.Spec.Git.Repositories)
	}
	deadline := time.Now().Add(10 * time.Minute)
	for {
		var resp struct {
			Events []server.Event `json:"events"`
		}
		if err := a.c.Do(ctx, "GET", "/v1/runs/"+runID+"/events", nil, &resp); err != nil {
			return err
		}
		var results []map[string]any
		for _, e := range resp.Events {
			if e.Type == "git.push" && e.Data["requestId"] == requestID {
				results = append(results, e.Data)
			}
		}
		if len(results) >= want {
			if a.output == "json" {
				_ = a.json(results)
			}
			failed := false
			for _, r := range results {
				if a.output != "json" {
					fmt.Fprintf(a.stdout, "%s → %s: %s %s\n", r["repo"], r["branch"], r["status"], r["error"])
				}
				if r["status"] != "pushed" && r["status"] != "up-to-date" {
					failed = true
				}
			}
			if failed {
				return exitCode(1)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the push")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (a *app) snapshotsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "snapshots <run>",
		Short: "List a Run's snapshots and where each lives",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Snapshots []server.Snapshot `json:"snapshots"`
			}
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/runs/"+args[0]+"/snapshots", nil, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp.Snapshots)
			}
			var rows [][]string
			for _, s := range resp.Snapshots {
				var size int64
				for _, v := range s.Manifest.Volumes {
					size += v.Size
				}
				where := []string{}
				if s.OnHost != "" {
					where = append(where, "host "+s.OnHost)
				}
				if s.Uploaded {
					where = append(where, "s3")
				}
				if !s.Available {
					where = []string{"unavailable"}
				}
				rows = append(rows, []string{s.ID, fmt.Sprint(s.Epoch), bytesHuman(size), strings.Join(where, ", "), ago(&s.CreatedAt)})
			}
			a.table("ID\tEPOCH\tSIZE\tWHERE\tCREATED", rows)
			return nil
		},
	}
}

func (a *app) artifactsCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "artifacts <run>",
		Short: "List (or download) the files a Run produced",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Artifacts []server.Artifact `json:"artifacts"`
			}
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/runs/"+args[0]+"/artifacts", nil, &resp); err != nil {
				return err
			}
			if dir != "" {
				for _, art := range resp.Artifacts {
					dst := filepath.Join(dir, fmt.Sprint(art.Epoch), filepath.FromSlash(strings.TrimPrefix(art.Path, "/")))
					if err := a.download(cmd, art.ID, dst); err != nil {
						return fmt.Errorf("%s: %w", art.Path, err)
					}
					if a.output != "json" {
						fmt.Fprintln(a.stdout, dst)
					}
				}
			}
			if a.output == "json" {
				return a.json(resp.Artifacts)
			}
			if dir == "" {
				var rows [][]string
				for _, art := range resp.Artifacts {
					rows = append(rows, []string{art.ID, fmt.Sprint(art.Epoch), art.Path, bytesHuman(art.Size), art.ContentType})
				}
				a.table("ID\tEPOCH\tPATH\tSIZE\tTYPE", rows)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "download", "", "download every artifact into this directory")
	return cmd
}

func (a *app) download(cmd *cobra.Command, id, dst string) error {
	req, err := http.NewRequestWithContext(ctxOf(cmd), "GET", a.c.Base+"/v1/artifacts/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.c.Key)
	// The redirect goes to a presigned URL; the API key must not follow it.
	hc := &http.Client{CheckRedirect: func(r *http.Request, via []*http.Request) error {
		r.Header.Del("Authorization")
		return nil
	}}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, b)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func (a *app) hostsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "hosts", Short: "Hosts that run your workloads"}
	var all bool
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List hosts",
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Hosts []server.Host `json:"hosts"`
			}
			path := "/v1/hosts"
			if all {
				path += "?all=true"
			}
			if err := a.c.Do(ctxOf(cmd), "GET", path, nil, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp.Hosts)
			}
			var rows [][]string
			for _, h := range resp.Hosts {
				state := h.State
				if h.Draining && state != "draining" {
					state += " (draining)"
				}
				rows = append(rows, []string{h.Name, h.Pool, state, fmt.Sprint(h.LiveRuns), fmt.Sprintf("%g", h.Capacity.CPUs), bytesHuman(h.Capacity.Memory), ago(h.LastHeartbeat)})
			}
			a.table("NAME\tPOOL\tSTATE\tRUNS\tCPUS\tMEMORY\tHEARTBEAT", rows)
			return nil
		},
	}
	ls.Flags().BoolVar(&all, "all", false, "include terminated hosts")
	drain := &cobra.Command{
		Use:   "drain <host>",
		Short: "Stop placing Runs on a host and move its Runs elsewhere",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.c.Do(ctxOf(cmd), "POST", "/v1/hosts/"+args[0]+"/drain", map[string]any{}, nil)
		},
	}
	cmd.AddCommand(ls, drain)
	return cmd
}

func (a *app) poolsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "pools", Short: "Host pools"}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List pools",
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Pools []server.Pool `json:"pools"`
			}
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/pools", nil, &resp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(resp.Pools)
			}
			var rows [][]string
			for _, p := range resp.Pools {
				rows = append(rows, []string{p.Name, p.Provider, fmt.Sprintf("%d-%d", p.MinHosts, p.MaxHosts), fmt.Sprint(p.WarmHosts), fmt.Sprint(p.Shared)})
			}
			a.table("NAME\tPROVIDER\tHOSTS\tWARM\tSHARED", rows)
			return nil
		},
	}
	var p server.Pool
	var template string
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Create or update a pool",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p.Name = args[0]
			if template != "" {
				if err := jsonUnmarshalString(template, &p.Template); err != nil {
					return fmt.Errorf("--template: %w", err)
				}
			}
			return a.c.Do(ctxOf(cmd), "POST", "/v1/pools", p, nil)
		},
	}
	set.Flags().StringVar(&p.Provider, "provider", "static", "static | ec2")
	set.Flags().IntVar(&p.MinHosts, "min", 0, "minimum hosts")
	set.Flags().IntVar(&p.MaxHosts, "max", 0, "maximum hosts")
	set.Flags().IntVar(&p.WarmHosts, "warm", 0, "idle hosts to keep ready")
	set.Flags().StringVar(&template, "template", "", "provider template (JSON)")
	cmd.AddCommand(ls, set)
	return cmd
}
