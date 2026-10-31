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
	keepKey                          bool // run the docs with their own release key (the release is signed by another)
}

// fault is what is wrong with a stand-in release, or with the machine it
// is installed on.
type fault int

const (
	noFault          fault = iota
	wrongChecksum          // SHA256SUMS gives the file another sum
	notSigned              // SHA256SUMS is not the one the release key signed
	noSums                 // the release has no SHA256SUMS
	twoBuilds              // the signed SHA256SUMS names two builds for the platform
	noBuild                // the release has no file under the name SHA256SUMS gives
	oldKeygen              // ssh-keygen predates -Y (OpenSSH before 8.1)
	noKeygen               // there is no ssh-keygen
	otherPlatform          // a machine there is no build for
	staleSigner            // a read-only allowed_signers with another key where the docs work, and a release signed by that key
	signerUnwritable       // the docs' own allowed_signers cannot be written (an attacker's file there), release signed by its key
	manifestSwap           // a writer replaces SHA256SUMS and the build right after the signature check succeeds
	binarySwap             // a writer replaces the downloaded build right after its checksum succeeds (stays verified)
	dirIsLink              // the install folder is a symbolic link
)

var faults = []fault{wrongChecksum, notSigned, noSums, twoBuilds, noBuild, oldKeygen, noKeygen, otherPlatform,
	staleSigner, signerUnwritable, manifestSwap, dirIsLink}

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
	return newInstallWorldAt(t, release, f, "")
}

// newInstallWorldAt is newInstallWorld for docs that install into
// install (relative to the world's directory; "" is the directory
// itself), where staleSigner and dirIsLink spoil it.
func newInstallWorldAt(t *testing.T, release string, f fault, install string) *installWorld {
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
	for _, d := range []string{w.bin, w.fixtures, filepath.Join(w.dir, "home"), filepath.Join(w.dir, "tmp")} {
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
	// a build that was never verified, for the writers below
	unverified := "#!/bin/sh\necho \"UNVERIFIED $*\" >> '" + w.mark + "'\n"
	write(filepath.Join(w.dir, "unverified"), unverified, 0o755)
	badSum := sha256.Sum256([]byte(unverified))
	write(filepath.Join(w.dir, "unverified-sums"), hex.EncodeToString(badSum[:])+"  "+release+"\n", 0o644)
	// both checksum tools, whichever this machine has; binarySwap: then a
	// writer replaces the build under its release name in the working
	// directory, as soon as the check has succeeded
	check := map[bool]string{true: sum, false: sum + " -a 256"}[filepath.Base(sum) == "sha256sum"] + " \"$@\"\n"
	if f == binarySwap {
		check = check[:len(check)-1] + "; rc=$?\n[ $rc = 0 ] && cp '" + filepath.Join(w.dir, "unverified") + "' '" + release + "'\nexit $rc\n"
	} else {
		check = "exec " + check
	}
	write(filepath.Join(w.bin, "sha256sum"), "#!/bin/sh\n"+check, 0o755)
	write(filepath.Join(w.bin, "shasum"), "#!/bin/sh\n[ \"$1\" = -a ] && shift 2\n"+check, 0o755)
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
	default: // the real ssh-keygen, noting where it verifies; manifestSwap: then the writer
		hook := ""
		if f == manifestSwap {
			hook = "[ $rc = 0 ] && cp '" + filepath.Join(w.dir, "unverified-sums") + "' SHA256SUMS && cp '" + filepath.Join(w.dir, "unverified") + "' '" + filepath.Join(w.fixtures, release) + "'\n"
		}
		write(filepath.Join(w.bin, "ssh-keygen"), "#!/bin/sh\n[ \"$1\" = -Y ] && { pwd; ls -ld .; } >> '"+filepath.Join(w.dir, "keygen-at")+"'\n'"+keygen+"' \"$@\"\nrc=$?\n"+hook+"exit $rc\n", 0o755)
	}
	installDir := filepath.Join(w.dir, install)
	switch f {
	case staleSigner, signerUnwritable: // the docs keep their own key: the release is signed by the test key only
		w.keepKey = true
		stale := w.signer + "\n"
		if f == staleSigner {
			for _, d := range slices.Compact([]string{installDir, w.dir}) {
				_ = os.MkdirAll(d, 0o755)
				write(filepath.Join(d, "allowed_signers"), stale, 0o444)
			}
		} else { // mktemp hands out a folder whose allowed_signers cannot be written
			stage := filepath.Join(w.dir, "tmp", "stage")
			write(filepath.Join(w.bin, "mktemp"), "#!/bin/sh\nmkdir -m 700 '"+stage+"' && printf '%s' '"+stale+"' > '"+stage+"/allowed_signers' && chmod 444 '"+stage+"/allowed_signers' && echo '"+stage+"'\n", 0o755)
		}
	case dirIsLink:
		elsewhere := filepath.Join(w.dir, "elsewhere")
		_ = os.MkdirAll(elsewhere, 0o755)
		_ = os.MkdirAll(filepath.Dir(installDir), 0o755)
		if err := os.Symlink(elsewhere, installDir); err != nil {
			t.Fatal(err)
		}
	}
	w.env = []string{"PATH=" + w.bin + ":" + system, "HOME=" + filepath.Join(w.dir, "home"), "TMPDIR=" + filepath.Join(w.dir, "tmp"), "LC_ALL=C"}
	return w
}

// run runs script with shell (sh, bash or zsh without comment support, as
// zsh is interactively) in dir and reports the exit code and output.
func (w *installWorld) run(t *testing.T, shell, dir, script string) (int, string) {
	t.Helper()
	if !w.keepKey {
		script = strings.ReplaceAll(script, docSigner, w.signer)
	}
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
// other; with none they run it on every platform, after verifying in a
// private temporary folder that is gone afterwards, from an install
// folder closed to other users. A writer that replaces the downloaded
// build under its release name once its checksum passed changes nothing.
func TestPosixInstallBlocksSayWhatIsWrong(t *testing.T) {
	for _, d := range []struct {
		doc, marker, install string
		msgs                 map[fault]string
		then, want           string // appended to the block; what the program then runs
		passed               string // printed once both checks passed, then the build's name
	}{
		{"README.md", "### macOS и Linux", "home/court-gateway", map[fault]string{
			wrongChecksum: "контрольная сумма court-gateway-9.9.9-", notSigned: "подпись SHA256SUMS не сошлась",
			noSums: "не удалось скачать SHA256SUMS", twoBuilds: "в SHA256SUMS не одна сборка",
			noBuild: "не удалось скачать https://", oldKeygen: "ssh-keygen старше OpenSSH 8.1",
			noKeygen: "нет ssh-keygen", otherPlatform: "выпуска нет", staleSigner: "подпись SHA256SUMS не сошлась",
			signerUnwritable: "не удалось записать ключ выпуска", manifestSwap: "контрольная сумма court-gateway-9.9.9-",
			dirIsLink: "не ваша или это ссылка",
		}, "", "setup\ninstall-service\ndoctor\n", "Подпись и сумма сошлись: "},
		{"AGENTS.md", "\nLinux and macOS:\n", "", map[fault]string{
			wrongChecksum: "CHECKSUM MISMATCH", notSigned: "SIGNATURE MISMATCH", noSums: "DOWNLOAD FAILED: SHA256SUMS",
			twoBuilds: "NOT ONE BUILD", noBuild: "DOWNLOAD FAILED: court-gateway-", oldKeygen: "SSH-KEYGEN TOO OLD",
			noKeygen: "NO SSH-KEYGEN", otherPlatform: "UNSUPPORTED PLATFORM", staleSigner: "SIGNATURE MISMATCH",
			signerUnwritable: "CANNOT WRITE THE RELEASE KEY", manifestSwap: "CHECKSUM MISMATCH",
		}, "[ -x court-gateway ] && ./court-gateway version\n", "version\n", "checksum-ok "},
	} {
		block := fencedBlock(t, d.doc, d.marker, "sh")
		if regexp.MustCompile(`<\pL+>`).MatchString(block) {
			t.Fatalf("%s: the install block has a placeholder to fill in:\n%s", d.doc, block)
		}
		said := func(out string) []string { // the messages of the faults out shows
			var got []string
			for _, msg := range d.msgs {
				if strings.Contains(out, msg) && !slices.Contains(got, msg) {
					got = append(got, msg)
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
					if d.msgs[f] == "" {
						continue
					}
					w := newInstallWorldAt(t, "court-gateway-9.9.9-linux-amd64", f, d.install)
					rc, out := w.run(t, shell, w.dir, block+d.then)
					if rc == 0 || w.ran() != "" || strings.Contains(out, d.passed) || !slices.Equal(said(out), []string{d.msgs[f]}) {
						t.Errorf("fault %d (%q): exit %d, program ran %q, messages %q\n%s", f, d.msgs[f], rc, w.ran(), said(out), out)
					}
				}
				for _, c := range []struct {
					platform string
					f        fault
				}{{"linux-amd64", noFault}, {"linux-arm64", noFault}, {"darwin-arm64", noFault}, {"darwin-amd64", noFault}, {"linux-amd64", binarySwap}} {
					w := newInstallWorldAt(t, "court-gateway-9.9.9-"+c.platform, c.f, d.install)
					rc, out := w.run(t, shell, w.dir, block+d.then)
					if rc != 0 || w.ran() != d.want || len(said(out)) != 0 || !strings.Contains(out, d.passed+"court-gateway-9.9.9-"+c.platform+"\n") {
						t.Errorf("%s fault %d: exit %d, program ran %q\n%s", c.platform, c.f, rc, w.ran(), out)
						continue
					}
					at, _ := os.ReadFile(filepath.Join(w.dir, "keygen-at"))
					where := strings.SplitN(string(at), "\n", 2)
					install := filepath.Join(w.dir, d.install)
					mode := os.FileMode(0)
					if fi, err := os.Stat(install); err == nil {
						mode = fi.Mode().Perm()
					}
					if len(where) != 2 || !strings.HasPrefix(where[1], "drwx------") || where[0] == install || mode != 0o700 {
						t.Errorf("%s: verified in %q, install folder mode %v; want a private temporary folder and a closed install folder", c.platform, at, mode)
					} else if _, err := os.Stat(where[0]); err == nil {
						t.Errorf("%s: the temporary folder %s is left behind", c.platform, where[0])
					}
					if _, err := os.Stat(filepath.Join(install, "court-gateway")); err != nil {
						t.Errorf("%s: no court-gateway in the install folder: %v", c.platform, err)
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
// line) with nothing to fill in, whose steps come in this order with
// these very predicates: the platform; TLS 1.2 (Windows PowerShell 5.1
// does not offer it by default); C:\court-gateway created, or, if it is
// there, refused when it is a link or holds foreign files while open to
// others; then closed to everyone but Administrators and SYSTEM (by SID,
// as setup closes it) and checked, before anything is downloaded;
// SHA256SUMS and its signature downloaded to fresh files (README: or
// copied from the folder, a .txt the browser added included); the
// release key written to a fresh file; the signature; the build's name
// from the verified SHA256SUMS; the build; its hash; only then the build
// put in place and run. Every cmdlet that touches a file stops on error
// and takes its path literally. The patterns that pick the build and
// that tell release files from foreign ones are run here.
func TestWindowsInstallBlocksCheckBeforeRunning(t *testing.T) {
	steps := []string{
		"[Net.ServicePointManager]::SecurityProtocol = 'Tls12'",
		"if (-not $a) { throw \"",
		"$acl.AreAccessRulesProtected -and $acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -in $ok -and",
		"| Where-Object { $_.AccessControlType -ne 'Allow' -or $_.IdentityReference.Value -notin $ok[0..1] })",
		"if (Test-Path -LiteralPath $d) {",
		"-band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw \"",
		"if ($extra -and -not (& $closed)) { throw \"",
		"} else { New-Item -ItemType Directory -Path $d -ErrorAction Stop | Out-Null }",
		"icacls $d /inheritance:r /grant:r '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-18:(OI)(CI)F' | Out-Null\n",
		"if ($LASTEXITCODE -ne 0 -or -not (& $closed)) { throw \"",
		"Set-Location -LiteralPath $d -ErrorAction Stop\n",
		"$drop = { param($p) if (Test-Path -LiteralPath $p) { Remove-Item -LiteralPath $p -Force -ErrorAction Stop } }",
		"& $drop \"$n.part\"\n",
		"try { Invoke-WebRequest \"$b/$n\" -OutFile \"$n.part\" -UseBasicParsing -ErrorAction Stop } catch {",
		"Move-Item -LiteralPath \"$n.part\" -Destination $n -Force -ErrorAction Stop\n",
		"& $get SHA256SUMS\n",
		"& $get SHA256SUMS.sig\n",
		"if (-not (Get-Command ssh-keygen -ErrorAction SilentlyContinue)) { throw \"",
		"& $drop allowed_signers\n",
		"' | Set-Content -LiteralPath allowed_signers -Encoding ascii -ErrorAction Stop\n",
		"$out = cmd /c 'ssh-keygen -Y verify -f allowed_signers -I court-gateway -n court-gateway -s SHA256SUMS.sig < SHA256SUMS 2>&1'\n",
		"if ($LASTEXITCODE -ne 0 -and \"$out\" -match 'unknown option|illegal option|usage:') { throw \"",
		"if ($LASTEXITCODE -ne 0) { throw \"",
		"$m = @(Select-String -LiteralPath SHA256SUMS -Pattern \"",
		"if ($m.Count -ne 1) { throw \"",
		"$want, $f = $m[0].Matches[0].Groups[1].Value, $m[0].Matches[0].Groups[2].Value\n",
		"& $get $f\n",
		"if ((Get-FileHash -LiteralPath $f -Algorithm SHA256 -ErrorAction Stop).Hash -ne $want) { throw \"",
		"try { Move-Item -LiteralPath $f -Destination court-gateway.exe -Force -ErrorAction Stop } catch { throw \"",
	}
	for doc, c := range map[string]struct{ marker, fallback, last string }{
		"README.md": {"### Windows", "Copy-Item -LiteralPath $src -Destination \"$n.part\" -ErrorAction Stop\n      Write-Host \"Не удалось скачать $n ($e) — ", "Write-Host \"Подпись и сумма сошлись: $f\"\n  .\\court-gateway.exe setup --install-service\n"},
		"AGENTS.md": {"\nWindows: run in PowerShell", "catch { throw \"DOWNLOAD FAILED: $n", "\"checksum-ok $f\"\n"},
	} {
		block := strings.TrimSpace(fencedBlock(t, doc, c.marker, "powershell"))
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
		at := -1
		for _, step := range append(slices.Insert(slices.Clone(steps), 14, c.fallback), c.last) {
			i := strings.Index(body[at+1:], step)
			if i < 0 {
				t.Fatalf("%s: no %q after the step before it:\n%s", doc, step, block)
			}
			at += 1 + i
		}
		if i := strings.Index(body, "court-gateway.exe setup"); i >= 0 && i < strings.Index(body, "try { Move-Item -LiteralPath $f") {
			t.Fatalf("%s: setup runs before the build is put in place:\n%s", doc, block)
		}
		// every cmdlet that touches a file: -ErrorAction Stop, and -LiteralPath where it takes a path
		unquoted := regexp.MustCompile(`"[^"]*"`).ReplaceAllString(body, `""`)
		for _, m := range regexp.MustCompile(`\b(New-Item|Set-Location|Set-Content|Get-FileHash|Move-Item|Copy-Item|Remove-Item|Get-Acl|Get-ChildItem|Get-Item|Invoke-WebRequest|Select-String)\b[^|})\n]*`).FindAllStringSubmatch(unquoted, -1) {
			if !strings.Contains(m[0], "-ErrorAction Stop") || (m[1] != "New-Item" && m[1] != "Invoke-WebRequest" && !strings.Contains(m[0], "-LiteralPath")) {
				t.Fatalf("%s: %q must stop on error and take its path literally", doc, m[0])
			}
		}
		seen := map[string]bool{}
		for _, m := range regexp.MustCompile(`throw "([^"$]*)`).FindAllStringSubmatch(body, -1) {
			if seen[m[1]] {
				t.Fatalf("%s: two checks throw %q", doc, m[1])
			}
			seen[m[1]] = true
		}
		// the build is picked from SHA256SUMS of every release platform, by a strict name
		pattern := regexp.MustCompile(`-Pattern "([^"]+)"`).FindStringSubmatch(body)
		if pattern == nil {
			t.Fatalf("%s: no Select-String pattern", doc)
		}
		for _, c := range []struct {
			arch   string
			extra  []string
			builds []string
		}{
			{"amd64", nil, []string{"court-gateway-1.0.0-windows-amd64.exe"}},
			{"arm64", nil, []string{"court-gateway-1.0.0-windows-arm64.exe"}},
			{"amd64", []string{"court-gateway-0.9.0-windows-amd64.exe"}, []string{"court-gateway-1.0.0-windows-amd64.exe", "court-gateway-0.9.0-windows-amd64.exe"}},
			{"amd64", []string{"court-gateway-1[0]-windows-amd64.exe", "court-gateway-1*-windows-amd64.exe", `court-gateway-1\..\x-windows-amd64.exe`, "court-gateway-1/x-windows-amd64.exe", "court-gateway-1 x-windows-amd64.exe"},
				[]string{"court-gateway-1.0.0-windows-amd64.exe"}},
		} {
			re := regexp.MustCompile("(?i)" + strings.ReplaceAll(pattern[1], "$a", c.arch)) // Select-String ignores case
			lines := []string{}
			for _, p := range update.Platforms {
				lines = append(lines, strings.Repeat("ab", 32)+"  court-gateway-1.0.0-"+p+map[bool]string{true: ".exe"}[strings.HasPrefix(p, "windows-")])
			}
			for _, e := range c.extra {
				lines = append(lines, strings.Repeat("cd", 32)+"  "+e)
			}
			var got []string
			for _, l := range lines {
				if m := re.FindStringSubmatch(l); m != nil {
					got = append(got, m[2])
				}
			}
			if !slices.Equal(got, c.builds) {
				t.Fatalf("%s: windows-%s picks %q from SHA256SUMS, want %q", doc, c.arch, got, c.builds)
			}
		}
		// what may lie in a folder still open to others: the release files and leftovers of a run, nothing else
		release := regexp.MustCompile(`-notmatch '([^']+)'`).FindStringSubmatch(body)
		if release == nil {
			t.Fatalf("%s: no pattern of the files a folder open to others may hold", doc)
		}
		re := regexp.MustCompile("(?i)" + release[1]) // -notmatch ignores case
		for _, name := range []string{"SHA256SUMS", "SHA256SUMS.txt", "SHA256SUMS.sig", "SHA256SUMS.sig.txt", "SHA256SUMS.part", "allowed_signers",
			"court-gateway-1.0.0-windows-amd64.exe", "court-gateway-1.0.0-windows-arm64.exe.part"} {
			if !re.MatchString(name) {
				t.Errorf("%s: a release file %q counts as foreign", doc, name)
			}
		}
		for _, name := range []string{"court-gateway.exe", "gateway.yaml", ".env", "state", "version.dll", "court-gateway-1[0]-windows-amd64.exe",
			"court-gateway-1.0.0-windows-amd64.exe.dll", "xSHA256SUMS", "allowed_signers.d"} {
			if re.MatchString(name) {
				t.Errorf("%s: %q counts as a release file", doc, name)
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
		{"README.md", "-ArgumentList \"/i $m ADDLOCAL=Client /qn\""}, // ssh-keygen -Y needs OpenSSH 8.1, older on Windows 10; the MSI alone also installs sshd
		{"AGENTS.md", "`ADDLOCAL=Client`"},
		{"README.md", "не нажимайте «Продолжить» в его вопросе о доступе"}, // «Continue» in Explorer opens the folder holding .env
		{"README.md", "FileVault, после отключения питания"},               // after a power loss a Mac with FileVault waits for a login
		{"README.md", "`CLOCK_UNKNOWN`"},                                   // a buffering proxy keeps the hub's time from the gateway
		{"docs/hub.md", "`504` (любой код)"},                               // a create answered 502/504 has an unknown outcome
		{"docs/hub.md", "`outdated=1` дольше суток"},                       // a self-built gateway does not update itself
		{"README.md", "Образ выпуска пока не публикуется"},                 // Docker is a self-build for now
		{"README.md", "переживает пересоздание контейнера"},                // the state is a volume: rejected CRM logins stay counted
	} {
		if !strings.Contains(readDoc(t, c.doc), c.fact) {
			t.Errorf("%s no longer says %q", c.doc, c.fact)
		}
	}
}
