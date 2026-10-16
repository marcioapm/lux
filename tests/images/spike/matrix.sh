#!/bin/sh
# The acceptance matrix, against whatever engine DOCKER_HOST points at, with
# the docker CLI. Each row prints "ROW<n> PASS|FAIL <detail>"; errors are
# printed in full, indented, under the row. ENGINE_LOG names the engine's log,
# whose tail follows a failed row. SKIP_ROWS lists rows not to run.
A=docker.io/library/alpine:3.24.2
log=${ENGINE_LOG:-/dev/null}
row() { echo "ROW$1 $2 $3"; [ "$2" = PASS ] || { sed 's/^/    | /' /tmp/row.out | tail -40; echo "    engine log:"; tail -25 "$log" | sed 's/^/    > /'; }; }
skip() { case " $SKIP_ROWS " in *" $1 "*) echo "ROW$1 SKIP"; return 0;; esac; return 1; }
alloc() { echo "awk 'BEGIN{x=\"a\";for(i=0;i<$1;i++)x=x x;print length(x)}'"; }

echo "engine: $(docker version --format '{{.Server.Version}}' 2>&1 | head -1) cgroupdriver=$(docker info --format '{{.CgroupDriver}}' 2>&1 | head -1) security=$(docker info --format '{{.SecurityOptions}}' 2>&1 | head -1)"
echo "run: memory.max=$(cat /sys/fs/cgroup/memory.max) cgroup=$(cat /proc/self/cgroup) uid_map=$(awk '{print $2}' /proc/self/uid_map | tr '\n' ,) $(grep CapEff /proc/self/status | tr -s '\t ' ' ')"
docker image inspect $A >/dev/null 2>&1 || docker load -q -i /opt/alpine.tar >/dev/null 2>&1 || echo "LOAD-FAILED"

# 1. memory limit and OOM
if ! skip 1; then
  docker rm -f m1 >/dev/null 2>&1
  docker run --name m1 --network none -m 128m --memory-swap 128m $A sh -c "echo inner-mem=\$(cat /sys/fs/cgroup/memory.max); $(alloc 28)" >/tmp/row.out 2>&1
  rc=$?
  oom=$(docker inspect m1 --format '{{.State.OOMKilled}}:{{.State.ExitCode}}' 2>>/tmp/row.out)
  mem=$(grep -o 'inner-mem=[0-9a-z]*' /tmp/row.out | cut -d= -f2)
  if [ "$mem" = 134217728 ] && [ "$oom" = "true:137" ]; then row 1 PASS "mem=$mem oom=$oom rc=$rc"; else row 1 FAIL "mem=$mem oom=$oom rc=$rc"; fi
  docker rm -f m1 >/dev/null 2>&1
fi

# 2. cpu and pids
if ! skip 2; then
  docker run --rm --network none --cpus 0.5 --pids-limit 64 $A sh -c 'echo "cpu=$(tr " " _ < /sys/fs/cgroup/cpu.max) pids=$(cat /sys/fs/cgroup/pids.max) cg=$(cat /proc/self/cgroup)"' >/tmp/row.out 2>&1
  got=$(grep -o 'cpu=[^ ]* pids=[^ ]*' /tmp/row.out)
  if [ "$got" = "cpu=50000_100000 pids=64" ]; then row 2 PASS "$got"; else row 2 FAIL "$got"; fi
fi

# 3. port publishing, reached from the Run itself
if ! skip 3; then
  docker rm -f p3 >/dev/null 2>&1
  docker run -d --name p3 -p 8080:80 $A sh -c 'while :; do echo port-ok | nc -l -p 80; done' >/tmp/row.out 2>&1
  got=""
  for i in $(seq 15); do got=$(nc -w 3 127.0.0.1 8080 </dev/null 2>>/tmp/row.out); [ "$got" = port-ok ] && break; sleep 1; done
  docker port p3 >>/tmp/row.out 2>&1
  if [ "$got" = port-ok ]; then row 3 PASS "127.0.0.1:8080"; else row 3 FAIL "got='$got'"; fi
  docker rm -f p3 >/dev/null 2>&1
fi

# 4. docker build, 2 steps (BuildKit through the engine's API, then classic)
mkdir -p /tmp/b4 && printf 'FROM %s\nRUN echo one > /one\nRUN echo two > /two\nCMD cat /one /two\n' $A > /tmp/b4/Dockerfile
if ! skip 4; then
  docker build -q -t spike-b4 /tmp/b4 >/tmp/row.out 2>&1 && kit=$(docker run --rm spike-b4 | tr '\n' +)
  cp /tmp/row.out /tmp/row4kit.out
  DOCKER_BUILDKIT=0 docker build -q -t spike-b4c /tmp/b4 >/tmp/row.out 2>&1 && classic=$(docker run --rm spike-b4c | tr '\n' +)
  cat /tmp/row4kit.out >> /tmp/row.out
  if [ "$kit" = one+two+ ]; then row 4 PASS "buildkit=$kit classic=$classic"; else row 4 FAIL "buildkit=$kit classic=$classic"; fi
  docker rmi -f spike-b4 spike-b4c >/dev/null 2>&1
fi

# 5. buildx docker-container driver, network=host, pinned BuildKit, --load.
# The base comes from an OCI layout of alpine, so BuildKit needs no registry.
# 5b, if BX_CGROUP_PARENT is set: the same with --driver-opt cgroup-parent.
bx() {  # row, extra driver opts
  docker image inspect moby/buildkit:v0.33.1 >/dev/null 2>&1 || docker load -q -i /opt/buildkit.tar >/dev/null 2>&1
  mkdir -p /tmp/oci && tar -xf /opt/alpine.tar -C /tmp/oci
  dg=$(grep -o '"digest":"sha256:[0-9a-f]*"' /tmp/oci/index.json | head -1 | cut -d'"' -f4)
  printf 'FROM base\nRUN echo one > /one\nRUN echo two > /two; echo "step-cgroup=$(cat /proc/self/cgroup) step-mem=$(cat /sys/fs/cgroup/memory.max)"\nCMD cat /one /two\n' > /tmp/b4/Dockerfile.bx
  { docker buildx create --name spk --driver docker-container --driver-opt network=host --driver-opt image=moby/buildkit:v0.33.1 $2 --use \
    && docker buildx inspect --bootstrap spk \
    && docker buildx build --progress plain --no-cache --load -t spike-bx -f /tmp/b4/Dockerfile.bx --build-context "base=oci-layout:///tmp/oci@$dg" /tmp/b4; } >/tmp/row.out 2>&1
  bx=$(docker run --rm spike-bx 2>>/tmp/row.out | tr '\n' +)
  [ -n "$BX_SHOW_CGROUP" ] && { grep -o 'step-cgroup=0::.*' /tmp/row.out | head -1; docker exec buildx_buildkit_spk0 cat /proc/1/cgroup /sys/fs/cgroup/memory.max 2>&1 | tr '\n' ' ' | sed 's/^/buildkitd-cgroup: /'; echo; }
  echo "buildkit container:" >>/tmp/row.out; docker ps -a --filter name=buildx_buildkit_spk --format '{{.Names}} {{.Status}}' >>/tmp/row.out 2>&1
  docker inspect buildx_buildkit_spk0 --format 'cgroupparent={{.HostConfig.CgroupParent}} privileged={{.HostConfig.Privileged}} net={{.HostConfig.NetworkMode}}' >>/tmp/row.out 2>&1
  docker logs buildx_buildkit_spk0 2>&1 | tail -15 >>/tmp/row.out
  if [ "$bx" = one+two+ ]; then row $1 PASS "buildx=$bx"; else row $1 FAIL "buildx=$bx"; fi
  docker buildx rm -f spk >/dev/null 2>&1; docker buildx use default >/dev/null 2>&1
  docker rmi -f spike-bx >/dev/null 2>&1
}
skip 5 || bx 5 ""
[ -n "$BX_CGROUP_PARENT" ] && { skip 5b || bx 5b "--driver-opt cgroup-parent=$BX_CGROUP_PARENT"; }

# 6. compose: two services, one with deploy.resources.limits.memory
if ! skip 6; then
  mkdir -p /tmp/c6 && cat > /tmp/c6/compose.yaml <<EOF
services:
  limited:
    image: $A
    command: sh -c 'echo limited-mem=\$\$(cat /sys/fs/cgroup/memory.max); sleep 3; nc -w 3 other 8080 </dev/null'
    depends_on: [other]
    deploy:
      resources:
        limits:
          memory: 64M
  other:
    image: $A
    command: sh -c 'while :; do echo compose-ok | nc -l -p 8080; done'
EOF
  ( cd /tmp/c6 && docker compose -p spk up -d && sleep 8 && docker compose -p spk logs --no-color limited ) >/tmp/row.out 2>&1
  mem=$(grep -o 'limited-mem=[0-9a-z]*' /tmp/row.out | cut -d= -f2 | tail -1)
  ok=$(grep -c compose-ok /tmp/row.out)
  ( cd /tmp/c6 && docker compose -p spk down -t 1 ) >/dev/null 2>&1
  if [ "$mem" = 67108864 ] && [ "$ok" -ge 1 ]; then row 6 PASS "mem=$mem compose-ok"; else row 6 FAIL "mem=$mem compose-ok=$ok"; fi
fi

# Probes isolating BuildKit's container: privileged, host network, both.
if [ -n "$PROBE_PRIV" ]; then
  echo "probe: run /sys mount: $(awk '$5 == "/sys" {print $6}' /proc/self/mountinfo)"
  for o in "--network host" "--privileged" "--privileged --network host" "--privileged --network host --cgroup-parent /workload/probe"; do
    r=$(docker run --rm $o $A sh -c 'echo ok $(awk "\$5 == \"/sys\" {print \$6}" /proc/self/mountinfo)' 2>&1 | tail -1 | cut -c1-400)
    echo "probe[$o]: $r"
  done
fi

# 7. the Run's own limit bounds an inner container given more
if ! skip 7; then
  docker run --rm --network none -m 4g --memory-swap 4g $A sh -c "echo inner-mem=\$(cat /sys/fs/cgroup/memory.max); $(alloc 30)" >/tmp/row.out 2>&1
  rc=$?
  runmem=$(cat /sys/fs/cgroup/memory.max)
  if [ $rc = 137 ] && [ "$runmem" != max ]; then row 7 PASS "rc=$rc run-memory.max=$runmem"; else row 7 FAIL "rc=$rc run-memory.max=$runmem"; fi
  echo "row7: engine-alive=$(docker version --format ok 2>&1 | tail -1 | cut -c1-120)"
  for c in . workload workload/main; do echo "row7: memory.events[$c]: $(tr '\n' ' ' < /sys/fs/cgroup/$c/memory.events)"; done
  [ -n "$ENGINE_PID" ] && echo "row7: engine-pid $ENGINE_PID $(kill -0 $ENGINE_PID 2>&1 && echo alive)"
fi
echo MATRIX-ROWS-DONE
echo MATRIX-DONE
