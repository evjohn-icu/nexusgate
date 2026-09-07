package mount

import (
	"fmt"
	"strings"
)

// VolumeDefinition is a docker-compose named-volume entry for a network
// share, generated so an operator running the Hub as the Docker image does
// not have to hand-translate a NAS address into driver_opts syntax — the two
// forms below differ in ways that are easy to get wrong (see ComposeVolume).
type VolumeDefinition struct {
	Name string
	// YAML is the entry only — "<name>:" and everything nested under it — not
	// the enclosing "volumes:" top-level key, so a caller can splice several
	// of these under one volumes: block alongside plain named volumes like
	// the ones already in docker-compose.yml.
	YAML string
	// Warning is non-empty exactly when this form has a security caveat the
	// caller must surface, not merely a usage note. Kept as a field rather
	// than a log line here because internal/mount does no I/O of its own —
	// see the package doc — and a warning nobody reads is worse than one
	// returned all the way to whatever is rendering this for the operator.
	Warning string
	// WarningKey is a stable, machine-readable identifier for Warning, empty
	// exactly when Warning is empty. It carries the same translation contract
	// as Step.Key and Note.Key in mount.go: the browser wizard
	// (internal/api/library_roots_page.go) looks up a Chinese rendering of
	// Warning by this key and falls back to the English text when the key is
	// unrecognised, because this package's own text must stay English — it
	// is also the CLI's doctor/root-warnings output.
	WarningKey string
	// ServiceYAML is the other half, and shipping the volume entry without it
	// was a real gap: a named volume that no service mounts is inert, so an
	// operator given only the definition above has to invent the service-side
	// line, the container path, and then discover on their own that the path
	// is what `root add` wants. Both services appear in it because a Worker
	// must reach the same footage as the Hub — a volume mounted only into the
	// Hub turns every leased derive job into a missing-file failure.
	ServiceYAML string
	// MountPath is where ServiceYAML mounts the volume inside the container,
	// and therefore the path to record as a library root. It is deliberately
	// not under /media/library: that is the bind mount's target, and nesting a
	// volume inside it would leave two mount sources fighting over one subtree
	// for no benefit — this volume does not travel through the bind at all.
	MountPath string
}

// ComposeVolume renders share as a Docker Compose named volume called name,
// using the built-in "local" driver's NFS or CIFS support. It reports false
// for anything that is not a network share: a local directory needs a bind
// mount, which has no driver_opts and nothing here to generate. It also
// reports false for a share whose name cannot be interpolated into the
// generated YAML — see CommandSafeShare — which is a different fact about the
// input and is why the caller cannot read a false here as "not a share".
//
// The two protocols are not equally good options and the difference is not
// cosmetic. Docker's local driver forwards driver_opts straight to the
// mount(2) syscall — it never shells out to mount.cifs — so the SMB form
// cannot use a credentials file the way linuxSteps' host mount can
// (credentials=/path/to/file is rejected with "invalid argument": see
// docker/cli#2802, open since 2020-10-20). The password has to be inline in
// o:, where `docker volume inspect` and
// /var/lib/docker/volumes/<name>/opts.json both keep it in cleartext. NFS
// has no such trade — it carries no credentials at all — which is why it is
// the path this function's doc, and Warning, steer an operator toward.
func ComposeVolume(share Share, name string) (VolumeDefinition, bool) {
	// Two reasons to report false, and they are different facts about the
	// input. A protocol this driver has no form for is the one below. This one
	// is a share whose own name cannot be interpolated into the device: scalar
	// — ParseShare now classifies such a share instead of refusing it, so it
	// reaches here. Neither case gets a partial stanza: a compose file with a
	// mangled device: is worse than no compose file, because it mounts, and
	// mounts something other than what was asked for.
	if !CommandSafeShare(share) {
		return VolumeDefinition{}, false
	}
	switch share.Protocol {
	case ProtocolNFS:
		return composeNFSVolume(share, name), true
	case ProtocolSMB:
		return composeSMBVolume(share, name), true
	default:
		return VolumeDefinition{}, false
	}
}

func composeNFSVolume(share Share, name string) VolumeDefinition {
	yaml := fmt.Sprintf(`  %s:
    driver: local
    driver_opts:
      type: "nfs"
      o: "addr=%s,ro,soft,nolock"
      device: ":%s"
`, name, share.Host, share.Name)
	mountPath := volumeMountPath(name)
	return VolumeDefinition{Name: name, YAML: yaml, MountPath: mountPath, ServiceYAML: serviceYAML(name, mountPath)}
}

// volumeMountPath is where a volume-mounted share lands inside the container.
func volumeMountPath(name string) string {
	return "/media/" + name
}

// serviceYAML mounts the volume into both services. The Worker is not
// optional here: it leases derive jobs that open the source file itself, so a
// volume the Hub can see and the Worker cannot turns every leased job into a
// missing-file failure rather than a fallback to local processing. :ro
// restates at the service level what the driver_opts already say, because
// this is the line an operator is most likely to copy into an existing
// compose file by hand, and read-only originals is the property this project
// will not silently lose.
func serviceYAML(name, mountPath string) string {
	return fmt.Sprintf(`services:
  hub:
    volumes:
      - %s:%s:ro
  worker:
    volumes:
      - %s:%s:ro
`, name, mountPath, name, mountPath)
}

func composeSMBVolume(share Share, name string) VolumeDefinition {
	user := share.User
	if user == "" {
		// Share never carries a password — see Share.User's doc in mount.go
		// — so this placeholder is not a fallback for a missing one, it is
		// the only value this form can ever have. Matches linuxSteps' own
		// YOUR_NAS_PASSWORD placeholder so the two forms read as one family.
		user = "YOUR_NAS_USERNAME"
	}
	options := fmt.Sprintf("username=%s,password=%s,ro,uid=10001,gid=10001,vers=3.0,iocharset=utf8", user, "YOUR_NAS_PASSWORD")
	yaml := fmt.Sprintf(`  %s:
    driver: local
    driver_opts:
      type: "cifs"
      o: "%s"
      device: "//%s/%s"
`, name, options, share.Host, share.Name)
	mountPath := volumeMountPath(name)
	warning := "Docker's local volume driver sends driver_opts straight to the mount(2) syscall and never runs the mount.cifs helper, so the credentials=/path/to/file form used elsewhere in this guide is rejected with \"invalid argument\" (docker/cli#2802, open since 2020-10-20). The password has to be inline in the o: parameter instead, which means it sits in cleartext in `docker volume inspect` output and in /var/lib/docker/volumes/" + name + "/opts.json. Every other secret in this project stays in an encrypted store that never reaches SQLite, an API response, or a log — this form is a documented fallback for when NFS is not available from the NAS, not the default. Note also the surface an operator is most likely to leak through: their own docker-compose.yml, which is a file people commit to git. Taking the password from an untracked .env as ${NAS_PASSWORD} keeps it out of the tracked file, but does not fix opts.json — Docker stores the resolved value, not the reference."
	return VolumeDefinition{Name: name, YAML: yaml, Warning: warning, WarningKey: "compose-smb-cleartext", MountPath: mountPath, ServiceYAML: serviceYAML(name, mountPath)}
}

// VolumeName derives a Docker Compose volume name for share deterministically
// from its host and share name. Docker volume names are restricted to
// [a-zA-Z0-9][a-zA-Z0-9_.-]*, but share.Host and share.Name are not — Name in
// particular is an NFS export path or SMB share name and can carry "/", "."
// runs, spaces or arbitrary Unicode pasted from a NAS admin page — so this
// cannot simply concatenate them into the YAML.
func VolumeName(share Share) string {
	return sanitizeVolumeName("nexusgate-" + share.Host + "-" + share.Name)
}

// sanitizeVolumeName rewrites s into the character set Docker accepts for a
// volume name, guaranteeing a non-empty result that starts with an
// alphanumeric character. It is deliberately generic — not aware of Share —
// so a future caller with a different naming scheme does not have to
// reimplement the escaping rules.
func sanitizeVolumeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "_.-")
	if name == "" {
		// Every character in s was outside the allowed set — an export path
		// that is pure Unicode, say. "v" is not informative, but it is valid,
		// and the alternative (returning "") would produce a docker-compose
		// stanza with no name at all.
		return "v"
	}
	return name
}
