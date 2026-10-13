package shim

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// A reclamation signals SIGTERM to every group at once and schedules
// SIGKILL exactly at the deadline, not at the workload's grace.
func TestReclaimSignalsTermNowKillAtDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	var sent []string
	var delay time.Duration
	var fire func()
	kill := func(pid int, sig syscall.Signal) { sent = append(sent, sig.String()) }
	after := func(d time.Duration, f func()) *time.Timer { delay, fire = d, f; return nil }
	reclaimSignals([]int{10, 20}, now.Add(15*time.Second), now, kill, after)
	if len(sent) != 2 || sent[0] != syscall.SIGTERM.String() || delay != 15*time.Second {
		t.Fatalf("sent %v, kill in %v", sent, delay)
	}
	fire()
	if len(sent) != 4 || sent[3] != syscall.SIGKILL.String() {
		t.Fatalf("after deadline %v", sent)
	}
	reclaimSignals(nil, now, now.Add(time.Minute), kill, after)
	if delay != 0 {
		t.Fatalf("past deadline delayed %v", delay)
	}
}

// Real processes: a workload ignoring SIGTERM, and its nested child, are
// both gone once the deadline passes; the deadline is short here, not 15s.
func TestReclaimKillsNestedProcessGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 60 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pg := cmd.Process.Pid
	reclaimSignals([]int{pg}, time.Now().Add(200*time.Millisecond), time.Now(),
		func(pid int, sig syscall.Signal) { _ = syscall.Kill(-pid, sig) }, time.AfterFunc)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("workload survived the deadline")
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(-pg, 0); err == nil {
		t.Fatal("a process of the group survived")
	}
}
