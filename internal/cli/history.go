package cli

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marcioapm/lux/internal/server"
)

func (a *app) historyCmd() *cobra.Command {
	var since, res string
	var host bool
	cmd := &cobra.Command{
		Use:   "history [run | --host host]",
		Short: "Resource use and system state over time",
		Long: `Without arguments, the system's history: Runs, queue, time to start,
hosts and capacity (an operator's whole system; --tenant narrows it). With a
Run, its resource use across placements; with --host, a host's.

--since is how far back (1h, 24h, 7d; default 1h). The resolution is the
finest kept for that range (raw, 60 or 3600 seconds), or --res.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{"since": {since}}
			if res != "" {
				q.Set("res", res)
			}
			path := "/v1/history"
			switch {
			case host && len(args) == 1:
				path = "/v1/hosts/" + args[0] + "/history"
			case host:
				return fmt.Errorf("--host needs a host")
			case len(args) == 1:
				path = "/v1/runs/" + args[0] + "/history"
			}
			var h server.History
			if err := a.c.Do(ctxOf(cmd), "GET", path+"?"+q.Encode(), nil, &h); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(h)
			}
			if len(h.Samples) == 0 {
				fmt.Fprintln(a.stdout, "no samples in this range")
				return nil
			}
			switch {
			case path == "/v1/history":
				a.systemHistory(h)
			default:
				a.usageHistory(h, !host)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "1h", "how far back (e.g. 30m, 24h, 7d)")
	cmd.Flags().StringVar(&res, "res", "", "resolution in seconds: 0 (raw), 60, 3600")
	cmd.Flags().BoolVar(&host, "host", false, "the argument is a host (id or name)")
	return cmd
}

// The text form: a line per series with its sparkline, then the latest
// value and the range.

// series is one line of history: how to read it and print its values.
type series struct {
	name   string
	get    func(server.Sample) *float64
	format func(float64) string
}

func (a *app) usageHistory(h server.History, run bool) {
	all := []series{
		{"cpu", func(s server.Sample) *float64 { return s.CPUCores }, cores},
		{"memory", func(s server.Sample) *float64 { return f64(s.MemoryBytes) }, bytesF},
		{"disk", func(s server.Sample) *float64 { return f64(s.DiskBytes) }, bytesF},
	}
	if run {
		all = append(all,
			series{"pids", func(s server.Sample) *float64 { return fInt(s.Pids) }, plain},
			series{"net rx", func(s server.Sample) *float64 { return s.NetRxRate }, rateF},
			series{"net tx", func(s server.Sample) *float64 { return s.NetTxRate }, rateF})
	} else {
		all = append(all,
			series{"placements", func(s server.Sample) *float64 { return fInt(s.Placements) }, plain},
			series{"alloc cpu", func(s server.Sample) *float64 { return s.AllocCPUs }, cores})
	}
	a.header(h)
	for _, s := range all {
		a.spark(s, h.Samples)
	}
}

func (a *app) systemHistory(h server.History) {
	count := func(get func(server.Sample) *int) func(server.Sample) *float64 {
		return func(s server.Sample) *float64 { return fInt(get(s)) }
	}
	a.header(h)
	for _, s := range []series{
		{"running", func(s server.Sample) *float64 { return fInt(ptr(s.Runs["running"])) }, plain},
		{"busy", count(func(s server.Sample) *int { return s.Busy }), plain},
		{"queued", count(func(s server.Sample) *int { return s.Queued }), plain},
		{"started", count(func(s server.Sample) *int { return s.Started }), plain},
		{"finished", count(func(s server.Sample) *int { return s.Finished }), plain},
		{"start p95", func(s server.Sample) *float64 { return s.StartP95 }, secs},
		{"hosts ready", func(s server.Sample) *float64 { return fInt(ptr(s.Hosts["ready"])) }, plain},
		{"alloc cpu", func(s server.Sample) *float64 { return s.SysAllocC }, cores},
		{"capacity cpu", func(s server.Sample) *float64 { return s.CapCPUs }, cores},
	} {
		a.spark(s, h.Samples)
	}
}

func (a *app) header(h server.History) {
	res := "raw"
	if h.Resolution > 0 {
		res = (time.Duration(h.Resolution) * time.Second).String()
	}
	fmt.Fprintf(a.stdout, "%s → %s, %d samples (%s)\n", h.From.Local().Format("Jan 2 15:04"), h.To.Local().Format("Jan 2 15:04"), len(h.Samples), res)
}

var sparks = []rune("▁▂▃▄▅▆▇█")

// spark prints one series: name, sparkline (at most 60 wide), last, min–max.
func (a *app) spark(sr series, samples []server.Sample) {
	var vs []float64
	for _, s := range samples {
		if v := sr.get(s); v != nil {
			vs = append(vs, *v)
		}
	}
	if len(vs) == 0 {
		return
	}
	// Downsample to 60 columns by taking each column's maximum.
	cols := min(len(vs), 60)
	col := make([]float64, cols)
	for i := range col {
		lo, hi := i*len(vs)/cols, (i+1)*len(vs)/cols
		col[i] = vs[lo]
		for _, v := range vs[lo:hi] {
			col[i] = max(col[i], v)
		}
	}
	lo, hi := vs[0], vs[0]
	for _, v := range vs {
		lo, hi = min(lo, v), max(hi, v)
	}
	var b strings.Builder
	for _, v := range col {
		i := 0
		if hi > lo {
			i = int((v - lo) / (hi - lo) * float64(len(sparks)-1))
		}
		b.WriteRune(sparks[i])
	}
	fmt.Fprintf(a.stdout, "%-13s %-60s  %s  (%s–%s)\n", sr.name, b.String(), sr.format(vs[len(vs)-1]), sr.format(lo), sr.format(hi))
}

func f64(v *int64) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

func fInt(v *int) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

func ptr(v int) *int { return &v }

func cores(v float64) string  { return fmt.Sprintf("%.2f cores", v) }
func bytesF(v float64) string { return bytesHuman(int64(v)) }
func rateF(v float64) string  { return bytesHuman(int64(v)) + "/s" }
func plain(v float64) string  { return fmt.Sprintf("%g", v) }
func secs(v float64) string   { return fmt.Sprintf("%.1fs", v) }
