package aurorafleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped sandbox profiles are pinned from the package directory so the
// ownership lock runs without Docker, AppArmor, or a Linux kernel. This is the
// non-gated half of the Linux boundary acceptance: it fixes which layer owns
// the mount denial so the Linux host only has to observe it.
var (
	seccompProfilePath  = filepath.Join("..", "..", "..", "deploy", "aurora-sandbox", "seccomp.json")
	appArmorProfilePath = filepath.Join("..", "..", "..", "deploy", "aurora-sandbox", "multica-aurora-sandbox.apparmor")
)

// mountSyscalls is the mount syscall family the sandbox must deny as one unit.
// A workspace that could issue any of these could build its own filesystem view
// or move the mounts the policy set up.
var mountSyscalls = []string{
	"mount", "umount", "umount2", "pivot_root", "mount_setattr",
	"move_mount", "open_tree", "fsopen", "fsmount", "fspick",
}

// TestSeccompProfileOwnsMountDenial pins the single owner of the mount denial:
// seccomp.json terminates every mount syscall with SCMP_ACT_KILL_PROCESS. If a
// future edit weakens one of those actions or drops a syscall, the Linux
// adversarial probe would stop being terminally denied; this test catches it on
// any platform. encoding/json matches field names case-insensitively, so the
// struct needs no tags for the lowercase profile keys.
func TestSeccompProfileOwnsMountDenial(t *testing.T) {
	raw, err := os.ReadFile(seccompProfilePath)
	if err != nil {
		t.Fatalf("read seccomp profile: %v", err)
	}
	var profile struct {
		Syscalls []struct {
			Names  []string
			Action string
		}
	}
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatalf("parse seccomp profile: %v", err)
	}
	killed := map[string]bool{}
	for _, rule := range profile.Syscalls {
		if rule.Action != "SCMP_ACT_KILL_PROCESS" {
			continue
		}
		for _, name := range rule.Names {
			killed[name] = true
		}
	}
	for _, name := range mountSyscalls {
		if !killed[name] {
			t.Errorf("seccomp profile does not kill %s with SCMP_ACT_KILL_PROCESS", name)
		}
	}
}

// TestAppArmorProfileLeavesMountToSeccomp keeps exactly one layer owning the
// observable mount denial. seccomp runs at syscall entry, before AppArmor's LSM
// hook, so an AppArmor mount deny would be an unobservable duplicate that only
// obscures which control the acceptance is proving. The profile still carries
// the controls seccomp does not express (raw sockets, the Docker socket, kernel
// and sysfs writes), so this is a clear ownership split, not a weakened sandbox.
func TestAppArmorProfileLeavesMountToSeccomp(t *testing.T) {
	raw, err := os.ReadFile(appArmorProfilePath)
	if err != nil {
		t.Fatalf("read AppArmor profile: %v", err)
	}
	profile := string(raw)
	for _, rule := range []string{"deny mount", "deny umount", "deny pivot_root"} {
		if containsAppArmorRule(profile, rule) {
			t.Errorf("AppArmor profile still carries %q, duplicating the seccomp mount owner", rule)
		}
	}
	for _, rule := range []string{"deny network raw", "deny /var/run/docker.sock", "deny /proc/kcore"} {
		if !containsAppArmorRule(profile, rule) {
			t.Errorf("AppArmor profile lost its non-overlapping control %q", rule)
		}
	}
}

// containsAppArmorRule reports whether the profile has an active (non-comment)
// rule starting with the given text.
func containsAppArmorRule(profile, rule string) bool {
	for _, line := range strings.Split(profile, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, rule) {
			return true
		}
	}
	return false
}
