#!/bin/sh
# Spike only: runs on the simulated host. For each cgroup given, puts a
# reader of the LXCFS files in it, as an ordinary process and as pid 1 of a
# new pid namespace (as a container's init), and prints what it saw.
# Usage: lxcfs-reader.sh <lxcfs dir> <cgroup path>...
dir=$1; shift
read_all() {
  echo "cg=$(cut -d: -f3 /proc/self/cgroup) pid=$$ mem=$(awk '/^MemTotal/{print $2}' $dir/proc/meminfo) cpus=$(grep -c ^processor $dir/proc/cpuinfo) online=$(cat $dir/sys/devices/system/cpu/online)"
}
if [ "$READER_CHILD" = 1 ]; then read_all; exit; fi
for cg in "$@"; do
  echo "D:target $cg memory.max=$(cat /sys/fs/cgroup$cg/memory.max 2>&1) cpu.max=$(cat /sys/fs/cgroup$cg/cpu.max 2>&1)"
  for ns in none pid; do
    if [ $ns = pid ]; then
      r=$(sh -c "echo \$\$ > /sys/fs/cgroup$cg/cgroup.procs && READER_CHILD=1 exec unshare -p -f sh $0 $dir" 2>&1)
    else
      r=$(sh -c "echo \$\$ > /sys/fs/cgroup$cg/cgroup.procs && READER_CHILD=1 exec sh $0 $dir" 2>&1)
    fi
    echo "D:reader pidns=$ns: $r"
  done
done
