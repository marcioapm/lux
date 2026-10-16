#!/bin/sh
# Prints one "P:<tag>:<key>=<value>" line per size a tool reports. Busybox
# is enough; python3/node lines appear only where they are installed.
t=${1:-run}
p() { echo "P:$t:$1=$2"; }
p meminfo.MemTotal "$(awk '/^MemTotal:/{print $2$3}' /proc/meminfo 2>&1)"
p meminfo.MemAvailable "$(awk '/^MemAvailable:/{print $2$3}' /proc/meminfo 2>&1)"
p free.total "$(free -k 2>&1 | awk '/^Mem:/{print $2"kB"}')"
p cpuinfo.processors "$(grep -c '^processor' /proc/cpuinfo 2>&1)"
p stat.cpuN "$(grep -c '^cpu[0-9]' /proc/stat 2>&1)"
p uptime "$(cut -d' ' -f1 /proc/uptime 2>&1)"
p loadavg "$(cut -d' ' -f1-3 /proc/loadavg 2>&1 | tr ' ' _)"
p swaps.lines "$(wc -l < /proc/swaps 2>&1)"
p sys.cpu.online "$(cat /sys/devices/system/cpu/online 2>&1)"
p nproc "$(nproc 2>&1)"
p getconf.NPROCESSORS_ONLN "$(getconf _NPROCESSORS_ONLN 2>&1)"
p cgroup "$(cat /proc/self/cgroup 2>&1)"
p cgroup.memory.max "$(cat /sys/fs/cgroup/memory.max 2>&1)"
p cgroup.cpu.max "$(cat /sys/fs/cgroup/cpu.max 2>&1 | tr ' ' _)"
p cgroup.cpuset.effective "$(cat /sys/fs/cgroup/cpuset.cpus.effective 2>&1)"
if command -v python3 >/dev/null; then
  p python.cpu_count "$(python3 -c 'import os;print(os.cpu_count())' 2>&1)"
  p python.sched_getaffinity "$(python3 -c 'import os;print(len(os.sched_getaffinity(0)))' 2>&1)"
  p python.process_cpu_count "$(python3 -c 'import os;print(getattr(os,"process_cpu_count",lambda:"n/a")())' 2>&1)"
fi
if command -v node >/dev/null; then
  p node.cpus "$(node -e 'console.log(require("os").cpus().length)' 2>&1)"
  p node.availableParallelism "$(node -e 'console.log(require("os").availableParallelism())' 2>&1)"
  p node.totalmem "$(node -e 'console.log(require("os").totalmem())' 2>&1)"
fi
p proc.mounts "$(awk '$2 ~ /^\/(proc|sys)/ || $2 ~ /^\/dev\/\.lux/ {printf "%s(%s) ", $2, $1}' /proc/self/mounts | head -c 1500)"
