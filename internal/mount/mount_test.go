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
		// The domain separator used to be allowed here as valid username data.
		// It is not data that survives: the compose o: scalar is double-quoted
		// YAML, so yaml.v3 reads DOMAIN\alice as DOMAIN, 0x07, "lice", and the
		// darwin mount_smbfs line is an unquoted shell argument that drops the
		// backslash outright. A dropped username shows the placeholder; a kept
		// one mounts as something the operator never typed.
		{"domain separator", `smb://DOMAIN\alice@nas/Video`},
		{"domain separator with an octal escape", `smb://DOMAIN\040alice@nas/Video`},
		{"backslash-n", `smb://CORP\nina@nas/Video`},
	}
	parsed := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			share, ok := ParseShare(tc.input)
			// Two outcomes are safe and which one an input gets is a property of
			// the input, not a weaker assertion: userinfo is only searched for
			// "@" ahead of the first "/", so a payload carrying a slash of its
			// own (the here-doc case embeds /tmp/pwned) puts a host in front of
			// it that fails validHost and is rejected outright. Rejecting is
			// strictly safer than the alternative below, so both are accepted
			// here; what neither may do is keep the hostile text.
			if !ok {
				return
			}
			parsed++
			if share.Host != "nas" || share.Name != "Video" {
				t.Fatalf("ParseShare(%q) = %+v, want host nas and share Video preserved", tc.input, share)
			}
			if share.User != "" {
				t.Fatalf("ParseShare(%q).User = %q, want dirty userinfo discarded", tc.input, share.User)
			}
		})
	}
	// Positive control for the early return above: if every case were rejected
	// outright, the drop-the-username path would be untested and this whole
	// table would pass without exercising validUser at all.
	if parsed == 0 {
		t.Fatal("no hostile input reached the parse-and-discard path, so validUser was never exercised")
	}
}

func TestParseShareAcceptsSafeSMBUsernames(t *testing.T) {
	for _, tc := range []struct {
		input string
		user  string
	}{
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
	// No slash inside the payload: userinfo is only searched for "@" ahead of
	// the first "/", so a payload carrying one is rejected by validHost before
	// guidance is ever generated. That rejection is safe but it would test
	// nothing here — the property under test is that a username which *does*
	// reach guidance cannot close the credentials here-doc early.
	const input = "smb://evil\nNEXUSGATE_CREDS\nwhoami\n@nas/Video"
	share, ok := ParseShare(input)
	if !ok {
		t.Fatal("hostile userinfo must not make the network share unrecognisable")
	}
	if share.User != "" {
		t.Fatalf("Share.User = %q, want the hostile username discarded", share.User)
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
				for _, payload := range []string{"evil", "whoami", "$(id)"} {
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
	for _, payload := range []string{"evil", "whoami", "$(id)"} {
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

	// Share.User was the first of three request-controlled fields reaching
	// these commands, not the only one. The share name and the mount point are
	// interpolated by the same unquoted %s, so this test owns all three rather
	// than leaving two of them to a reader noticing the asymmetry later.
	for _, hostile := range []string{
		"//nas/Video;id > /tmp/pwned",
		"//nas/Video`id`",
		"//nas/Video$(id)",
		"//nas/Video|id",
		"//nas/Video&id",
		"//nas/Video\nid",
		"smb://nas/My Share",
		"nas:/export;id",
		`\\nas\Video"; id; "`,
		// The UNC and // forms fold a backslash to / at the top of ParseShare,
		// so only the URI form carries one this far. It has to be rejected
		// here: the name reaches an fstab target, where libmount reads \040 as
		// a space, and a compose device: scalar, where yaml.v3 reads it as a
		// NUL byte followed by "40".
		`smb://nas/Video\040x`,
		// Not every rune above 0x80 is inert in the generated syntaxes, which
		// the ASCII-only rule assumed. yaml.v3 folds U+0085 in a device:
		// scalar to a space; U+2028 and U+2029 are the same class of YAML line
		// break in a 1.1 scanner.
		"smb://nas/Video\u0085x",
		"smb://nas/Video\u2028x",
		"smb://nas/Video\u2029x",
	} {
		if share, ok := ParseShare(hostile); ok {
			t.Errorf("%q parsed as the share %+v; a name carrying shell punctuation is not a share address", hostile, share)
		}
	}

	// Positive control for the block above. Without it a policy that rejected
	// every name whatsoever would satisfy every assertion here, and the share
	// names real operators have — Chinese, Japanese, an "@" in the name — are
	// exactly what a validUser-shaped Latin allowlist would have broken.
	for _, legitimate := range []struct {
		input string
		name  string
	}{
		{"//nas/Video", "Video"},
		{"//nas/素材库", "素材库"},
		{"//nas/撮影素材", "撮影素材"},
		{"//nas/My@Share", "My@Share"},
		{`\\nas\Photos@2024`, "Photos@2024"},
		{"//nas/Media+Archive", "Media+Archive"},
		{"nas:/export/video", "/export/video"},
	} {
		share, ok := ParseShare(legitimate.input)
		if !ok {
			t.Errorf("%q must still parse as a share", legitimate.input)
			continue
		}
		if share.Name != legitimate.name {
			t.Errorf("ParseShare(%q).Name = %q, want %q", legitimate.input, share.Name, legitimate.name)
		}
	}

	plain, ok := ParseShare("//nas/Video")
	if !ok {
		t.Fatal("the benign share must parse")
	}
	for _, hostile := range []string{
		"/mnt/x;id > /tmp/pwned",
		"/mnt/x`id`",
		"/mnt/x$(id)",
		"/mnt/x|id",
		"/mnt/My Footage",
		"/mnt/x\nid",
	} {
		for _, host := range []Host{
			{OS: "linux", UID: 1000, GID: 1000},
			{OS: "darwin"},
			{OS: "windows"},
			{OS: "linux", UID: 1000, GID: 1000, Container: true},
		} {
			guide := Guidance(plain, hostile, host)
			for _, step := range guide.Steps {
				for _, command := range step.Commands {
					if strings.Contains(command, hostile) {
						t.Errorf("mount point %q reached step %q on %s: %q", hostile, step.Key, host.OS, command)
					}
				}
			}
		}
	}

	// Positive control for the mount-point block: a usable one must still
	// produce commands that carry it, or the assertions above pass because
	// Guidance stopped emitting commands at all.
	usable := Guidance(plain, "/mnt/nexusgate/Video", Host{OS: "linux", UID: 1000, GID: 1000})
	carried := 0
	for _, step := range usable.Steps {
		for _, command := range step.Commands {
			if strings.Contains(command, "/mnt/nexusgate/Video") {
				carried++
			}
		}
	}
	if carried == 0 {
		t.Fatal("a usable mount point must still be interpolated into the generated commands")
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

func TestUnraidGuidanceIssuesNoShellCommands(t *testing.T) {
	share, ok := ParseShare("//nas/Video")
	if !ok {
		t.Fatal("share did not parse")
	}
	unraid := Guidance(share, "", Host{OS: "linux", Platform: "unraid", Container: true})
	if len(unraid.Steps) != 7 {
		t.Fatalf("Unraid guidance has %d steps, want 7", len(unraid.Steps))
	}
	for _, step := range unraid.Steps {
		if len(step.Commands) != 0 {
			t.Errorf("Unraid step %q has shell commands: %v", step.Key, step.Commands)
		}
	}

	// Positive control: the same share on an unknown platform still produces
	// the established Linux commands, so the empty result above is a branch
	// property rather than a consequence of an unrecognised share.
	plain := Guidance(share, "/mnt/nexusgate/Video", Host{OS: "linux"})
	hasCommand := false
	for _, step := range plain.Steps {
		if len(step.Commands) > 0 {
			hasCommand = true
			break
		}
	}
	if !hasCommand {
		t.Fatal("positive control for the plain platform produced no commands")
	}
}

// Unraid's mount flow is deliberately a web-UI sequence: Auto Mount must be
// saved before the explicit Mount action, or a later array restart loses the
// share even though the initial setup appeared to work.
func TestUnraidGuidancePlacesAutoMountBeforeMount(t *testing.T) {
	share := Share{Protocol: ProtocolSMB, Host: "nas", Name: "Video"}
	guide := Guidance(share, "", Host{OS: "linux", Platform: "unraid", Container: true})
	index := func(key string) int {
		for i, step := range guide.Steps {
			if step.Key == key {
				return i
			}
		}
		return -1
	}
	add, auto, mount := index("unraid-add-remote-smb"), index("unraid-auto-mount"), index("unraid-mount-remote")
	if add < 0 || auto < 0 || mount < 0 {
		t.Fatalf("Unraid guidance keys missing: add=%d auto=%d mount=%d", add, auto, mount)
	}
	if auto != add+1 || mount != auto+1 {
		t.Fatalf("Unraid step order = add %d, auto %d, mount %d; Auto Mount must be between add and Mount", add, auto, mount)
	}
	if guide.Steps[auto].Title != "Save the share and tick Auto Mount, so it returns when the array restarts" {
		t.Fatalf("Auto Mount step title = %q", guide.Steps[auto].Title)
	}
}

// Unraid's web UI owns the mount lifecycle, so its container note must not send
// an operator toward Compose, which Unraid does not run natively.
func TestUnraidContainerGuidanceUsesUnraidContainerNote(t *testing.T) {
	share := Share{Protocol: ProtocolSMB, Host: "nas", Name: "Video"}
	guide := Guidance(share, "", Host{OS: "linux", Platform: "unraid", Container: true})
	want := "This container cannot mount the share for itself, and that is deliberate — it runs unprivileged without CAP_SYS_ADMIN. Unassigned Devices does the mounting. If you mounted the share after this container started, open Docker, edit nexusgate-hub, apply the change to recreate the container, and the mount becomes visible in here."
	found := false
	for _, note := range guide.Notes {
		if note.Key == "unraid-container-note" {
			found = true
			if note.Text != want {
				t.Errorf("Unraid container note text = %q, want exact operator guidance", note.Text)
			}
		}
		if note.Key == "container-cannot-mount" {
			t.Error("Unraid guidance must not emit the generic Compose container note")
		}
		if strings.Contains(note.Text, "docker-compose.yml") || strings.Contains(note.Text, "docker compose up -d") {
			t.Errorf("Unraid note contains non-native Compose instructions: %q", note.Text)
		}
	}
	if !found {
		t.Fatal("Unraid container guidance must emit unraid-container-note (catalog key roots.guide.unraid-container-note)")
	}
}

func TestUnraidGuidanceDoesNotInventTheMountPoint(t *testing.T) {
	share := Share{Protocol: ProtocolSMB, Host: "nas", Name: "Video"}
	invented := "/mnt/remotes/" + share.Host + "_" + share.Name
	guide := Guidance(share, invented, Host{OS: "linux", Platform: "unraid", Container: true})
	titles := make([]string, 0, len(guide.Steps))
	for _, step := range guide.Steps {
		titles = append(titles, step.Title)
	}
	if strings.Contains(strings.Join(titles, "\n"), invented) {
		t.Fatalf("Unraid guidance invented the mount point %q in step text", invented)
	}

	// Positive control: the ordinary Linux branch includes the explicit path
	// in its generated command, proving this query would find the path there.
	plain := Guidance(share, invented, Host{OS: "linux"})
	if !strings.Contains(strings.Join(plain.Lines(), "\n"), invented) {
		t.Fatalf("positive control did not render the explicit mount point %q", invented)
	}
}

func TestUnraidGuidanceWinsOverContainerBranch(t *testing.T) {
	share := Share{Protocol: ProtocolSMB, Host: "nas", Name: "Video"}
	unraid := Guidance(share, "/mnt/remotes/nas_Video", Host{
		OS:        "linux",
		Platform:  "unraid",
		Container: true,
		MediaBind: Bind{Source: "/mnt/remotes", Target: "/media/library"},
	})
	for _, step := range unraid.Steps {
		if step.Key == "container-run-on-host" {
			t.Fatalf("Unraid guidance took the container framing branch: %+v", step)
		}
		for _, command := range step.Commands {
			if strings.Contains(command, "mount -t cifs") {
				t.Fatalf("Unraid guidance issued a CIFS mount command: %q", command)
			}
		}
	}

	// Positive control: a container with the platform left unknown reaches the
	// existing framing and CIFS command, proving both negative checks target a
	// real alternative branch.
	plain := Guidance(share, "/mnt/remotes/nas_Video", Host{
		OS:        "linux",
		Container: true,
		MediaBind: Bind{Source: "/mnt/remotes", Target: "/media/library"},
	})
	hasContainerFraming := false
	hasCIFS := false
	for _, step := range plain.Steps {
		if step.Key == "container-run-on-host" {
			hasContainerFraming = true
		}
		for _, command := range step.Commands {
			if strings.Contains(command, "mount -t cifs") {
				hasCIFS = true
			}
		}
	}
	if !hasContainerFraming || !hasCIFS {
		t.Fatalf("positive control missed the existing container branch: framing=%t cifs=%t", hasContainerFraming, hasCIFS)
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
// The sink that made the domain separator indefensible. driver_opts o: and
// device: are double-quoted YAML scalars, so every backslash reaching them is
// an escape introducer to the parser that reads the file, not a character in
// the value. This asserts the property at the generated text rather than at the
// validator, because the validator is one of two things that could regress and
// the parser will not tell anyone when it does.
func TestComposeVolumeEmitsNoBackslashIntoADoubleQuotedScalar(t *testing.T) {
	for _, input := range []string{
		`smb://DOMAIN\alice@nas/Video`,
		`smb://DOMAIN\040alice@nas/Video`,
		`smb://CORP\nina@nas/Video`,
		`smb://alice@nas/Video`,
		"//nas/素材库",
		"nas:/export/video",
	} {
		share, ok := ParseShare(input)
		if !ok {
			continue // rejected outright is safe; this test owns what survives
		}
		volume, ok := ComposeVolume(share, VolumeName(share))
		if !ok {
			continue
		}
		if strings.Contains(volume.YAML, `\`) {
			t.Errorf("ParseShare(%q) produced compose YAML carrying a backslash, which the\nYAML parser reads as an escape rather than as data:\n%s", input, volume.YAML)
		}
	}

	// Positive control: without it a ComposeVolume that returned an empty string
	// would satisfy every assertion above.
	share, ok := ParseShare(`smb://alice@nas/Video`)
	if !ok {
		t.Fatal("smb://alice@nas/Video must parse")
	}
	volume, ok := ComposeVolume(share, VolumeName(share))
	if !ok {
		t.Fatal("an SMB share must produce a compose volume")
	}
	if !strings.Contains(volume.YAML, "username=alice,") {
		t.Errorf("compose YAML lost the clean username:\n%s", volume.YAML)
	}
}

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

// The wizard used to contradict itself on Windows: windowsSteps mapped a drive
// letter while DefaultMountpoint, having no windows branch at all, fell through
// to /mnt/nexusgate/<share> — so addRootStep told the operator to record a path
// that cannot exist on the machine the instructions were written for. The two
// must name the same location.
func TestWindowsGuidanceRecordsThePathItToldYouToOpen(t *testing.T) {
	share, ok := ParseShare(`\\nas01\Video`)
	if !ok {
		t.Fatal(`\\nas01\Video should parse as a share`)
	}
	host := Host{OS: "windows"}
	mountpoint := DefaultMountpoint(share, host)
	if mountpoint != `\\nas01\Video` {
		t.Fatalf("DefaultMountpoint = %q, want the UNC path", mountpoint)
	}
	guide := Guidance(share, mountpoint, host)
	var recorded string
	for _, step := range guide.Steps {
		for _, command := range step.Commands {
			if strings.HasPrefix(command, "nexusgate root add ") {
				recorded = strings.TrimPrefix(command, "nexusgate root add ")
			}
		}
	}
	if recorded == "" {
		t.Fatal("no root add command in the Windows guidance")
	}
	if recorded != mountpoint {
		t.Fatalf("root add records %q but the operator was told to open %q", recorded, mountpoint)
	}
	if strings.Contains(recorded, "/mnt/") {
		t.Fatalf("root add records a POSIX path on Windows: %q", recorded)
	}
}

// A drive letter and a Finder volume both belong to the login session that
// created them, so offering either to a Hub running as a service is advice that
// cannot work. The interactive cases are asserted alongside as the positive
// control: without them, a step list that lost net use entirely would pass.
// A mount point carrying a backslash used to pass, because the generated fstab
// line and the sudo mkdir -p line above it were assumed to name the same
// directory. They do not: libmount decodes \040 in a target, so findmnt
// resolves an fstab entry for /mnt/x\040y to /mnt/x y while mkdir creates
// something else, and the operator mounts a directory neither of them read as
// the one they typed. Windows is the exception the rule has to keep, not an
// oversight — there a backslash is the separator DefaultMountpoint itself
// returns.
func TestValidMountpointRejectsEscapesExceptWhereTheyArePathSeparators(t *testing.T) {
	linux := Host{OS: "linux"}
	windows := Host{OS: "windows"}

	for _, c := range []struct {
		mountpoint string
		host       Host
		want       bool
		why        string
	}{
		{`/mnt/x\040y`, linux, false, "fstab decodes \\040 to a space; mkdir does not"},
		{`/mnt/nexusgate/Video\`, linux, false, "a trailing escape eats the next fstab field"},
		{"/mnt/nexusgate/Video\u0085", linux, false, "not every rune above 0x80 is inert"},
		{"/mnt/nexusgate/Video", linux, true, "the ordinary answer must keep working"},
		{"/mnt/nexusgate/素材库", linux, true, "positive control: CJK names still mount"},
		{`\\nas01\Video`, windows, true, "the UNC path DefaultMountpoint returns"},
		{`Z:\Video`, windows, true, "a drive path is the other Windows shape"},
		{`\\nas01\Video;id`, windows, false, "the separator exception is not a bypass"},
	} {
		if got := ValidMountpoint(c.mountpoint, c.host); got != c.want {
			t.Errorf("ValidMountpoint(%q, %s) = %v, want %v — %s", c.mountpoint, c.host.OS, got, c.want, c.why)
		}
	}

	// The fail-safe shape Guidance documents: a rejected mount point yields no
	// commands at all rather than commands naming a directory the operator did
	// not type.
	share, ok := ParseShare("//nas01/Video")
	if !ok {
		t.Fatal("//nas01/Video should parse as a share")
	}
	if guide := Guidance(share, `/mnt/x\040y`, linux); len(guide.Steps) != 0 {
		t.Errorf("Guidance emitted %d step(s) for a rejected mount point; want none", len(guide.Steps))
	}

	// The Windows round trip the split exists for: whatever DefaultMountpoint
	// hands the wizard has to survive being posted back.
	if mp := DefaultMountpoint(share, windows); !ValidMountpoint(mp, windows) {
		t.Errorf("DefaultMountpoint returned %q, which ValidMountpoint then rejects", mp)
	}
}

func TestServiceHostIsNotOfferedASessionScopedMount(t *testing.T) {
	share, ok := ParseShare(`\\nas01\Video`)
	if !ok {
		t.Fatal("share should parse")
	}
	hasStep := func(guide Guide, key string) bool {
		for _, step := range guide.Steps {
			if step.Key == key {
				return true
			}
		}
		return false
	}
	hasNote := func(guide Guide, key string) bool {
		for _, note := range guide.Notes {
			if note.Key == key {
				return true
			}
		}
		return false
	}

	interactive := Guidance(share, DefaultMountpoint(share, Host{OS: "windows"}), Host{OS: "windows"})
	if !hasStep(interactive, "windows-map") {
		t.Error("an interactive Windows Hub should still be offered the drive-letter convenience")
	}
	if !hasStep(interactive, "windows-explorer-unc") {
		t.Error("the Explorer UNC step must lead, for both interactive and service hosts")
	}

	service := Guidance(share, DefaultMountpoint(share, Host{OS: "windows", Service: true}), Host{OS: "windows", Service: true})
	if hasStep(service, "windows-map") {
		t.Error("a service does not inherit the operator's mapped drives, so net use must not be offered to one")
	}
	if !hasStep(service, "windows-explorer-unc") {
		t.Error("the service host still needs the UNC step")
	}
	if !hasNote(service, "windows-service-account") {
		t.Error("a service host must be told its logon account is what the NAS authenticates")
	}

	nfsShare := Share{Protocol: ProtocolSMB, Host: "nas01", Name: "Video"}
	darwinService := Guidance(nfsShare, "/Volumes/Video", Host{OS: "darwin", Service: true})
	if !hasNote(darwinService, "darwin-session-scope") {
		t.Error("a macOS service host must be warned that a Finder mount is session-scoped")
	}
	darwinUser := Guidance(nfsShare, "/Volumes/Video", Host{OS: "darwin"})
	if hasNote(darwinUser, "darwin-session-scope") {
		t.Error("the session-scope warning is noise for a Hub running as the logged-in user")
	}
}

// "@" is legal in an SMB share name, and userinfo can only precede the host, so
// only an "@" before the first "/" separates a username. Searching the whole
// string found one inside the share name instead: //nas/My@Share split into a
// username of "nas/My", left nothing that parsed as host/share, and stopped
// being recognised as a share at all — so the wizard offered local-directory
// advice for a NAS path. The UPN case is the reason the search within the
// authority is still LastIndex rather than Index.
func TestParseShareSplitsUserinfoOnlyBeforeTheHost(t *testing.T) {
	for _, tc := range []struct {
		in         string
		host, name string
		user       string
	}{
		{`//nas/My@Share`, "nas", "My@Share", ""},
		{`\\nas\Photos@2024`, "nas", "Photos@2024", ""},
		{`smb://alice@nas/Video`, "nas", "Video", "alice"},
		{`smb://alice@corp.com@nas/Video`, "nas", "Video", "alice@corp.com"},
		{`smb://alice@nas/My@Share`, "nas", "My@Share", "alice"},
		{`//nas.local/Video`, "nas.local", "Video", ""},
	} {
		share, ok := ParseShare(tc.in)
		if !ok {
			t.Errorf("ParseShare(%q) rejected the address outright", tc.in)
			continue
		}
		if share.Host != tc.host || share.Name != tc.name || share.User != tc.user {
			t.Errorf("ParseShare(%q) = host %q name %q user %q, want host %q name %q user %q",
				tc.in, share.Host, share.Name, share.User, tc.host, tc.name, tc.user)
		}
	}
}

// ContainerPath's contract is that it reports false rather than guessing when
// the host path is not under the bind's source. A textual prefix test is not a
// containment test: /mnt/remotes/../etc starts with /mnt/remotes/ and is /etc,
// and it used to translate to /media/etc with ok=true — a path outside the bind
// target that the wizard then hands the operator as the one to record with
// `root add`.
func TestContainerPathRefusesPathsThatOnlyLookContained(t *testing.T) {
	host := Host{OS: "linux", Container: true, MediaBind: Bind{Target: "/media/library", Source: "/mnt/remotes"}}
	// Positive control: a genuinely contained path still translates, so the
	// refusals below are evidence and not a function that stopped working.
	if got, ok := ContainerPath("/mnt/remotes/nas_Video", host); !ok || got != "/media/library/nas_Video" {
		t.Fatalf("contained path translated to %q ok=%v, want /media/library/nas_Video true", got, ok)
	}
	for _, escape := range []string{
		"/mnt/remotes/../etc",
		"/mnt/remotes/a/../../etc/shadow",
		"/mnt/remotes/./../..",
	} {
		got, ok := ContainerPath(escape, host)
		if ok {
			t.Errorf("ContainerPath(%q) = %q, true — it escapes the bind and must be refused", escape, got)
		}
	}
	// A sibling directory sharing a name prefix was already refused; keep it so.
	if _, ok := ContainerPath("/mnt/remotesEVIL/x", host); ok {
		t.Error("a sibling directory with a shared name prefix must not count as contained")
	}
}
