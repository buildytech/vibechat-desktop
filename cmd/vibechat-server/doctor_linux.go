package main

import (
	"os"
	"strings"
	"syscall"

	"github.com/buildytech/vibechat-desktop/internal/procgroup"
)

// kernelChecks: the SDK sandbox on Linux is Landlock + bubblewrap (user namespaces) + seccomp.
func kernelChecks() []check {
	var cs []check
	if raw, err := os.ReadFile("/sys/kernel/security/lsm"); err != nil {
		cs = append(cs, warn("landlock", "cannot read /sys/kernel/security/lsm: "+err.Error()))
	} else if lsm := strings.TrimSpace(string(raw)); strings.Contains(","+lsm+",", ",landlock,") {
		cs = append(cs, ok("landlock", "active ("+lsm+")"))
	} else {
		cs = append(cs, bad("landlock", "not in the active LSMs ("+lsm+"): boot with lsm=...,landlock"))
	}
	cmd := procgroup.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER}
	if err := cmd.Run(); err != nil {
		cs = append(cs, bad("userns", "cannot create a user namespace ("+err.Error()+"): Incus needs security.nesting=true; sysctl kernel.unprivileged_userns_clone=1"))
	} else {
		cs = append(cs, ok("userns", "unprivileged user namespaces work"))
	}
	// Some kernels (AppArmor) let unconfined processes create user namespaces without capabilities inside: the
	// sandbox then fails at its loopback setup (RTM_NEWADDR: operation not permitted).
	if raw, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); err == nil && strings.TrimSpace(string(raw)) == "1" {
		cs = append(cs, warn("apparmor", "kernel.apparmor_restrict_unprivileged_userns=1: user namespaces get no capabilities, so the agent sandbox cannot set up its network namespace (sysctl it to 0)"))
	}
	return cs
}
