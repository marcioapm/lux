package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// httpCall calls a lux service (workload.services) through its socket:
//
//	http [-H 'Name: value']... <service> <METHOD> <path> [body]
//
// It says "status <code>", then the response body line by line as it
// arrives (a stream shows as it streams).
func (a *agent) httpCall(args string) {
	var extra [][2]string
	for strings.HasPrefix(args, "-H ") {
		rest := strings.TrimPrefix(args, "-H ")
		q := rest[:1]
		if q != "'" && q != `"` {
			a.say("error: -H needs a quoted header")
			return
		}
		end := strings.Index(rest[1:], q)
		if end < 0 {
			a.say("error: unterminated -H")
			return
		}
		k, v, _ := strings.Cut(rest[1:1+end], ":")
		extra = append(extra, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
		args = strings.TrimSpace(rest[2+end:])
	}
	parts := strings.SplitN(args, " ", 4)
	if len(parts) < 3 {
		a.say("error: http [-H 'K: v'] <service> <METHOD> <path> [body]")
		return
	}
	name, method, path := parts[0], parts[1], parts[2]
	env := "LUX_SERVICE_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	addr, ok := strings.CutPrefix(os.Getenv(env), "unix:")
	if !ok {
		a.say("error: no service " + name + " (" + env + " unset)")
		return
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", addr)
	}}, Timeout: 2 * time.Minute}
	var body *strings.Reader
	if len(parts) == 4 {
		body = strings.NewReader(parts[3])
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequest(method, "http://"+name+path, body)
	if err != nil {
		a.say("error: " + err.Error())
		return
	}
	if len(parts) == 4 {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range extra {
		req.Header.Set(h[0], h[1])
	}
	resp, err := client.Do(req)
	if err != nil {
		a.say("error: " + err.Error())
		return
	}
	defer resp.Body.Close()
	a.say(fmt.Sprintf("status %d", resp.StatusCode))
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			a.say(line)
		}
	}
}
