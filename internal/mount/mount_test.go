package mount

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The operator pastes what they have: a UNC path out of Explorer, an smb:// URL
// out of Finder, an export out of the NAS admin page. Every one of those is
// currently answered with "no such file or directory", which is true and
// useless.
func TestParseShareRecognisesWhatOperatorsActuallyPaste(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  Share
	}{
		{`//192.0.2.10/Video`, Share{Protocol: ProtocolSMB, Host: "192.0.2.10", Name: "Video"}},
		{`\\nas\Footage`, Share{Protocol: ProtocolSMB, Host: "nas", Name: "Footage"}},
		{`smb://nas.local/Video`, Share{Protocol: ProtocolSMB, Host: "nas.local", Name: "Video"}},
		{`smb://ev@nas.local/Video`, Share{Protocol: ProtocolSMB, Host: "nas.local", Name: "Video", User: "ev"}},
		{`//nas/Video/2024`, Share{Protocol: ProtocolSMB, Host: "nas", Name: "Video/2024"}},
		{`192.0.2.10:/volume1/Video`, Share{Protocol: ProtocolNFS, Host: "192.0.2.10", Name: "/volume1/Video"}},
	} {
		got, ok := ParseShare(tc.input)
		if !ok {
			t.Errorf("%s: not recognised as a share", tc.input)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.input, got, tc.want)
		}
	}
}

// A browser address bar accepts smb://user:password@host/share, so an
// operator copying one out is not implausible. If ParseShare kept the
// password half in Share.User, every consumer of Share — the generated mount
// commands, and later the /api/v1/roots/inspect JSON response — would carry
// it forward and eventually render it in a browser. The username is kept;
// the password is not.
func TestParseShareDropsPasswordFromUserinfo(t *testing.T) {
	got, ok := ParseShare("smb://ev:hunter2@nas.local/Video")
	if !ok {
		t.Fatal("not recognised as a share")
	}
	if got.User != "ev" {
		t.Fatalf("User = %q, want %q (password must be dropped, not just the rest of the struct)", got.User, "ev")
	}
	if strings.Contains(got.String(), "hunter2") || strings.Contains(fmt.Sprintf("%+v", got), "hunter2") {
		t.Fatalf("password leaked into Share: %+v", got)
	}
}

func TestParseShareRejectsUserThatCouldBreakOutOfGeneratedSyntax(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"newline", "smb://evil\n@nas/Video"},
		{"here-doc delimiter collision", "smb://evil\nNEXUSGATE_CREDS\nid > /tmp/pwned\n@nas/Video"},
		{"single quote", `smb://evil'user@nas/Video`},
		{"double quote", "smb://evil\"user@nas/Video"},
		{"backtick", "smb://evil`user@nas/Video"},
		{"command substitution", `smb://evil$(id)@nas/Video`},
		{"comma", `smb://evil,user@nas/Video`},
		{"semicolon", `smb://evil;user@nas/Video`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			share, ok := ParseShare(tc.input)
			if !ok {
				t.Fatalf("ParseShare(%q) rejected a network share with dirty userinfo", tc.input)
			}
			if share.Host != "nas" || share.Name != "Video" {
				t.Fatalf("ParseShare(%q) = %+v, want host nas and share Video preserved", tc.input, share)
			}
			if share.User != "" {
				t.Fatalf("ParseShare(%q).User = %q, want dirty userinfo discarded", tc.input, share.User)
			}
		})
	}
}

func TestParseShareAcceptsSafeSMBUsernames(t *testing.T) {
	for _, tc := range []struct {
		input string
		user  string
	}{
		{`smb://DOMAIN\alice@nas/Video`, `DOMAIN\alice`},
		{`smb://alice@nas/Video`, "alice"},
		{`smb://alice.smith@nas/Video`, "alice.smith"},
		{`smb://alice-01@nas/Video`, "alice-01"},
		{`smb://alice@domain@nas/Video`, "alice@domain"},
	} {
		share, ok := ParseShare(tc.input)
		if !ok {
			t.Fatalf("ParseShare(%q) rejected a safe SMB username", tc.input)
		}
		if share.User != tc.user {
			t.Errorf("ParseShare(%q).User = %q, want %q", tc.input, share.User, tc.user)
		}
	}
}

func TestGuidanceGeneratedCommandsSurviveHostileShareInput(t *testing.T) {
	const input = "smb://evil\nNEXUSGATE_CREDS\nid > /tmp/pwned\n@nas/Video"
	share, ok := ParseShare(input)
	if !ok {
		t.Fatal("hostile userinfo must not make the network share unrecognisable")
	}

	checkCommands := func(t *testing.T, guide Guide) {
		t.Helper()
		for _, step := range guide.Steps {
			for _, command := range step.Commands {
				lines := strings.Split(command, "\n")
				delimiterLines := 0
				for i, line := range lines {
					if line == "NEXUSGATE_CREDS" {
						delimiterLines++
						if i != len(lines)-1 {
							t.Errorf("step %q has a here-doc delimiter before the command's final line: %q", step.Key, command)
						}
					}
				}
				if delimiterLines > 0 &&
					(!strings.Contains(command, "<<'NEXUSGATE_CREDS'") || delimiterLines != 1) {
					t.Errorf("step %q has injected here-doc delimiter lines: %q", step.Key, command)
				}
				for _, payload := range []string{"evil", "id > /tmp/pwned", "$(id)"} {
					if strings.Contains(command, payload) {
						t.Errorf("step %q contains hostile userinfo %q: %q", step.Key, payload, command)
					}
				}
			}
		}
	}

	linux := Guidance(share, "/mnt/nexusgate/Video", Host{OS: "linux", UID: 1000, GID: 1000})
	checkCommands(t, linux)

	definition, ok := ComposeVolume(share, "nas-video")
	if !ok {
		t.Fatal("hostile userinfo must not prevent compose guidance")
	}
	for _, payload := range []string{"evil", "id > /tmp/pwned", "$(id)"} {
		if strings.Contains(definition.YAML, payload) {
			t.Errorf("compose YAML contains hostile userinfo %q: %q", payload, definition.YAML)
		}
	}

	darwin := Guidance(share, "/Volumes/Video", Host{OS: "darwin"})
	checkCommands(t, darwin)
	foundMountCommand := false
	for _, step := range darwin.Steps {
		for _, command := range step.Commands {
			if strings.Contains(command, "mount_smbfs") {
				foundMountCommand = true
				if strings.Contains(command, "\n") {
					t.Errorf("mount_smbfs command contains an injected newline: %q", command)
				}
			}
		}
	}
	if !foundMountCommand {
		t.Fatal("darwin guidance did not produce a mount_smbfs command")
	}
}

// Mistaking a local path for a share is the expensive error: the advice that
// follows would be irrelevant and it would bury the real problem, which is
// usually a typo in a directory name.
func TestParseShareLeavesLocalPathsAlone(t *testing.T) {
	for _, input := range []string{
		"/mnt/footage",
		"/Users/example/Movies",
		`C:\Footage`,
		"C:/Footage",
		"./relative",
		"",
		"/",
		"//",
	} {
		if share, ok := ParseShare(input); ok {
			t.Errorf("%q was read as the share %+v", input, share)
		}
	}
}

// A password on the mount command line is readable by every user on the machine
// through the process list, and a password in /etc/fstab is world readable by
// design. Neither may ever appear in generated advice.
func TestSMBGuidanceKeepsThePasswordOutOfCommandsAndFstab(t *testing.T) {
	share, ok := ParseShare("//192.0.2.10/Video")
	if !ok {
		t.Fatal("share did not parse")
	}
	guide := Guidance(share, "/mnt/nexusgate/Video", Host{OS: "linux", UID: 1000, GID: 1000})
	rendered := strings.Join(guide.Lines(), "\n")

	if !strings.Contains(rendered, "credentials=/etc/nexusgate/192.0.2.10.cred") {
		t.Error("the mount must read credentials from a file")
	}
	if !strings.Contains(rendered, "chmod 600 /etc/nexusgate/192.0.2.10.cred") {
		t.Error("the credentials file must be restricted to its owner")
	}
	for _, line := range guide.Lines() {
		if strings.Contains(line, "fstab") {
			continue
		}
		if strings.Contains(line, "password=") && !strings.Contains(line, "tee") {
			t.Errorf("a password reaches a command line: %s", line)
		}
	}
	fstab := ""
	for _, step := range guide.Steps {
		if strings.Contains(step.Title, "fstab") {
			fstab = strings.Join(step.Commands, "\n")
		}
	}
	if fstab == "" {
		t.Fatal("no fstab line was produced, so the mount would not survive a reboot")
	}
	if strings.Contains(fstab, "password") {
		t.Errorf("the fstab line carries a password: %s", fstab)
	}
	if !strings.Contains(fstab, "nofail") {
		t.Error("without nofail an absent NAS stops the machine from booting")
	}
}

func TestGuidanceStartsWithMachineLocationForEveryOS(t *testing.T) {
	share, ok := ParseShare("//nas/Video")
	if !ok {
		t.Fatal("share did not parse")
	}
	wantTitle := "These commands run on the machine running NexusGate, not on the computer you are reading this page on. If you are sitting at that machine, open a terminal; otherwise SSH into it first."
	for _, os := range []string{"linux", "darwin", "windows"} {
		guide := Guidance(share, "/mnt/nexusgate/Video", Host{OS: os})
		if len(guide.Steps) == 0 {
			t.Fatalf("%s: guidance has no steps", os)
		}
		first := guide.Steps[0]
		if first.Key != "run-on-hub-host" || first.Title != wantTitle || len(first.Commands) != 0 {
			t.Errorf("%s: first step = %+v, want the empty Hub-host framing step", os, first)
		}
	}
}

func TestGuidanceClassifiesFstabLinesAndInstallsLinuxClients(t *testing.T) {
	shareCases := []struct {
		input       string
		installKey  string
		installLine []string
		fstype      string
	}{
		{"//nas/Video", "install-smb-client", []string{"sudo apt-get install -y cifs-utils", "sudo dnf install -y cifs-utils"}, "cifs"},
		{"nas:/export/Video", "install-nfs-client", []string{"sudo apt-get install -y nfs-common", "sudo dnf install -y nfs-utils"}, "nfs"},
	}
	for _, tc := range shareCases {
		share, ok := ParseShare(tc.input)
		if !ok {
			t.Fatalf("ParseShare(%q) failed", tc.input)
		}
		guide := Guidance(share, "/mnt/nexusgate/Video", Host{OS: "linux", UID: 1000, GID: 1000})
		if guide.Steps[1].Key != tc.installKey {
			t.Fatalf("%s: first Linux step after host framing = %q, want %q", tc.input, guide.Steps[1].Key, tc.installKey)
		}
		if fmt.Sprint(guide.Steps[1].Commands) != fmt.Sprint(tc.installLine) {
			t.Errorf("%s: install commands = %v, want %v", tc.input, guide.Steps[1].Commands, tc.installLine)
		}
		for _, step := range guide.Steps {
			if step.Key == "fstab" {
				if step.Kind != "file-line" || step.File != "/etc/fstab" {
					t.Errorf("%s: fstab classification = kind %q, file %q", tc.input, step.Kind, step.File)
				}
				if len(step.Commands) != 1 || !strings.Contains(step.Commands[0], " "+tc.fstype+" ") {
					t.Errorf("%s: fstab line = %v", tc.input, step.Commands)
				}
				continue
			}
			if step.Kind != "" || step.File != "" {
				t.Errorf("%s: non-fstab step %q has kind %q and file %q", tc.input, step.Key, step.Kind, step.File)
			}
		}
	}
}

func TestGuidanceSMBCredentialsUseOneQuotedHereDoc(t *testing.T) {
	share, ok := ParseShare("smb://nas/Video")
	if !ok {
		t.Fatal("share did not parse")
	}
	guide := Guidance(share, "/mnt/nexusgate/Video", Host{OS: "linux", UID: 1000, GID: 1000})
	var credentials Step
	for _, step := range guide.Steps {
		if step.Key == "smb-credentials-file" {
			credentials = step
			break
		}
	}
	if len(credentials.Commands) != 3 {
		t.Fatalf("credentials step has %d commands, want directory, here-doc, chmod", len(credentials.Commands))
	}
	command := credentials.Commands[1]
	for _, want := range []string{
		"sudo tee /etc/nexusgate/nas.cred >/dev/null <<'NEXUSGATE_CREDS'",
		"username=YOUR_NAS_USERNAME",
		"password=YOUR_NAS_PASSWORD",
		"NEXUSGATE_CREDS",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("credentials here-doc missing %q: %s", want, command)
		}
	}
	if strings.Count(command, "YOUR_NAS_PASSWORD") != 1 {
		t.Errorf("password placeholder appears %d times in credentials content", strings.Count(command, "YOUR_NAS_PASSWORD"))
	}
	if strings.Contains(command, "printf") || len(strings.Split(command, "\n")) != 4 {
		t.Errorf("credentials content is not one four-line here-doc command: %q", command)
	}
}

// Footage is read-only everywhere else in this system; the mount is where that
// can be made true of the whole share rather than only of our own code.
func TestGuidanceMountsReadOnly(t *testing.T) {
	for _, input := range []string{"//nas/Video", "nas:/volume1/Video"} {
		share, _ := ParseShare(input)
		for _, host := range []Host{{OS: "linux"}, {OS: "darwin"}} {
			rendered := strings.Join(Guidance(share, "", host).Lines(), "\n")
			if !strings.Contains(rendered, "ro") {
				t.Errorf("%s on %s: mount is not read-only", input, host.OS)
			}
		}
	}
}

// The trap costs hours because nothing reports it: the mount succeeds, the
// shell can list the files, and the Hub sees an empty directory.
func TestWSLGuidanceMountsIntoPIDOnesNamespace(t *testing.T) {
	share, _ := ParseShare("//192.0.2.10/Video")
	rendered := strings.Join(Guidance(share, "", Host{OS: "linux", WSL: true}).Lines(), "\n")
	if !strings.Contains(rendered, "nsenter -t 1 -m") {
		t.Error("a WSL mount must be made in PID 1's mount namespace or services cannot see it")
	}
	if !strings.Contains(rendered, "namespace") {
		t.Error("the reason must be stated; the command alone looks like superstition")
	}

	plain := strings.Join(Guidance(share, "", Host{OS: "linux"}).Lines(), "\n")
	if strings.Contains(plain, "nsenter") {
		t.Error("nsenter must not be suggested outside WSL, where it is unnecessary")
	}
}

// Copy mode is the difference between a NAS library that finishes and one that
// does not, and it is off by default because it would duplicate a local one.
func TestGuidanceRecommendsStagingForNetworkRoots(t *testing.T) {
	share, _ := ParseShare("//nas/Video")
	rendered := strings.Join(Guidance(share, "", Host{OS: "linux"}).Lines(), "\n")
	if !strings.Contains(rendered, "source_staging") {
		t.Error("a network root without copy-mode advice will be slow for reasons the operator cannot see")
	}
}

// Every Step and Note carries a Key because the browser wizard translates
// generated guidance into Chinese by looking Key up in a table
// (internal/api/library_roots_page.go); a step or note with an empty Key
// would render blank rather than falling back to the English text, and two
// items sharing a Key would silently translate one as if it were the other.
// This is checked across every OS and share combination Guidance can
// produce, not just one, because a key is only assigned where a step or
// note is constructed and it is easy to add a new construction site (a new
// OS branch, a new WSL/windows-only note) without giving it a Key.
func TestGuidanceStepsAndNotesCarryStableKeys(t *testing.T) {
	smb, _ := ParseShare("//192.0.2.10/Video")
	nfs, _ := ParseShare("192.0.2.10:/volume1/Video")
	hosts := []Host{
		{OS: "linux", UID: 1000, GID: 1000},
		{OS: "linux", UID: 1000, GID: 1000, WSL: true},
		{OS: "darwin"},
		{OS: "windows"},
	}
	for _, share := range []Share{smb, nfs} {
		for _, host := range hosts {
			guide := Guidance(share, "", host)
			seen := map[string]bool{}
			for _, step := range guide.Steps {
				if step.Key == "" {
					t.Errorf("share=%s host=%+v: step %q has no key", share, host, step.Title)
					continue
				}
				if seen[step.Key] {
					t.Errorf("share=%s host=%+v: step key %q reused within one guide", share, host, step.Key)
				}
				seen[step.Key] = true
			}
			for _, note := range guide.Notes {
				if note.Key == "" {
					t.Errorf("share=%s host=%+v: note %q has no key", share, host, note.Text)
					continue
				}
				if seen[note.Key] {
					t.Errorf("share=%s host=%+v: note key %q reused within one guide", share, host, note.Key)
				}
				seen[note.Key] = true
			}
		}
	}
}

const mountinfoFixture = `21 25 0:20 / /proc rw,relatime shared:5 - proc proc rw
25 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
120 25 0:52 / /mnt/nexusgate/Video ro,relatime shared:66 - cifs //192.0.2.10/Video ro
131 25 0:55 / /mnt/wsl/host ro,relatime shared:70 - drvfs C:\134 ro
140 25 0:60 / /mnt/backup\040archive rw,relatime shared:80 - nfs4 nas:/export rw
150 25 0:61 / /mnt/empty rw,relatime shared:90 - ext4 /dev/sdb1 rw
`

func TestFilesystemForIdentifiesNetworkRoots(t *testing.T) {
	for _, tc := range []struct {
		path        string
		wantType    string
		wantNetwork bool
	}{
		{"/mnt/nexusgate/Video/2024/clip.mp4", "cifs", true},
		{"/mnt/wsl/host", "drvfs", true},
		{"/mnt/backup archive", "nfs4", true},
		{"/home/example/footage", "ext4", false},
		{"/mnt/empty", "ext4", false},
	} {
		got, ok := FilesystemFor(tc.path, mountinfoFixture)
		if !ok {
			t.Errorf("%s: no filesystem resolved", tc.path)
			continue
		}
		if got.Type != tc.wantType {
			t.Errorf("%s: got type %q, want %q", tc.path, got.Type, tc.wantType)
		}
		if got.Network != tc.wantNetwork {
			t.Errorf("%s: got network=%t, want %t", tc.path, got.Network, tc.wantNetwork)
		}
	}
}

// The deeper mount describes the path. Taking the first match instead would
// report every path as being on / and no root would ever look like a NAS.
func TestFilesystemForPrefersTheDeepestMount(t *testing.T) {
	got, ok := FilesystemFor("/mnt/nexusgate/Video", mountinfoFixture)
	if !ok {
		t.Fatal("no filesystem resolved")
	}
	if got.Mountpoint != "/mnt/nexusgate/Video" {
		t.Errorf("got mountpoint %q, want the cifs mount itself", got.Mountpoint)
	}
	if !got.ReadOnly {
		t.Error("a mount with the ro option must be reported as read-only")
	}
}

// An unreadable mount table means unknown. Reporting local would state
// something false about where the footage lives.
func TestFilesystemForReportsUnknownRatherThanGuessing(t *testing.T) {
	if _, ok := FilesystemFor("/mnt/footage", ""); ok {
		t.Error("an empty mount table must not resolve to a filesystem")
	}
}

// The Hub's primary distribution form is the Docker image, where the process
// generating this advice is itself the thing that cannot run `mount` — it is
// a fixed unprivileged uid with no CAP_SYS_ADMIN, on purpose. Guidance built
// for that case must say so explicitly rather than reading like host advice
// that happens not to work.
func TestContainerGuidanceDiffersFromHostGuidance(t *testing.T) {
	share, ok := ParseShare("//192.0.2.10/Video")
	if !ok {
		t.Fatal("share did not parse")
	}
	hostGuide := Guidance(share, "", Host{OS: "linux", UID: 1000, GID: 1000})
	containerGuide := Guidance(share, "", Host{OS: "linux", UID: 1000, GID: 1000, Container: true})

	if containerGuide.Steps[0].Key != "container-run-on-host" || len(containerGuide.Steps[0].Commands) != 0 {
		t.Fatalf("container guidance must lead with an empty Docker-host framing step, got %+v", containerGuide.Steps[0])
	}
	if hostGuide.Steps[0].Key != "run-on-hub-host" || len(hostGuide.Steps[0].Commands) != 0 {
		t.Fatalf("host guidance must lead with an empty Hub-host framing step, got %+v", hostGuide.Steps[0])
	}
	for _, step := range hostGuide.Steps {
		if step.Key == "container-run-on-host" {
			t.Error("host guidance must not carry the container framing step — it has nothing to be framed against")
		}
	}

	hasNote := func(guide Guide, key string) bool {
		for _, note := range guide.Notes {
			if note.Key == key {
				return true
			}
		}
		return false
	}
	if !hasNote(containerGuide, "container-cannot-mount") {
		t.Fatal("container guidance must explain why the container cannot mount for itself")
	}
	if hasNote(hostGuide, "container-cannot-mount") {
		t.Error("host guidance must not carry the container-only note")
	}

	note := ""
	for _, n := range containerGuide.Notes {
		if n.Key == "container-cannot-mount" {
			note = n.Text
		}
	}
	if !strings.Contains(note, "CAP_SYS_ADMIN") || !strings.Contains(note, "docker.sock") {
		t.Errorf("the note must name the two ways a container could otherwise acquire mount authority, got: %s", note)
	}
	if !strings.Contains(note, "rslave") {
		t.Errorf("the note must explain the bind propagation trap for a share mounted after the container started, got: %s", note)
	}
	if !strings.Contains(note, "docker compose up -d") {
		t.Errorf("the note must say how to recover without rslave propagation, got: %s", note)
	}
}

// Every construction site for a Step or Note needs a Key, including the two
// this task adds — TestGuidanceStepsAndNotesCarryStableKeys above only
// exercises non-container hosts, so this repeats that check specifically for
// Container:true, across both protocols.
func TestContainerGuidanceStepsAndNotesCarryStableKeys(t *testing.T) {
	smb, _ := ParseShare("//192.0.2.10/Video")
	nfs, _ := ParseShare("192.0.2.10:/volume1/Video")
	for _, share := range []Share{smb, nfs} {
		guide := Guidance(share, "", Host{OS: "linux", UID: 1000, GID: 1000, Container: true})
		seen := map[string]bool{}
		for _, step := range guide.Steps {
			if step.Key == "" {
				t.Errorf("share=%s: step %q has no key", share, step.Title)
				continue
			}
			if seen[step.Key] {
				t.Errorf("share=%s: step key %q reused within one guide", share, step.Key)
			}
			seen[step.Key] = true
		}
		for _, note := range guide.Notes {
			if note.Key == "" {
				t.Errorf("share=%s: note %q has no key", share, note.Text)
				continue
			}
			if seen[note.Key] {
				t.Errorf("share=%s: note key %q reused within one guide", share, note.Key)
			}
			seen[note.Key] = true
		}
	}
}

// The NFS form carries no credentials at all, which is exactly why it is the
// recommended docker-native path over SMB — so it must come back with an
// empty Warning, not a generic disclaimer.
func TestComposeVolumeNFSFormHasNoWarning(t *testing.T) {
	share, ok := ParseShare("192.0.2.10:/volume1/Video")
	if !ok {
		t.Fatal("share did not parse")
	}
	def, ok := ComposeVolume(share, "nas-video")
	if !ok {
		t.Fatal("an NFS share must produce a volume definition")
	}
	if def.Warning != "" {
		t.Errorf("the NFS form has no credentials to warn about, got Warning=%q", def.Warning)
	}
	if def.WarningKey != "" {
		t.Errorf("WarningKey must be empty exactly when Warning is empty, got %q", def.WarningKey)
	}
	for _, want := range []string{
		"driver: local",
		`type: "nfs"`,
		"addr=192.0.2.10,ro,soft,nolock",
		`device: ":/volume1/Video"`,
	} {
		if !strings.Contains(def.YAML, want) {
			t.Errorf("YAML missing %q, got:\n%s", want, def.YAML)
		}
	}
}

// Docker's local driver forwards driver_opts straight to mount(2) and never
// runs mount.cifs, so the SMB form's password cannot go in a credentials
// file the way the host mount's does — it has to be inline, in cleartext,
// in docker volume inspect output. That is exactly the exposure the rest of
// this project avoids by keeping secrets in an encrypted store, so a caller
// must be told about it rather than silently handed a working-looking YAML
// blob.
func TestComposeVolumeSMBFormWarnsAboutCleartextCredentials(t *testing.T) {
	share, ok := ParseShare("smb://ev@192.0.2.10/Video")
	if !ok {
		t.Fatal("share did not parse")
	}
	def, ok := ComposeVolume(share, "nas-video")
	if !ok {
		t.Fatal("an SMB share must produce a volume definition")
	}
	if def.Warning == "" {
		t.Fatal("the SMB form must warn that the password ends up in cleartext")
	}
	if def.WarningKey == "" {
		t.Fatal("a non-empty Warning must carry a stable WarningKey so the browser wizard can translate it, the same contract Step.Key and Note.Key already use")
	}
	for _, want := range []string{"docker volume inspect", "opts.json", "cleartext"} {
		if !strings.Contains(def.Warning, want) {
			t.Errorf("Warning missing %q, got: %s", want, def.Warning)
		}
	}
	for _, want := range []string{
		"driver: local",
		`type: "cifs"`,
		"username=ev",
		"uid=10001,gid=10001",
		`device: "//192.0.2.10/Video"`,
	} {
		if !strings.Contains(def.YAML, want) {
			t.Errorf("YAML missing %q, got:\n%s", want, def.YAML)
		}
	}
	if strings.Contains(def.YAML, "credentials=") {
		t.Error("the compose form must not suggest the credentials-file syntax Docker's local driver rejects")
	}
}

// A local path is not a network share and has nothing for driver_opts to
// say; the caller wants a plain bind mount instead.
func TestComposeVolumeRejectsNonNetworkShare(t *testing.T) {
	if _, ok := ComposeVolume(Share{}, "whatever"); ok {
		t.Error("a zero-value Share carries no recognised protocol and must not produce a volume definition")
	}
}

// Docker volume names are restricted to [a-zA-Z0-9][a-zA-Z0-9_.-]*, but
// Share.Name is an NFS export path or SMB share name pasted from a NAS admin
// page — it can carry "/", spaces, punctuation Docker rejects, or nothing
// ASCII at all. VolumeName must never hand ComposeVolume something invalid.
var dockerVolumeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func TestVolumeNameProducesDockerSafeNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		share Share
	}{
		{"plain nfs export", Share{Protocol: ProtocolNFS, Host: "192.0.2.10", Name: "/volume1/Video"}},
		{"share name with a slash", Share{Protocol: ProtocolSMB, Host: "nas", Name: "Video/2024"}},
		{"share name with characters Docker rejects", Share{Protocol: ProtocolSMB, Host: "nas.local", Name: "My Footage (2024)! 素材"}},
		{"empty share name", Share{Protocol: ProtocolNFS, Host: "nas", Name: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := VolumeName(tc.share)
			if !dockerVolumeNamePattern.MatchString(got) {
				t.Errorf("VolumeName(%+v) = %q, does not match Docker's volume name pattern %s", tc.share, got, dockerVolumeNamePattern)
			}
		})
	}
}

// Two shares that differ must not collide on the same volume name, and the
// same share must always produce the same name — the wizard regenerates the
// stanza across requests (see regenerateGuidance in
// internal/api/library_roots_page.go) and a name that moved would silently
// point an existing compose.yml at a different export.
func TestVolumeNameIsStableAndDistinguishesShares(t *testing.T) {
	a := Share{Protocol: ProtocolNFS, Host: "192.0.2.10", Name: "/volume1/Video"}
	b := Share{Protocol: ProtocolNFS, Host: "192.0.2.10", Name: "/volume1/Photos"}
	if VolumeName(a) != VolumeName(a) {
		t.Fatal("VolumeName must be deterministic for the same share")
	}
	if VolumeName(a) == VolumeName(b) {
		t.Fatalf("two distinct shares produced the same volume name %q", VolumeName(a))
	}
}

// TestComposeVolumeCarriesBothHalves guards the gap a design review found:
// a named volume no service mounts is inert, and an operator handed only the
// volumes: entry has to invent the service line, the container path, and the
// fact that the container path is what `root add` wants. Both protocols must
// carry both halves, and the Worker must appear alongside the Hub — it opens
// source files itself for leased derive jobs.
func TestComposeVolumeCarriesBothHalves(t *testing.T) {
	for _, input := range []string{"//192.0.2.10/Video", "192.0.2.10:/volume1/Video"} {
		share, ok := ParseShare(input)
		if !ok {
			t.Fatalf("ParseShare(%q) failed", input)
		}
		definition, ok := ComposeVolume(share, VolumeName(share))
		if !ok {
			t.Fatalf("ComposeVolume(%q) reported false", input)
		}
		if definition.MountPath == "" {
			t.Errorf("%s: MountPath is empty, so nothing tells the operator what to record as a root", input)
		}
		if !strings.Contains(definition.ServiceYAML, "hub:") || !strings.Contains(definition.ServiceYAML, "worker:") {
			t.Errorf("%s: ServiceYAML must mount into both services, got:\n%s", input, definition.ServiceYAML)
		}
		if !strings.Contains(definition.ServiceYAML, definition.MountPath) {
			t.Errorf("%s: ServiceYAML must mount at MountPath %q, got:\n%s", input, definition.MountPath, definition.ServiceYAML)
		}
		if !strings.Contains(definition.ServiceYAML, ":ro") {
			t.Errorf("%s: the service-side line must stay read-only, got:\n%s", input, definition.ServiceYAML)
		}
		if strings.HasPrefix(definition.MountPath, "/media/library") {
			t.Errorf("%s: MountPath %q nests inside the bind mount's target; the volume does not travel through the bind", input, definition.MountPath)
		}
	}
}
