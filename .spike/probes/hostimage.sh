#!/bin/sh
# What the simulated host image offers for LXCFS.
head -3 /etc/os-release
command -v python3 node lxcfs fusermount3 || true
ls -l /dev/fuse
dnf -q --setopt=install_weak_deps=False info lxcfs 2>&1 | grep -E '^(Name|Version|Release|Repo)'
