// Package mount turns a network share into the commands that mount it, and
// recognises when a library root is already sitting on one.
//
// It deliberately mounts nothing. Mounting needs root, and surviving a reboot
// needs a line in /etc/fstab or a systemd unit — both of them state outside
// $NEXUSGATE_DATA_DIR, which is the one place this Hub is allowed to own. A
// tool that quietly acquires root to write files it cannot later account for is
// worse than one that prints an exact command and lets the operator run it.
//
// What that leaves is the part that is actually hard: knowing that a NAS share
// needs credentials in a 0600 file rather than on the command line, that the
// mount should be read-only because footage is never written, that a network
// root wants source_staging copy mode, and that under WSL2 a mount made in a
// shell is invisible to a service. None of that is discoverable from the error
// the operating system gives, which is "no such file or directory".
package mount

import (
	"fmt"
	"os"
	"path"
	"runtime"
	"strings"
)

type Protocol string

const (
	ProtocolSMB Protocol = "smb"
	ProtocolNFS Protocol = "nfs"
)

// Share is a network location a library root could live on.
type Share struct {
	Protocol Protocol
	Host     string
	// Name is the SMB share name or the NFS export path.
	Name string
	// User is only ever a username taken from smb://user@host/share. A password
	// is never parsed, carried or printed here: it belongs in a credentials file
	// the operator writes, so that it never reaches a process listing, a shell
	// history or this Hub's logs.
	User string
}

func (s Share) String() string {
	switch s.Protocol {
	case ProtocolNFS:
		return s.Host + ":" + s.Name
	default:
		return "//" + s.Host + "/" + s.Name
	}
}

// ParseShare recognises the forms an operator is likely to paste in place of a
// path. It is deliberately strict: a local path that is merely unusual must not
// be mistaken for a share, because the advice that follows would be wrong and
// the real error — a typo in a directory name — would be hidden by it.
func ParseShare(input string) (Share, bool) {
	value := strings.TrimSpace(input)
	if value == "" {
		return Share{}, false
	}
	// Windows writes its own UNC paths with backslashes, and that is how a
	// share is copied out of Explorer. Keep backslashes in URI userinfo: they
	// are the domain separator in DOMAIN\\user and are valid username data.
	if !strings.HasPrefix(strings.ToLower(value), "smb://") &&
		!strings.HasPrefix(strings.ToLower(value), "cifs://") &&
		!strings.HasPrefix(strings.ToLower(value), "nfs://") {
		value = strings.ReplaceAll(value, `\`, "/")
	}

	switch {
	case strings.HasPrefix(strings.ToLower(value), "smb://"):
		return parseHostAndName(value[len("smb://"):], ProtocolSMB)
	case strings.HasPrefix(strings.ToLower(value), "cifs://"):
		return parseHostAndName(value[len("cifs://"):], ProtocolSMB)
	case strings.HasPrefix(strings.ToLower(value), "nfs://"):
		share, ok := parseHostAndName(value[len("nfs://"):], ProtocolNFS)
		if ok {
			share.Name = "/" + share.Name
		}
		return share, ok
	case strings.HasPrefix(value, "//"):
		return parseHostAndName(value[2:], ProtocolSMB)
	}
	return parseNFSHostPath(value)
}

func parseHostAndName(rest string, protocol Protocol) (Share, bool) {
	user := ""
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		user, rest = rest[:at], rest[at+1:]
		// A pasted URL sometimes carries user:password@host, the form every
		// browser address bar accepts. The password half is discarded here,
		// never assigned to User, so that nothing downstream — a log line, the
		// inspect API, this page rendering it back in a browser — can ever
		// carry it forward. Share.User exists only to prefill a username in the
		// generated commands; the password always comes from a prompt or a
		// credentials file, never from this struct.
		if colon := strings.IndexByte(user, ':'); colon >= 0 {
			user = user[:colon]
		}
	}
	host, name, found := strings.Cut(strings.Trim(rest, "/"), "/")
	if !found || !validHost(host) || name == "" {
		return Share{}, false
	}
	// Share.User is copied into a shell here-document, a shell command line,
	// and YAML. Those three syntaxes have different escaping rules, so the
	// boundary is safest at the parser rather than as three separate output
	// escaping schemes. Keep the share classification when userinfo is dirty:
	// the host and share name still provide useful, correct network-share
	// guidance, and the existing placeholder path can ask for the username.
	if !validUser(user) {
		user = ""
	}
	return Share{Protocol: protocol, Host: host, Name: strings.Trim(name, "/"), User: user}, true
}

// parseNFSHostPath handles the bare host:/export form. The guard against a
// Windows drive letter is why the host has to be more than one character: "C:/"
// is a path, not an export.
func parseNFSHostPath(value string) (Share, bool) {
	host, export, found := strings.Cut(value, ":")
	if !found || len(host) < 2 || !validHost(host) || !strings.HasPrefix(export, "/") {
		return Share{}, false
	}
	return Share{Protocol: ProtocolNFS, Host: host, Name: export}, true
}

func validHost(host string) bool {
	if host == "" || strings.HasPrefix(host, "-") {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// validUser is intentionally narrower than the username grammar any one SMB
// implementation may accept. Internal spaces are not included: the available
// SMB documentation does not establish a portable username rule for them, and
// rejecting them avoids relying on three consumers' different escaping rules.
func validUser(user string) bool {
	if user == "" {
		return false
	}
	for _, r := range user {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == '\\' || r == '@':
		default:
			return false
		}
	}
	return true
}

// Host describes the machine the mount will be made on. It is a value rather
// than a set of runtime lookups so the advice can be generated for a machine
// that is not this one, and so it can be tested without one.
type Host struct {
	OS string
	// WSL changes the advice materially rather than cosmetically: a mount made
	// in an interactive shell lands in that shell's mount namespace and is
	// simply absent from a service started by systemd or over SSH.
	WSL bool
	// Container is true when this process is itself the thing running inside
	// a Docker/Podman container — the primary distribution form for the Hub.
	// It changes the advice for the same reason WSL does: the machine that
	// can run `mount` is not the machine this process sees as "here". Unlike
	// WSL, there is no nsenter escape hatch, because the container has
	// neither CAP_SYS_ADMIN nor root — see the container-cannot-mount note in
	// notes() for why that is deliberate rather than a gap to work around.
	Container bool
	// Service is true when the Hub runs under a service manager rather than in
	// the operator's own login session. It changes the answer rather than the
	// wording: a drive letter mapped by `net use` and a volume mounted by
	// Finder both belong to the login session that created them, and a service
	// running as another account simply does not have them. False is the
	// honest default — it yields the interactive answer, which is merely
	// redundant for a service, where the reverse would be advice that cannot
	// work.
	Service bool
	// MediaBind is the bind mount footage arrives through when Container is
	// true, discovered by MediaBind() rather than assumed. It is carried on
	// Host — a value — rather than looked up inside Guidance so that this
	// package still reads nothing of its own while generating advice, the
	// same reason FilesystemFor takes the mount table as text.
	//
	// Its zero value means "not discovered", which is a state the generated
	// guidance has to handle rather than paper over: the two paths below are
	// then named with the HOST_MEDIA_ROOT / PATH_INSIDE_THE_HUB_CONTAINER
	// placeholders and an extra step tells the operator how to find the real
	// ones. A guessed path would be indistinguishable from a correct one
	// right up until the share is mounted and stays invisible to the Hub.
	MediaBind Bind
	UID       int
	GID       int
}

// The two placeholders a containerised guide falls back to when the media bind
// could not be discovered. They are deliberately in <angle brackets>: a shell
// refuses to run a command containing one, where an invented-but-plausible
// path like /mnt/remotes would run and silently mount the share somewhere the
// Hub cannot see.
const (
	hostMediaRootPlaceholder = "<HOST_MEDIA_ROOT>"
	containerPathPlaceholder = "<PATH_INSIDE_THE_HUB_CONTAINER>"
)

// hostMediaRoot is the host-side directory a containerised Hub's footage must
// be mounted under, or the placeholder when it is not known.
func (h Host) hostMediaRoot() string {
	if h.MediaBind.Source != "" {
		return h.MediaBind.Source
	}
	return hostMediaRootPlaceholder
}

func LocalHost() Host {
	host := Host{OS: runtime.GOOS, UID: os.Getuid(), GID: os.Getgid()}
	if runtime.GOOS == "linux" {
		if release, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			host.WSL = strings.Contains(strings.ToLower(string(release)), "microsoft")
		}
	}
	// systemd sets INVOCATION_ID for every unit it starts and for nothing else,
	// which makes it the one service signal this package can prove rather than
	// infer. There is no equivalent it can read for a Windows service or a
	// macOS LaunchDaemon from inside a plain Go process, so those stay false:
	// an interactive answer given to a service is redundant, while a service
	// answer given to an interactive operator withholds the drive-letter step
	// they can actually use.
	_, host.Service = os.LookupEnv("INVOCATION_ID")
	host.Container = runningInContainer()
	if host.Container {
		// Only meaningful inside a container, and only ever read here: a
		// media bind on a bare-metal host is just a directory.
		host.MediaBind, _ = MediaBind(ReadMountTable())
	}
	return host
}

// runningInContainer checks the two markers Docker and Podman each leave
// behind for exactly this question. Both checks are cheap stats and neither
// has an error path worth surfacing: a container that lacks both files still
// gets ordinary host advice, which is wrong but not unsafe, whereas failing
// LocalHost() over it would take down every caller of mount.Guidance.
func runningInContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// shareBaseName is the last path element of a share, the name a mount point is
// built from. Shared by DefaultMountpoint and the compose volume's container
// path so the same share never produces two different directory names.
func shareBaseName(share Share) string {
	name := path.Base(strings.Trim(share.Name, "/"))
	if name == "" || name == "." || name == "/" {
		name = share.Host
	}
	return name
}

// DefaultMountpoint is the *host-side* path a share is mounted at when the
// operator did not say. On a bare-metal host it is under a directory of our
// own rather than directly in /mnt, so that a second library does not have to
// fight the first for a name.
//
// A containerised Hub cannot use that answer. Its footage arrives through a
// bind mount of a host directory, and mount propagation only carries mounts
// made *under* that bind's source — a share mounted at /mnt/nexusgate/<name>
// on the host is outside the bind entirely and stays invisible inside the
// container no matter which propagation mode the bind has. So the default
// moves under the bind's source, and ContainerPath below translates it into
// the path the Hub will actually be able to open.
func DefaultMountpoint(share Share, host Host) string {
	name := shareBaseName(share)
	// Checked before the darwin branch because host.OS here is the
	// *container's* OS (always linux), not the Docker host's, so the /Volumes
	// answer would be wrong twice over.
	if host.Container {
		return path.Join(host.hostMediaRoot(), name)
	}
	if host.OS == "darwin" {
		return "/Volumes/" + name
	}
	// Windows has no /mnt, and the drive letter the steps used to map belongs
	// to one login session. Returning the UNC path makes the mount point the
	// wizard verifies and the path addRootStep records the same string the
	// operator was told to open — they used to disagree outright, mapping Z:
	// and then recording /mnt/nexusgate/<share>, a path that cannot exist on
	// the machine the instructions were written for.
	if host.OS == "windows" {
		return windowsUNC(share)
	}
	return "/mnt/nexusgate/" + name
}

// windowsUNC renders the share the way Explorer and a Windows service both
// accept it. Unlike a mapped drive it names no session state, which is why it
// is both the default mount point above and the first step windowsSteps gives.
func windowsUNC(share Share) string {
	return `\\` + share.Host + `\` + strings.ReplaceAll(share.Name, "/", `\`)
}

// ContainerPath translates a host-side mount point into the path the same
// directory appears at inside this container, which is the only path the Hub
// can stat, record as a library root, or hand to FFmpeg. It reports false
// rather than guessing whenever it cannot state the answer as fact: when the
// Hub is not containerised, when no media bind was discovered, and when the
// given path is not under the bind's source — that last case is not a
// translation failure but a mount that will never be visible in here at all.
//
// The host mount point may still carry the HOST_MEDIA_ROOT placeholder. That
// translates fine and the result is not a guess: whatever the bind's source
// turns out to be, a share mounted directly under it appears under the bind's
// target with the same relative path.
func ContainerPath(hostPath string, host Host) (string, bool) {
	if !host.Container || host.MediaBind.Target == "" {
		return "", false
	}
	relative, ok := relativeToDirectory(strings.TrimSpace(hostPath), host.hostMediaRoot())
	if !ok {
		return "", false
	}
	return path.Join(host.MediaBind.Target, relative), true
}

// relativeToDirectory returns the part of value below directory, and reports
// false when value is not below (or equal to) it.
func relativeToDirectory(value, directory string) (string, bool) {
	directory = strings.TrimSuffix(directory, "/")
	if directory == "" {
		return "", false
	}
	if value == directory {
		return "", true
	}
	if strings.HasPrefix(value, directory+"/") {
		return strings.TrimPrefix(value, directory+"/"), true
	}
	return "", false
}

// Step is one numbered instruction in a Guide.
//
// Key is a stable, machine-readable identifier for this step — not shown to
// the operator directly, but used by the browser wizard
// (internal/api/library_roots_page.go) to look up a Chinese translation of
// Title, because this package's own text must stay English (it is also the
// CLI's doctor/root-warnings output) while the product's UI copy must be
// Chinese. Renaming a key here silently drops that step's translation back
// to English rather than failing a build: an untranslated string reaching
// the page is deliberately the failure mode, not a panic or a build break.
// TestGuidanceStepsAndNotesCarryStableKeys in mount_test.go and
// TestLibraryRootsPageTranslatesEveryGuidanceKey in
// internal/api/library_roots_page_test.go exist to catch a key that was
// renamed, or added, without the translation table being updated to match.
type Step struct {
	Key   string
	Title string
	// Kind is empty or "command" for a line to run in a shell, and "file-line"
	// for a line to append to the file named by File. Keeping that distinction
	// here prevents an fstab entry from being mistaken for an executable command.
	Kind     string
	File     string
	Commands []string
}

// Note is a single item of reasoning-bearing advice attached to a Guide —
// why the mount is read-only, why a network root wants copy-mode staging,
// why WSL needs the nsenter prefix. Key carries the same translation
// contract as Step.Key.
//
// This is a struct with a Key field rather than a parallel []string of
// keys alongside Notes, because a parallel slice can silently drift out of
// index alignment (append to one, forget the other) with no compiler
// error; carrying Key next to Text on one value makes that impossible.
type Note struct {
	Key  string
	Text string
}

type Guide struct {
	Summary string
	Steps   []Step
	Notes   []Note
}

// Guidance produces the instructions that mount share at mountpoint. Values are
// filled in from the host rather than left as shell substitutions, because a
// line that has to be edited before it runs or is saved is one more thing to get
// wrong.
func Guidance(share Share, mountpoint string, host Host) Guide {
	if mountpoint == "" {
		mountpoint = DefaultMountpoint(share, host)
	}
	guide := Guide{Summary: fmt.Sprintf("%s is a network share, not a local path. Mount it, then add the mount point.", share)}
	switch host.OS {
	case "windows":
		guide.Steps = windowsSteps(share, mountpoint, host)
	case "darwin":
		guide.Steps = darwinSteps(share, mountpoint)
	default:
		guide.Steps = linuxSteps(share, mountpoint, host)
	}
	runOnHost := Step{
		Key:   "run-on-hub-host",
		Title: "These commands run on the machine running NexusGate, not on the computer you are reading this page on. If you are sitting at that machine, open a terminal; otherwise SSH into it first.",
	}
	if host.Container {
		// A container has a separate mount namespace and no authority to mount;
		// keep this boundary explicit before any command that would otherwise
		// silently fail inside it.
		runOnHost = Step{
			Key:   "container-run-on-host",
			Title: "Run the following on the machine hosting the Docker daemon — not inside this container",
		}
	}
	guide.Steps = append([]Step{runOnHost}, guide.Steps...)
	guide.Steps = append(guide.Steps, addRootStep(mountpoint, host))
	guide.Notes = notes(share, host)
	return guide
}

// addRootStep is the one step that does not run where the others do. Every
// command above it is executed by whoever owns the mount namespace — on a
// containerised Hub, the Docker host. `root add` is executed by the Hub
// itself, so on a containerised Hub it must be given the path *inside* the
// container: the host-side mount point it was told to create is not a path
// this process can stat, record, or hand to FFmpeg.
//
// When the translation cannot be stated as fact, this emits no command at
// all. A `root add` against a guessed path is worse than none: it either
// fails immediately, or succeeds against an empty directory Docker created
// on demand and registers a library root that will never contain anything.
func addRootStep(mountpoint string, host Host) Step {
	if !host.Container {
		return Step{
			Key:      "add-root",
			Title:    "Add the mount point as a library root",
			Commands: []string{"nexusgate root add " + mountpoint},
		}
	}
	containerPath, ok := ContainerPath(mountpoint, host)
	if !ok {
		return Step{
			Key: "add-root-not-visible",
			Title: fmt.Sprintf(
				"This mount point is outside the directory bound into this container (%s), so the Hub will never see it. Mount the share somewhere under %s instead, then re-run this wizard — or rebuild the container with a bind that covers it.",
				host.MediaBind.Target, host.hostMediaRoot()),
		}
	}
	return Step{
		Key: "add-root-in-container",
		Title: fmt.Sprintf(
			"Back inside the Hub container — %s on the host is %s in here, and that is the path to record",
			mountpoint, containerPath),
		Commands: []string{"nexusgate root add " + containerPath},
	}
}

func credentialsPath(share Share) string {
	return "/etc/nexusgate/" + share.Host + ".cred"
}

func linuxSteps(share Share, mountpoint string, host Host) []Step {
	prefix := ""
	if host.WSL {
		// Mounting into PID 1's namespace is what makes the mount visible to
		// everything else on the machine. Without it the mount exists only for
		// the shell that made it, and the Hub — started by systemd, or over a
		// separate SSH connection — sees an empty directory.
		prefix = "nsenter -t 1 -m -- "
	}
	if share.Protocol == ProtocolNFS {
		return []Step{
			{Key: "install-nfs-client", Title: "Install the NFS client package the first time you need it (use the one for your distribution)", Commands: []string{"sudo apt-get install -y nfs-common", "sudo dnf install -y nfs-utils"}},
			{Key: "create-mountpoint", Title: "Create the mount point", Commands: []string{"sudo mkdir -p " + mountpoint}},
			{
				Key:      "nfs-mount",
				Title:    "Mount the export read-only",
				Commands: []string{fmt.Sprintf("sudo %smount -t nfs %s %s -o ro,_netdev", prefix, share, mountpoint)},
			},
			{
				Key:      "fstab",
				Title:    "Make it survive a reboot — append to /etc/fstab",
				Kind:     "file-line",
				File:     "/etc/fstab",
				Commands: []string{fmt.Sprintf("%s %s nfs ro,_netdev,nofail 0 0", share, mountpoint)},
			},
		}
	}
	credentials := credentialsPath(share)
	user := share.User
	if user == "" {
		user = "YOUR_NAS_USERNAME"
	}
	options := fmt.Sprintf("credentials=%s,ro,uid=%d,gid=%d,iocharset=utf8,_netdev", credentials, host.UID, host.GID)
	return []Step{
		{Key: "install-smb-client", Title: "Install the SMB client package the first time you need it (use the one for your distribution)", Commands: []string{"sudo apt-get install -y cifs-utils", "sudo dnf install -y cifs-utils"}},
		{
			// The password goes into a root-only file rather than into the mount
			// command, where it would be readable by every user through the
			// process list, and rather than into /etc/fstab, which is world
			// readable by design.
			Key:   "smb-credentials-file",
			Title: "Put the credentials in a file only root can read — replace YOUR_NAS_PASSWORD with your NAS password",
			Commands: []string{
				"sudo install -d -m 700 /etc/nexusgate",
				fmt.Sprintf("sudo tee %s >/dev/null <<'NEXUSGATE_CREDS'\nusername=%s\npassword=YOUR_NAS_PASSWORD\nNEXUSGATE_CREDS", credentials, user),
				"sudo chmod 600 " + credentials,
			},
		},
		{Key: "create-mountpoint", Title: "Create the mount point", Commands: []string{"sudo mkdir -p " + mountpoint}},
		{
			Key:      "smb-mount",
			Title:    "Mount the share read-only",
			Commands: []string{fmt.Sprintf("sudo %smount -t cifs %s %s -o %s", prefix, share, mountpoint, options)},
		},
		{
			Key:      "fstab",
			Title:    "Make it survive a reboot — append to /etc/fstab",
			Kind:     "file-line",
			File:     "/etc/fstab",
			Commands: []string{fmt.Sprintf("%s %s cifs %s,nofail 0 0", share, mountpoint, options)},
		},
	}
}

func darwinSteps(share Share, mountpoint string) []Step {
	user := share.User
	if user == "" {
		user = "YOUR_NAS_USERNAME"
	}
	if share.Protocol == ProtocolNFS {
		return []Step{
			{Key: "create-mountpoint", Title: "Create the mount point", Commands: []string{"sudo mkdir -p " + mountpoint}},
			{Key: "nfs-mount", Title: "Mount the export read-only", Commands: []string{fmt.Sprintf("sudo mount -t nfs -o ro %s %s", share, mountpoint)}},
		}
	}
	return []Step{
		{Key: "create-mountpoint", Title: "Create the mount point", Commands: []string{"sudo mkdir -p " + mountpoint}},
		{
			// mount_smbfs asks for the password on a terminal when it is left out
			// of the URL, which keeps it out of the shell history.
			Key:      "darwin-mount",
			Title:    "Mount the share (the password is prompted for, not typed here)",
			Commands: []string{fmt.Sprintf("mount_smbfs -o ro //%s@%s/%s %s", user, share.Host, share.Name, mountpoint)},
		},
	}
}

func windowsSteps(share Share, mountpoint string, host Host) []Step {
	unc := windowsUNC(share)
	steps := []Step{
		{
			// This is the Explorer path, and it leads because it is the one
			// that works for both a desktop Hub and a service; a drive letter
			// is a convenience layered on top of it, not the way in. The UNC
			// address is a command rather than part of the title because the
			// title is translated by Key through a lookup that substitutes
			// nothing — a path interpolated into it survives only in English.
			// Carrying it here also gives it the copy button, which is what
			// the operator wants: this string is pasted, not typed.
			Key:      "windows-explorer-unc",
			Title:    "Paste this into File Explorer's address bar and sign in, so Windows holds the credentials for this share",
			Commands: []string{unc},
		},
	}
	if host.Service {
		// A service does not inherit the operator's mapped drives, and the
		// existing windows-service-drive-letter note said so while the steps
		// went on recommending one anyway. Offering no drive letter here is
		// the point: there is nothing for the operator to try that could work.
		return steps
	}
	steps = append(steps, Step{
		Key:      "windows-map",
		Title:    "Optional, and only while the Hub runs as you rather than as a service: map the share to a drive letter",
		Commands: []string{fmt.Sprintf(`net use Z: %s /persistent:yes`, unc)},
	})
	return steps
}

func notes(share Share, host Host) []Note {
	notes := []Note{
		{Key: "read-only", Text: "The mount is read-only on purpose. Footage is never written to, and a read-only mount makes that true of the whole share rather than only of this program."},
		{Key: "staging-copy", Text: `Set "source_staging": {"mode": "copy"} in config.json for a network root. FFmpeg reading a 4K source over SMB for every derive is what makes a NAS library crawl; copy mode stages the file into cache/sources/ once instead.`},
		{Key: "worker-same-path", Text: "A Worker needs the same footage at the same path, or an explicit --mount root-id=/its/own/path when it enrols."},
	}
	if host.Container {
		notes = append(notes, Note{
			Key:  "container-cannot-mount",
			Text: "This container cannot mount the share for itself, and that is deliberate: it runs as a fixed unprivileged user with no CAP_SYS_ADMIN, and either that capability or a bind-mounted docker.sock would let it acquire the host mount authority it is specifically denied — docker.sock is the worse of the two, since it is unrestricted root on the Docker host, not merely on this container. That is why the steps above run on the Docker host instead of in here. If the share is mounted on the host after this container already started, it stays invisible inside the container unless the media bind carries \"bind.propagation: rslave\" in docker-compose.yml; without that, recreate the container (docker compose up -d) once the host mount exists.",
		})
	}
	if host.WSL {
		notes = append(notes,
			Note{Key: "wsl-namespace", Text: "Under WSL2 a mount made in an interactive shell is invisible to systemd services and to other SSH sessions, because they are in different mount namespaces. The nsenter prefix above mounts into PID 1's namespace, where everything can see it."},
			Note{Key: "wsl-not-persistent", Text: "WSL2 does not persist mounts across a restart of the distribution. Either re-run the mount command, or add it to the boot command in /etc/wsl.conf."})
	}
	if host.OS == "windows" {
		notes = append(notes,
			Note{Key: "windows-service-drive-letter", Text: `A drive letter mapped in your own session does not exist for a service running as another account. If the Hub runs as a service, give it the UNC path (\\host\share\folder) instead of the letter.`})
		if host.Service {
			// Reaching the share by UNC removes the session-scoped drive
			// letter, not the authentication: the account the service logs on
			// as is the one the NAS sees, and LocalSystem or a local account
			// with no NAS rights fails here while the operator's own Explorer
			// window opens the same path without complaint.
			notes = append(notes,
				Note{Key: "windows-service-account", Text: `The UNC path removes the drive letter's session problem but not the credential one: the service's own logon account is what the NAS authenticates. LocalSystem and a local account without rights on the NAS will fail here even though your Explorer window opens the same path.`})
		}
	}
	if host.OS == "darwin" && host.Service {
		// Finder's "Connect to Server" is the obvious macOS answer and it is
		// the wrong one here for the same reason net use is on Windows, which
		// is worth saying explicitly: the operator can watch the volume appear
		// in Finder and still have the Hub see nothing.
		notes = append(notes,
			Note{Key: "darwin-session-scope", Text: `A share connected through Finder is mounted for your login session, so /Volumes/<share> can be absent for a LaunchDaemon that started before you logged in, and can disappear when you log out. Make the mount from the service's own account — the mount_smbfs step above does that — rather than from Finder.`})
	}
	return notes
}

// Lines renders the guide for a terminal.
func (g Guide) Lines() []string {
	lines := []string{g.Summary, ""}
	for i, step := range g.Steps {
		lines = append(lines, fmt.Sprintf("  %d. %s", i+1, step.Title), "")
		for _, command := range step.Commands {
			lines = append(lines, "     "+command)
		}
		lines = append(lines, "")
	}
	for _, note := range g.Notes {
		lines = append(lines, "  note: "+note.Text)
	}
	return lines
}
