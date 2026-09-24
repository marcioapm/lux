package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// Spot interruptions. EC2 announces, about two minutes ahead, that it will
// take a spot instance back: GET /latest/meta-data/spot/instance-action
// answers {"action": "terminate"|"stop"|"hibernate", "time": …} instead of
// 404. The runner watches for it and tells luxd, which preempts the host's
// Runs: each stops, snapshots, and resumes on another host. Stops from then
// on get half the time left as grace, leaving the rest to snapshot and
// upload.

// spotPoll is how often the notice is checked (AWS suggests every 5s).
const spotPoll = 5 * time.Second

func (r *Runner) watchSpot(ctx context.Context, base string) {
	m := &imds{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 5 * time.Second}}
	for {
		action, at, err := m.instanceAction(ctx)
		switch {
		case err != nil:
			r.log.Debug("spot notice check", "err", err)
		case action != "":
			r.log.Warn("spot interruption notice: moving Runs away", "action", action, "at", at)
			r.evictBy.Store(&evicting{at: at, reason: "spot interruption (" + action + ")"})
			r.reportEvicting(ctx)
			return // one notice per instance
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(spotPoll):
		}
	}
}

type evicting struct {
	at     time.Time
	reason string
}

// reportEvicting tells luxd this host is going, until luxd has it: the
// Runs must be moved while there is time. Sent again for any assign that
// raced the drain, so luxd preempts that placement too.
func (r *Runner) reportEvicting(ctx context.Context) {
	ev := r.evictBy.Load()
	if ev == nil {
		return
	}
	for ctx.Err() == nil && time.Now().Before(ev.at) {
		err := r.conn.Report(ctx, proto.Frame{Type: proto.MsgHostEvicting,
			Data: proto.Marshal(proto.Evicting{Deadline: ev.at, Reason: ev.reason})})
		if err == nil {
			return
		}
		r.log.Warn("reporting the eviction", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// evictionGrace is the grace a stop may use: the workload's own, or half the
// time left before the host goes if it is being evicted.
func (r *Runner) evictionGrace(grace time.Duration) time.Duration {
	ev := r.evictBy.Load()
	if ev == nil {
		return grace
	}
	return max(min(grace, time.Until(ev.at)/2), time.Second)
}

// imds is an EC2 instance metadata client (IMDSv2: a session token first).
type imds struct {
	base  string
	http  *http.Client
	token string
	exp   time.Time
}

func (m *imds) get(ctx context.Context, path string) (int, []byte, error) {
	if m.token == "" || time.Now().After(m.exp) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, m.base+"/latest/api/token", nil)
		req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "21600")
		resp, err := m.http.Do(req)
		if err != nil {
			return 0, nil, err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, nil, fmt.Errorf("imds token: %s", resp.Status)
		}
		m.token, m.exp = string(b), time.Now().Add(6*time.Hour-time.Minute)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path, nil)
	req.Header.Set("X-aws-ec2-metadata-token", m.token)
	resp, err := m.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		m.token = "" // expired early: a new one next time
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, b, err
}

// instanceAction returns the pending spot action and its time, or "" if
// none is scheduled.
func (m *imds) instanceAction(ctx context.Context) (string, time.Time, error) {
	code, body, err := m.get(ctx, "/latest/meta-data/spot/instance-action")
	if err != nil {
		return "", time.Time{}, err
	}
	switch code {
	case http.StatusNotFound:
		return "", time.Time{}, nil
	case http.StatusOK:
	default:
		return "", time.Time{}, fmt.Errorf("imds instance-action: %d", code)
	}
	var v struct {
		Action string    `json:"action"`
		Time   time.Time `json:"time"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", time.Time{}, fmt.Errorf("imds instance-action: %w", err)
	}
	if v.Action == "" {
		return "", time.Time{}, errors.New("imds instance-action: no action")
	}
	return v.Action, v.Time, nil
}
