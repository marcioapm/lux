#!/bin/sh
# Spike only: runs inside a throwaway simulated host. Checks the hidden
# procfs hook fires for an annotated container, outside lux, with the
# flags a Lux nested Run gets.
mkdir -p /usr/share/containers/oci/hooks.d
echo containers:2147483647:2147483648 >> /etc/subuid; echo containers:2147483647:2147483648 >> /etc/subgid
sed s/STAGE/poststart/g /usr/local/share/lux-spike-hook.json.in > /usr/share/containers/oci/hooks.d/lux-spike.json
podman pull -q docker.io/library/alpine:3.24.2 >/dev/null 2>&1
for flags in "" "--userns=auto:size=65536" "--userns=auto:size=65536 --cap-drop=ALL --security-opt=unmask=ALL --cgroupns=private --pid=private --device=/dev/fuse"; do
  echo "=== flags: $flags"
  podman run --rm $flags --tmpfs /dev/.lux/proc:ro,size=4k,mode=0555 --tmpfs /dev/.lux/sys:ro,size=4k,mode=0555 --annotation io.lux.spike.hidden=proc,sys docker.io/library/alpine:3.24.2 sh -c 'sleep 1; grep " /dev/.lux" /proc/self/mountinfo; ls /dev/.lux/proc | head -3' 2>&1
  cat /var/log/lux-spike-hook.log; rm -f /var/log/lux-spike-hook.log
done
