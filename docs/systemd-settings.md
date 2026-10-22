# systemd settings for CPU isolation

Kubernetes does not keep operating system daemons off the isolated cores (see section B.2 of
[WHAT-WHY-HOW.md](./WHAT-WHY-HOW.md)). This has to be done on the systemd side.

The examples below assume kubelet's `reservedSystemCPUs: 0-9`, i.e. cores `0-9` are the
Housekeeping (HK) cores and `10-39` are the isolated ones. Adapt the core list to your node.

The settings are:

1. All slices are limited to the HK cores (`AllowedCPUs=0-9`), except `kubepods.slice`.
2. The same for the init (PID 1) process (`init.scope`).
3. `CPUAffinity=0-9` is set as the default for all service units.
4. kubelet's CPU Manager state is reset, to avoid "config change" errors after the reboot.

The procedure finishes with a reboot (see section 5), so the `systemctl daemon-reload` /
`systemctl daemon-reexec` commands are left commented out in each step. Run them only to apply a
step without rebooting.

## 1. Limit all slices to the HK cores, except `kubepods.slice`

```sh
for s in system.slice user.slice machine.slice; do \
  mkdir -p /etc/systemd/system/$s.d; \
  printf '[Slice]\nAllowedCPUs=0-9\n' > /etc/systemd/system/$s.d/50-housekeeping.conf ;\
done
# Following command not needed as we are finishing with a reboot (see section 5):
#
# $ systemctl daemon-reload
```

`kubepods.slice` is deliberately left out: kubelet manages the cpusets below it, and the plugin
needs the isolated cores to be available there.

`machine.slice` only has a cgroup while a VM or container is running in it, so it may be missing
from `/sys/fs/cgroup`. The drop-in is harmless until then.

## 2. Limit the init (PID 1) process to the HK cores

```sh
mkdir -p /etc/systemd/system/init.scope.d; \
  printf '[Scope]\nAllowedCPUs=0-9\n' > /etc/systemd/system/init.scope.d/50-housekeeping.conf
# Following command not needed as we are finishing with a reboot (see section 5):
#
# $ systemctl daemon-reload
```

> **IMPORTANT:** `init.scope` is a scope unit, not a slice, so the section **must** be `[Scope]`.


## 3. Set `CPUAffinity=0-9` as the default for all service units

```sh
mkdir -p /etc/systemd/system.conf.d
printf '[Manager]\nCPUAffinity=0-9\n' > /etc/systemd/system.conf.d/50-cpuaffinity.conf
# Following command not needed as we are finishing with a reboot (see section 5):
#
# $ systemctl daemon-reexec
```

This sets the CPU affinity of PID 1 and, by inheritance, of every process systemd starts. It also
covers processes that never end up in one of the slices above.

## 4. Reset kubelet's CPU Manager state

kubelet refuses to start if the CPU Manager settings no longer match the ones saved in
`/var/lib/kubelet/cpu_manager_state`. Remove that file before rebooting:

```sh
systemctl stop rke2-server   # or rke2-agent
rm /var/lib/kubelet/cpu_manager_state
```

## 5. Reboot

```sh
transactional-update grub.cfg   # only if the kernel command line was edited
reboot
```

## Verifying

```sh
# Slices and init.scope must be limited to the HK cores (machine.slice may not exist).
cat /sys/fs/cgroup/{system,user,machine}.slice/cpuset.cpus.effective   # 0-9
cat /sys/fs/cgroup/init.scope/cpuset.cpus.effective                    # 0-9
grep Cpus_allowed_list /proc/1/status                                  # 0-9

# No drop-in must have been ignored.
journalctl -b | grep -i "unknown section"
```

## Pitfalls

- **Leftover drop-ins.** systemd applies all the drop-ins of a unit in file name order, and
  the last one setting `AllowedCPUs=` wins. A leftover file (e.g. `50-allowedcpus.conf` with
  `AllowedCPUs=0-2`) is silently overridden by `50-housekeeping.conf` only because of how the names
  sort. Check what is actually loaded, and remove anything that is not wanted:

  ```sh
  systemctl show init.scope system.slice user.slice machine.slice -p Id -p AllowedCPUs -p DropInPaths
  ```
