package runner

import (
	"os"
	"strings"
)

// Spike only (.spike/lxcfs.md), off unless the runner's environment asks:
//
//	LUX_SPIKE_LXCFS=<dir>       LXCFS mountpoint on the host; its proc files
//	                            are bound over the Run's /proc files
//	LUX_SPIKE_LXCFS_SYS=online|cpu|none
//	                            what of <dir>/sys/devices/system/cpu is
//	                            bound (default online)
//	LUX_SPIKE_HIDDEN_PROC=1     annotate the container so the spike OCI hook
//	                            adds a clean procfs at /dev/.lux/proc
//	LUX_SPIKE_HIDDEN_SYS=1      and a clean sysfs at /dev/.lux/sys
var lxcfsProcFiles = []string{"meminfo", "cpuinfo", "stat", "uptime", "loadavg", "diskstats", "swaps"}

const spikeHiddenAnnotation = "io.lux.spike.hidden"

func spikeLXCFSArgs() []string {
	var args []string
	if dir := os.Getenv("LUX_SPIKE_LXCFS"); dir != "" {
		for _, f := range lxcfsProcFiles {
			args = append(args, "-v", dir+"/proc/"+f+":/proc/"+f+":ro")
		}
		switch os.Getenv("LUX_SPIKE_LXCFS_SYS") {
		case "none":
		case "cpu":
			args = append(args, "-v", dir+"/sys/devices/system/cpu:/sys/devices/system/cpu:ro")
		default:
			args = append(args, "-v", dir+"/sys/devices/system/cpu/online:/sys/devices/system/cpu/online:ro")
		}
	}
	var hidden []string
	if os.Getenv("LUX_SPIKE_HIDDEN_PROC") == "1" {
		hidden = append(hidden, "proc")
	}
	if os.Getenv("LUX_SPIKE_HIDDEN_SYS") == "1" {
		hidden = append(hidden, "sys")
	}
	if len(hidden) > 0 {
		// Empty mountpoints the hook mounts over: it runs as host root,
		// which the Run's user namespace does not map, so it cannot make them.
		for _, h := range hidden {
			args = append(args, "--tmpfs", "/dev/.lux/"+h+":ro,size=4k,mode=0555")
		}
		args = append(args, "--annotation", spikeHiddenAnnotation+"="+strings.Join(hidden, ","))
	}
	return args
}
