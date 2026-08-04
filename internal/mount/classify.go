package mount

import (
	"os"
	"sort"
	"strings"
)

// networkFilesystems are the types whose latency changes how the pipeline
// should behave, not merely where the bytes live. Reading a 4K source over one
// of these for every derive stage is the difference between a library that
// indexes overnight and one that does not finish.
//
// drvfs and 9p are here for the same reason even though they are local in the
// sense that no network card is involved: under WSL2 they cross the hypervisor
// boundary per read, and measure like a slow NAS.
var networkFilesystems = map[string]string{
	"cifs":        "SMB",
	"smb3":        "SMB",
	"smbfs":       "SMB",
	"nfs":         "NFS",
	"nfs4":        "NFS",
	"afpfs":       "AFP",
	"fuse.sshfs":  "SSHFS",
	"fuse.rclone": "rclone",
	"9p":          "WSL/9p",
	"drvfs":       "WSL/drvfs",
	"virtiofs":    "virtiofs",
}

// Filesystem describes where a path actually lives.
type Filesystem struct {
	// Type is the kernel's name for it, empty when it could not be determined.
	Type string
	// Mountpoint is the mount the path falls under.
	Mountpoint string
	// Network is true when reads cross a network or a hypervisor boundary.
	Network bool
	// Label names the protocol for a human, empty for a local filesystem.
	Label string
	// ReadOnly reports whether the mount forbids writes, which for a footage
	// root is the desired state rather than a problem.
	ReadOnly bool
}

// ReadMountTable returns the contents of the kernel's mount table, or an empty
// string on systems that do not publish one. An empty result means "unknown",
// never "local": claiming a path is local when it cannot be checked would turn
// this from advice into misinformation.
func ReadMountTable() string {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	return string(data)
}

// mountEntry is one line of /proc/self/mountinfo reduced to the fields this
// package reads. The numbers in the field comments are the 1-based positions
// the kernel documents in Documentation/filesystems/proc.rst.
type mountEntry struct {
	// root is field 4: the root of this mount *within its own source
	// filesystem*. For a bind mount that is the source directory — which is
	// how MediaBind recovers a host-side path from inside a container — and
	// for an ordinary whole-filesystem mount it is simply "/".
	root string
	// mountpoint is field 5: where the mount appears in this namespace.
	mountpoint string
	// options is field 6, the per-mount options ("ro", "relatime", …).
	options string
	// fstype is the first field after the "-" separator.
	fstype string
}

// parseMountTable is the only parser for /proc/self/mountinfo in this package.
// It exists as one function rather than being inlined at each call site
// because the format has a trap — a variable number of optional fields before
// the "-" separator — and a second copy of that rule is a second place for it
// to be got wrong.
func parseMountTable(mountTable string) []mountEntry {
	var entries []mountEntry
	for _, line := range strings.Split(mountTable, "\n") {
		// mountinfo: ID parent major:minor root mountpoint options... - fstype source superopts
		// The optional fields before the separator are why the tail is found by
		// searching for "-" rather than by counting from the left.
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		separator := -1
		for i := 5; i < len(fields); i++ {
			if fields[i] == "-" {
				separator = i
				break
			}
		}
		if separator < 0 || separator+1 >= len(fields) {
			continue
		}
		entries = append(entries, mountEntry{
			root:       unescapeMountField(fields[3]),
			mountpoint: unescapeMountField(fields[4]),
			options:    fields[5],
			fstype:     fields[separator+1],
		})
	}
	return entries
}

// FilesystemFor resolves which mount a path belongs to. It takes the mount
// table as text so the classification is testable on any machine, including
// against tables captured from a NAS.
//
// The path must already be absolute and cleaned.
func FilesystemFor(path, mountTable string) (Filesystem, bool) {
	entries := parseMountTable(mountTable)
	if len(entries) == 0 {
		return Filesystem{}, false
	}
	// The longest matching mount point wins: /mnt and /mnt/footage can both
	// contain the path and only the deeper one describes it.
	sort.SliceStable(entries, func(i, j int) bool {
		return len(entries[i].mountpoint) > len(entries[j].mountpoint)
	})
	for _, candidate := range entries {
		if !underMountpoint(path, candidate.mountpoint) {
			continue
		}
		label, network := networkFilesystems[candidate.fstype]
		return Filesystem{
			Type:       candidate.fstype,
			Mountpoint: candidate.mountpoint,
			Network:    network,
			Label:      label,
			ReadOnly:   hasOption(candidate.options, "ro"),
		}, true
	}
	return Filesystem{}, false
}

// mediaBindTargets are the container-side paths this project's own deployment
// artifacts bind footage to: docker-compose.yml's media bind and both Unraid
// templates in deploy/ use /media/library. Discovery is restricted to that
// list on purpose — picking "whichever mount looks like media" out of an
// arbitrary table would be a guess, and a guessed path is worse than no path
// here, because the operator will follow it and the mount will land where the
// Hub can never see it.
var mediaBindTargets = []string{"/media/library"}

// Bind is the bind mount that carries footage from the Docker host into this
// container. The two halves are different filesystems' names for one
// directory, and confusing them is the whole reason this type exists: mount
// commands run on the host and must use Source, while everything that runs
// against the Hub — verification, `root add`, the recorded library root path —
// must use Target.
type Bind struct {
	// Target is where the bind appears inside this container, e.g.
	// /media/library. Empty when no media bind could be found at all.
	Target string
	// Source is the host-side directory the bind comes from, e.g.
	// /mnt/remotes. Empty when it could not be determined confidently — see
	// MediaBind for when that happens. Empty must never be filled in with a
	// plausible-looking default; a wrong host path reads exactly like a right
	// one until the share is mounted and stays invisible.
	Source string
}

// Known reports whether both halves are known, which is the condition for
// generating a mount command an operator can paste without editing.
func (b Bind) Known() bool { return b.Target != "" && b.Source != "" }

// MediaBind finds the mount that carries footage into this container and
// recovers the host-side directory behind it from /proc/self/mountinfo's
// field 4 — the root of the mount within its source filesystem, which for a
// bind mount is the source directory.
//
// The honest limit of that trick: field 4 is a path *within a filesystem*, not
// within the host's namespace, so it equals the host path only when the source
// filesystem is itself mounted at / on the host. Bind a directory that lives
// on a separately mounted host filesystem (say /srv is its own filesystem and
// /srv/media is bound in) and field 4 reads "/media", not "/srv/media"; on an
// overlay upper layer or inside nested mount namespaces it can be misleading
// in other ways. That case is not detectable from in here, which is why the
// caller must present a discovered Source as something to check rather than as
// fact — see the container-media-bind note in mount.go.
//
// Two states are distinguished rather than collapsed. false means no media
// bind was found: nothing is known, and the caller must fall back to naming
// both paths generically. true with an empty Bind.Source means the bind was
// found but its host-side path was not recoverable (field 4 is "/", i.e. a
// whole filesystem was bound rather than a directory inside one) — the
// container-side Target is still fact, and still worth telling the operator.
func MediaBind(mountTable string) (Bind, bool) {
	for _, entry := range parseMountTable(mountTable) {
		if !isMediaBindTarget(entry.mountpoint) {
			continue
		}
		bind := Bind{Target: entry.mountpoint}
		// "/" means the mount's root is the whole source filesystem, so field
		// 4 says nothing about where that filesystem sits on the host. A
		// relative or empty value is not a path this code understands and is
		// treated the same way: unknown, never guessed.
		if strings.HasPrefix(entry.root, "/") && entry.root != "/" {
			bind.Source = strings.TrimSuffix(entry.root, "/")
		}
		return bind, true
	}
	return Bind{}, false
}

func isMediaBindTarget(mountpoint string) bool {
	for _, target := range mediaBindTargets {
		if mountpoint == target {
			return true
		}
	}
	return false
}

func underMountpoint(path, mountpoint string) bool {
	if mountpoint == "/" {
		return strings.HasPrefix(path, "/")
	}
	if path == mountpoint {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(mountpoint, "/")+"/")
}

func hasOption(options, want string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == want {
			return true
		}
	}
	return false
}

// unescapeMountField reverses the octal escaping the kernel applies to
// characters that would otherwise break the field separation. A share mounted
// under a path with a space in it is common enough on a NAS to matter.
func unescapeMountField(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			value := 0
			valid := true
			for _, digit := range field[i+1 : i+4] {
				if digit < '0' || digit > '7' {
					valid = false
					break
				}
				value = value*8 + int(digit-'0')
			}
			if valid {
				out.WriteByte(byte(value))
				i += 3
				continue
			}
		}
		out.WriteByte(field[i])
	}
	return out.String()
}

// LooksUnmounted reports whether a directory is most likely a mount point whose
// mount is missing. An empty directory is not proof, but it is the state an
// operator meets after a reboot when the share did not come back, and the
// alternative — scanning it and recording every asset as missing — is worse
// than saying so.
func LooksUnmounted(path, mountTable string) bool {
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) > 0 {
		return false
	}
	filesystem, known := FilesystemFor(path, mountTable)
	if !known {
		return false
	}
	// A mount that is present resolves to itself; an absent one resolves to
	// whatever filesystem holds the empty directory underneath.
	return filesystem.Mountpoint != strings.TrimSuffix(path, "/")
}
