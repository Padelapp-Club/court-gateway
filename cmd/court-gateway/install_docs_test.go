package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Padelapp-Club/court-gateway/internal/doctor"
	"github.com/Padelapp-Club/court-gateway/internal/update"
)

// The install commands of README.md, AGENTS.md and deploy/cloud-init.yaml
// are run as written, against a stand-in release (a script that records
// its arguments) and stand-ins for the network, the platform and sudo;
// only the release key they publish is swapped for a test key. With
// anything wrong (a checksum, a SHA256SUMS the release key did not sign, a
// missing file, an ssh-keygen that cannot verify) the program must never
// run, and the commands must say what is wrong; with nothing wrong it must.

// docSigner is the release key the install docs publish (allowed_signers).
const docSigner = "court-gateway ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJAe4CDRNNnk++UtmLOdDcQcRc0jK56eYJPo+DfdyK8y"

// signerLine is a whole allowed_signers line of the release key: the key
// ends where the quoted string does, so a key with anything appended to it
// is a different key.
var signerLine = regexp.MustCompile(`court-gateway ssh-ed25519 [A-Za-z0-9+/=]+`)

// The install docs publish one release key.
func TestInstallDocsPublishOneReleaseKey(t *testing.T) {
	for _, doc := range []string{"README.md", "AGENTS.md", "deploy/cloud-init.yaml"} {
		text := readDoc(t, doc)
		keys := signerLine.FindAllString(text, -1)
		if len(keys) == 0 || len(keys) != strings.Count(text, "ssh-ed25519 ") {
			t.Errorf("%s: %d whole release keys among %d ssh-ed25519 lines", doc, len(keys), strings.Count(text, "ssh-ed25519 "))
		}
		for _, k := range keys {
			if k != docSigner {
				t.Errorf("%s: release key %q, want %q", doc, k, docSigner)
			}
		}
	}
}

// fencedBlock returns the first ```lang block after the line containing
// marker in doc.
func fencedBlock(t *testing.T, doc, marker, lang string) string {
	t.Helper()
	text := readDoc(t, doc)
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("%s: no %q", doc, marker)
	}
	text = text[i:]
	open := "```" + lang + "\n"
	j := strings.Index(text, open)
	if j < 0 {
		t.Fatalf("%s: no %s block after %q", doc, lang, marker)
	}
	text = text[j+len(open):]
	return text[:strings.Index(text, "```")]
}

// readDoc returns a file of the repository.
func readDoc(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type installWorld struct {
	dir, bin, fixtures, mark, signer string
	env                              []string
}

// fault is what is wrong with a stand-in release, or with the machine it
// is installed on.
type fault int

const (
	noFault       fault = iota
	wrongChecksum       // SHA256SUMS gives the file another sum
	notSigned           // SHA256SUMS is not the one the release key signed
	noSums              // the release has no SHA256SUMS
	twoBuilds           // the signed SHA256SUMS names two builds for the platform
	noBuild             // the release has no file under the name SHA256SUMS gives
	oldKeygen           // ssh-keygen predates -Y (OpenSSH before 8.1)
	noKeygen            // there is no ssh-keygen
	otherPlatform       // a machine there is no build for
)

var faults = []fault{wrongChecksum, notSigned, noSums, twoBuilds, noBuild, oldKeygen, noKeygen, otherPlatform}

// unames are what uname -s and uname -m print on each POSIX release
// platform.
var unames = map[string][2]string{"linux-amd64": {"Linux", "x86_64"}, "linux-arm64": {"Linux", "aarch64"},
	"darwin-arm64": {"Darwin", "arm64"}, "darwin-amd64": {"Darwin", "x86_64"}}

// newInstallWorld puts a stand-in release where the stand-in curl finds
// it: the file named release (court-gateway-9.9.9-<platform>), a
// SHA256SUMS with its sum and the sums of the other release platforms,
// and SHA256SUMS.sig, the signature of it by a test release key — each
// as f spoils it. uname reports release's platform; sudo, runuser and the
// system tools are stand-ins too. w.signer is the test key's
// allowed_signers line.
func newInstallWorld(t *testing.T, release string, f fault) *installWorld {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands")
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen")
	}
	sum := ""
	for _, tool := range []string{"sha256sum", "shasum"} {
		if p, err := exec.LookPath(tool); err == nil {
			sum = p
			break
		}
	}
	if sum == "" {
		t.Skip("no sha256sum or shasum")
	}
	w := &installWorld{dir: t.TempDir()}
	w.bin, w.fixtures, w.mark = filepath.Join(w.dir, "bin"), filepath.Join(w.dir, "release"), filepath.Join(w.dir, "ran")
	for _, d := range []string{w.bin, w.fixtures, filepath.Join(w.dir, "home")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	program := "#!/bin/sh\necho \"$*\" >> '" + w.mark + "'\n"
	digest := sha256.Sum256([]byte(program))
	hexSum := hex.EncodeToString(digest[:])
	if f == wrongChecksum {
		hexSum = strings.Repeat("0", 64)
	}
	write := func(path, data string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	if f != noBuild {
		write(filepath.Join(w.fixtures, release), program, 0o644)
	}
	platform := strings.TrimPrefix(release, "court-gateway-9.9.9-")
	lines := ""
	for _, p := range update.Platforms { // the release's own line among the others, as scripts/build.sh writes them
		if p == platform {
			lines += hexSum + "  " + release + "\n"
		} else {
			lines += strings.Repeat("1", 64) + "  court-gateway-9.9.9-" + p + map[bool]string{true: ".exe"}[strings.HasPrefix(p, "windows-")] + "\n"
		}
	}
	if f == twoBuilds {
		lines += strings.Repeat("3", 64) + "  court-gateway-9.9.8-" + platform + "\n"
	}
	sums := filepath.Join(w.fixtures, "SHA256SUMS")
	write(sums, lines, 0o644)
	key := filepath.Join(w.dir, "release-key")
	for _, args := range [][]string{{"-q", "-t", "ed25519", "-N", "", "-C", "", "-f", key}, {"-q", "-Y", "sign", "-f", key, "-n", "court-gateway", sums}} {
		if out, err := exec.Command(keygen, args...).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen %v: %v\n%s", args, err, out)
		}
	}
	pub, _ := os.ReadFile(key + ".pub")
	w.signer = "court-gateway " + strings.TrimSpace(string(pub))
	switch f {
	case notSigned: // a SHA256SUMS with the file's right sum, but not the one signed
		write(sums, hexSum+"  "+release+"\n", 0o644)
	case noSums:
		_ = os.Remove(sums)
	}
	// curl: the file named by the URL's last element, to -o or to its own name (-O)
	write(filepath.Join(w.bin, "curl"), `#!/bin/sh
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
    -*) ;;
    *) url="$1" ;;
  esac
  shift
done
name="${url##*/}"
[ -f "`+w.fixtures+`/$name" ] || exit 22
cp "`+w.fixtures+`/$name" "${out:-$name}"
`, 0o755)
	uname := unames[platform]
	if f == otherPlatform {
		uname = [2]string{"Linux", "i686"}
	}
	write(filepath.Join(w.bin, "uname"), "#!/bin/sh\ncase \"$1\" in -m) echo "+uname[1]+" ;; *) echo "+uname[0]+" ;; esac\n", 0o755)
	write(filepath.Join(w.bin, "sudo"), "#!/bin/sh\nexec \"$@\"\n", 0o755)
	write(filepath.Join(w.bin, "runuser"), "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done; shift; exec \"$@\"\n", 0o755)
	for _, noop := range []string{"useradd", "chown", "sleep", "id"} {
		write(filepath.Join(w.bin, noop), "#!/bin/sh\nexit 0\n", 0o755)
	}
	write(filepath.Join(w.bin, "dpkg"), "#!/bin/sh\necho amd64\n", 0o755)
	// both checksum tools, whichever this machine has
	write(filepath.Join(w.bin, "sha256sum"), "#!/bin/sh\nexec "+map[bool]string{true: sum, false: sum + " -a 256"}[filepath.Base(sum) == "sha256sum"]+" \"$@\"\n", 0o755)
	write(filepath.Join(w.bin, "shasum"), "#!/bin/sh\n[ \"$1\" = -a ] && shift 2\nexec "+map[bool]string{true: sum, false: sum + " -a 256"}[filepath.Base(sum) == "sha256sum"]+" \"$@\"\n", 0o755)
	system := "/usr/bin:/bin"
	switch f {
	case oldKeygen: // as OpenSSH before 8.1 answers -Y
		write(filepath.Join(w.bin, "ssh-keygen"), "#!/bin/sh\necho 'ssh-keygen: illegal option -- Y' >&2\necho 'usage: ssh-keygen [-q] [-b bits] [-C comment] [-f output_keyfile]' >&2\nexit 1\n", 0o755)
	case noKeygen: // the system's tools, all but ssh-keygen
		system = filepath.Join(w.dir, "system")
		_ = os.MkdirAll(system, 0o755)
		for _, d := range []string{"/bin", "/usr/bin"} {
			entries, _ := os.ReadDir(d)
			for _, e := range entries {
				if e.Name() != "ssh-keygen" {
					_ = os.Symlink(filepath.Join(d, e.Name()), filepath.Join(system, e.Name()))
				}
			}
		}
	}
	w.env = []string{"PATH=" + w.bin + ":" + system, "HOME=" + filepath.Join(w.dir, "home"), "LC_ALL=C"}
	return w
}

// run runs script with shell (sh, bash or zsh without comment support, as
// zsh is interactively) in dir and reports the exit code and output.
func (w *installWorld) run(t *testing.T, shell, dir, script string) (int, string) {
	t.Helper()
	script = strings.ReplaceAll(script, docSigner, w.signer)
	var cmd *exec.Cmd
	switch shell {
	case "zsh":
		cmd = exec.Command("zsh", "-f", "-c", "unsetopt interactivecomments; eval \"$1\"", "zsh", script)
	default:
		cmd = exec.Command(shell, "-c", script)
	}
	cmd.Dir, cmd.Env = dir, w.env
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	} else if err != nil {
		t.Fatalf("%s: %v", shell, err)
	}
	return 0, string(out)
}

func (w *installWorld) ran() string {
	data, _ := os.ReadFile(w.mark)
	return string(data)
}

// The POSIX install blocks of README.md and AGENTS.md, run as written in
// sh, bash and zsh: no version to fill in, the platform found by uname
// and the file name taken from the verified SHA256SUMS. Every fault stops
// them before anything runs the program, with its own message and no
// other; with none they run it on every platform.
func TestPosixInstallBlocksSayWhatIsWrong(t *testing.T) {
	for _, d := range []struct {
		doc, marker string
		msgs        map[fault]string
		then, want  string // appended to the block; what the program then runs
	}{
		{"README.md", "### macOS и Linux", map[fault]string{
			wrongChecksum: "контрольная сумма court-gateway-9.9.9-", notSigned: "подпись SHA256SUMS не сошлась",
			noSums: "не удалось скачать SHA256SUMS", twoBuilds: "в SHA256SUMS не одна сборка",
			noBuild: "не удалось скачать https://", oldKeygen: "ssh-keygen старше OpenSSH 8.1",
			noKeygen: "нет ssh-keygen", otherPlatform: "выпуска нет",
		}, "", "setup\ninstall-service\ndoctor\n"},
		{"AGENTS.md", "\nLinux and macOS:\n", map[fault]string{
			wrongChecksum: "CHECKSUM MISMATCH", notSigned: "SIGNATURE MISMATCH", noSums: "DOWNLOAD FAILED: SHA256SUMS",
			twoBuilds: "NOT ONE BUILD", noBuild: "DOWNLOAD FAILED: court-gateway-", oldKeygen: "SSH-KEYGEN TOO OLD",
			noKeygen: "NO SSH-KEYGEN", otherPlatform: "UNSUPPORTED PLATFORM",
		}, "[ -x court-gateway ] && ./court-gateway version\n", "version\n"},
	} {
		block := fencedBlock(t, d.doc, d.marker, "sh")
		if regexp.MustCompile(`<\pL+>`).MatchString(block) {
			t.Fatalf("%s: the install block has a placeholder to fill in:\n%s", d.doc, block)
		}
		said := func(out string) []fault {
			var got []fault
			for f, msg := range d.msgs {
				if strings.Contains(out, msg) {
					got = append(got, f)
				}
			}
			return got
		}
		for _, shell := range []string{"sh", "bash", "zsh"} {
			if _, err := exec.LookPath(shell); err != nil {
				continue
			}
			t.Run(d.doc+"/"+shell, func(t *testing.T) {
				t.Parallel()
				for _, f := range faults {
					w := newInstallWorld(t, "court-gateway-9.9.9-linux-amd64", f)
					rc, out := w.run(t, shell, w.dir, block+d.then)
					if rc == 0 || w.ran() != "" || strings.Contains(out, "checksum-ok") || !slices.Equal(said(out), []fault{f}) {
						t.Errorf("fault %d (%q): exit %d, program ran %q, messages of faults %v\n%s", f, d.msgs[f], rc, w.ran(), said(out), out)
					}
				}
				for platform := range unames {
					w := newInstallWorld(t, "court-gateway-9.9.9-"+platform, noFault)
					rc, out := w.run(t, shell, w.dir, block+d.then)
					if rc != 0 || w.ran() != d.want || len(said(out)) != 0 {
						t.Errorf("%s: exit %d, program ran %q\n%s", platform, rc, w.ran(), out)
					}
				}
			})
		}
	}
}

// The cloud-init script, with its paths moved into a temporary root: a
// wrong checksum stops it before the program runs; the secrets and the
// user data copies on disk are gone after every run.
func TestCloudInitStopsOnChecksumMismatchAndForgetsSecrets(t *testing.T) {
	data := []byte(readDoc(t, "deploy/cloud-init.yaml"))
	var doc struct {
		WriteFiles []struct{ Path, Content string } `yaml:"write_files"`
		RunCmd     [][]string                       `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		fault   fault
		secrets string
		wantRC  int
		wantRan string
	}{
		{"wrong checksum", wrongChecksum, "CRM_PASSWORD='x'\nGATEWAY_JOIN_CODE='y'\n", 1, ""},
		{"not signed", notSigned, "CRM_PASSWORD='x'\nGATEWAY_JOIN_CODE='y'\n", 1, ""},
		{"no secrets: setup over SSH", noFault, doc.WriteFiles[0].Content, 0, ""},
		{"secrets given", noFault, "CRM_PASSWORD='x'\nGATEWAY_JOIN_CODE='y'\n", 0, "setup --non-interactive --json --config DIR/gateway.yaml\ninstall-service -config DIR/gateway.yaml --run-as court-gateway\ndoctor -config DIR/gateway.yaml\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newInstallWorld(t, "court-gateway-9.9.9-linux-amd64", c.fault)
			root, cloud, run, opt := filepath.Join(w.dir, "root"), filepath.Join(w.dir, "cloud"), filepath.Join(w.dir, "run"), filepath.Join(w.dir, "opt")
			for _, d := range []string{root, cloud, run} {
				_ = os.MkdirAll(d, 0o755)
			}
			userData := []string{"user-data.txt", "user-data.txt.i", "cloud-config.txt", "obj.pkl",
				filepath.Join(run, "combined-cloud-config.json"), filepath.Join(run, "instance-data-sensitive.json")}
			for _, f := range userData {
				if !filepath.IsAbs(f) {
					f = filepath.Join(cloud, f)
				}
				_ = os.WriteFile(f, []byte("user data"), 0o600)
			}
			secrets := filepath.Join(root, "court-gateway-setup.env")
			_ = os.WriteFile(secrets, []byte(c.secrets), 0o600)
			script := strings.NewReplacer("/opt/court-gateway", opt, "/root/", root+"/", "/var/lib/cloud/instance", cloud, "/run/cloud-init", run).Replace(doc.RunCmd[0][2])
			rc, out := w.run(t, "sh", w.dir, script)
			if (rc == 0) != (c.wantRC == 0) || w.ran() != strings.ReplaceAll(c.wantRan, "DIR", opt) {
				t.Fatalf("exit %d, program ran %q\n%s", rc, w.ran(), out)
			}
			for _, f := range append([]string{secrets}, userData...) {
				if !filepath.IsAbs(f) {
					f = filepath.Join(cloud, f)
				}
				if _, err := os.Stat(f); err == nil {
					t.Fatalf("%s is still on disk", f)
				}
			}
		})
	}
}

// PowerShell is not run here: the Windows blocks are checked for their
// shape. One script block (a throw ends all of it, not only its own
// line) with nothing to fill in. In order: the platform, TLS 1.2 (Windows
// PowerShell 5.1 does not offer it by default), the README's folder,
// SHA256SUMS and its signature downloaded (README: or taken from the
// folder, a .txt the browser added renamed, and a clear throw if neither),
// ssh-keygen there, the signature, the build's name from the verified
// SHA256SUMS, the build downloaded, its hash; each check throws its own
// message, and nothing renames or runs the file before the hash. The
// pattern that picks the build is run here on a SHA256SUMS of every
// release platform.
func TestWindowsInstallBlocksCheckBeforeRunning(t *testing.T) {
	for doc, marker := range map[string]string{"README.md": "### Windows", "AGENTS.md": "\nWindows: run in PowerShell"} {
		block := strings.TrimSpace(fencedBlock(t, doc, marker, "powershell"))
		body := block[strings.Index(block, "& {"):]
		if !strings.HasPrefix(body, "& {") || !strings.HasSuffix(body, "}") || !strings.Contains(body, docSigner) {
			t.Fatalf("%s: one script block with the release key:\n%s", doc, block)
		}
		if regexp.MustCompile(`<\pL+>`).MatchString(body) || strings.Contains(body, "$f = '") {
			t.Fatalf("%s: the block has a version to fill in:\n%s", doc, block)
		}
		// "$name:" in a PowerShell string is a scope-qualified variable: a parse error
		if bad := regexp.MustCompile(`\$\w+:[^\w]`).FindString(body); bad != "" {
			t.Fatalf("%s: %q does not parse in a PowerShell string:\n%s", doc, bad, block)
		}
		steps := []string{"SecurityProtocol = 'Tls12'", "$env:PROCESSOR_ARCHITECTURE", "Invoke-WebRequest \"$b/$n\" -OutFile $n -UseBasicParsing -ErrorAction Stop",
			"& $get SHA256SUMS\n", "& $get SHA256SUMS.sig\n", "Get-Command ssh-keygen", "ssh-keygen -Y verify", "Select-String -LiteralPath SHA256SUMS",
			"& $get $f\n", "Get-FileHash", "Move-Item"}
		if doc == "README.md" {
			steps = slices.Insert(steps, 2, "New-Item -ItemType Directory -Force C:\\court-gateway -ErrorAction Stop", "Set-Location C:\\court-gateway -ErrorAction Stop")
			steps = slices.Insert(steps, 5, `Rename-Item -LiteralPath "$n.txt" $n`, `if (-not (Test-Path -LiteralPath $n)) { throw`)
		}
		at := -1
		for _, step := range steps {
			i := strings.Index(body, step)
			if i <= at {
				t.Fatalf("%s: %q is missing or out of order (%v):\n%s", doc, step, steps, block)
			}
			at = i
		}
		line := func(i int) string { return body[strings.LastIndex(body[:i], "\n")+1 : i+strings.Index(body[i:], "\n")] }
		verify := strings.Index(body, "ssh-keygen -Y verify")
		after := body[verify+strings.Index(body[verify:], "\n")+1:]
		for _, check := range []string{"if (-not $a)", "Get-Command ssh-keygen", "$m.Count -ne 1", "Get-FileHash"} {
			if i := strings.Index(body, check); i < 0 || !strings.Contains(line(i), "throw") {
				t.Fatalf("%s: no %q that throws:\n%s", doc, check, block)
			}
		}
		if l := strings.SplitN(after, "\n", 3); !strings.Contains(l[0], "throw") || !strings.Contains(l[1], "throw") || !strings.Contains(l[0], "8.1") {
			t.Fatalf("%s: the signature check must throw for an old ssh-keygen, then for a mismatch:\n%s", doc, block)
		}
		throws := regexp.MustCompile(`throw "([^"$]*)`).FindAllStringSubmatch(body, -1)
		seen := map[string]bool{}
		for _, m := range throws {
			if seen[m[1]] {
				t.Fatalf("%s: two checks throw %q", doc, m[1])
			}
			seen[m[1]] = true
		}
		hash := strings.Index(body, "Get-FileHash")
		for _, use := range []string{".exe setup", "court-gateway.exe", "Unblock-File"} {
			if i := strings.Index(body, use); i >= 0 && i < hash {
				t.Fatalf("%s: %q comes before the hash check:\n%s", doc, use, block)
			}
		}
		if doc == "AGENTS.md" && !strings.Contains(line(strings.Index(body, "Invoke-WebRequest")), `throw "DOWNLOAD FAILED`) {
			t.Fatalf("%s: a failed download must throw its own message:\n%s", doc, block)
		}
		pattern := regexp.MustCompile(`-Pattern "([^"]+)"`).FindStringSubmatch(body)
		if pattern == nil {
			t.Fatalf("%s: no Select-String pattern", doc)
		}
		for _, c := range []struct {
			arch   string
			extra  string
			builds []string
		}{
			{"amd64", "", []string{"court-gateway-1.0.0-windows-amd64.exe"}},
			{"arm64", "", []string{"court-gateway-1.0.0-windows-arm64.exe"}},
			{"amd64", "  court-gateway-0.9.0-windows-amd64.exe\n", []string{"court-gateway-1.0.0-windows-amd64.exe", "court-gateway-0.9.0-windows-amd64.exe"}},
		} {
			re := regexp.MustCompile("(?i)" + strings.ReplaceAll(pattern[1], "$a", c.arch)) // Select-String ignores case
			var got []string
			sums := ""
			for _, p := range update.Platforms {
				sums += strings.Repeat("ab", 32) + "  court-gateway-1.0.0-" + p + map[bool]string{true: ".exe"}[strings.HasPrefix(p, "windows-")] + "\n"
			}
			if c.extra != "" {
				sums += strings.Repeat("cd", 32) + c.extra
			}
			for _, l := range strings.Split(strings.TrimSuffix(sums, "\n"), "\n") {
				if m := re.FindStringSubmatch(l); m != nil {
					got = append(got, m[2])
				}
			}
			if !slices.Equal(got, c.builds) {
				t.Fatalf("%s: windows-%s picks %v from SHA256SUMS, want %v", doc, c.arch, got, c.builds)
			}
		}
	}
}

// The release is reproducible only with one Go: go.mod pins it as the
// toolchain, and the Dockerfile and README name the same version.
func TestToolchainPinnedEverywhere(t *testing.T) {
	mod := readDoc(t, "go.mod")
	i := strings.Index(mod, "\ntoolchain go")
	if i < 0 {
		t.Fatal("go.mod has no toolchain line")
	}
	ver := strings.Fields(mod[i+len("\ntoolchain go"):])[0]
	if !strings.Contains(readDoc(t, "Dockerfile"), "FROM golang:"+ver+"-") || !strings.Contains(readDoc(t, "README.md"), "(сейчас "+ver+")") {
		t.Fatalf("toolchain go%s is not the Dockerfile's and README's Go", ver)
	}
}

// scripts/build.sh builds exactly the release platforms that manifests
// list (update.Platforms), Windows on ARM64 included.
func TestBuildTargetsAreTheReleasePlatforms(t *testing.T) {
	script := readDoc(t, "scripts/build.sh")
	i := strings.Index(script, `TARGETS="${TARGETS:-`)
	if i < 0 {
		t.Fatal("build.sh has no default TARGETS")
	}
	list := script[i+len(`TARGETS="${TARGETS:-`):]
	list = list[:strings.Index(list, "}")]
	got := strings.ReplaceAll(list, "/", "-")
	if got != strings.Join(update.Platforms, " ") || !slices.Contains(update.Platforms, "windows-arm64") {
		t.Fatalf("build.sh targets %q, release platforms %v", got, update.Platforms)
	}
}

// The doctor sample in README.md shows every check doctor makes.
func TestReadmeDoctorSampleShowsEveryCheck(t *testing.T) {
	sample := fencedBlock(t, "README.md", "## Проверка: `court-gateway doctor`", "")
	for _, code := range []string{doctor.CodeConfig, doctor.CodeSecretFile, doctor.CodeCRMLogin, doctor.CodeOccupancy,
		doctor.CodeHubReach, doctor.CodeHubAuth, doctor.CodeTunnel, doctor.CodeHubConfig, doctor.CodeVersion} {
		if !strings.Contains(sample, "] "+code+" ") {
			t.Errorf("README.md: the doctor sample has no %s", code)
		}
	}
}

// What the club or the operator must be told, and the docs keep saying.
func TestDocsKeepTheOperatorFacts(t *testing.T) {
	for _, c := range []struct{ doc, fact string }{
		{"README.md", "winget install Microsoft.OpenSSH.Beta"}, // ssh-keygen -Y needs OpenSSH 8.1, older on Windows 10
		{"AGENTS.md", "winget install Microsoft.OpenSSH.Beta"},
		{"README.md", "FileVault, после отключения питания"}, // after a power loss a Mac with FileVault waits for a login
		{"README.md", "`CLOCK_UNKNOWN`"},                     // a buffering proxy keeps the hub's time from the gateway
		{"docs/hub.md", "`504` (любой код)"},                 // a create answered 502/504 has an unknown outcome
		{"docs/hub.md", "`outdated=1` дольше суток"},         // a self-built gateway does not update itself
		{"README.md", "Образ выпуска пока не публикуется"},   // Docker is a self-build for now
		{"README.md", "вернёт версию образа"},                // updates live in the container: recreating drops them
	} {
		if !strings.Contains(readDoc(t, c.doc), c.fact) {
			t.Errorf("%s no longer says %q", c.doc, c.fact)
		}
	}
}
