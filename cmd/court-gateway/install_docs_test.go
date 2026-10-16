package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
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
// its arguments) and stand-ins for the network and for sudo; only the
// release key they publish is swapped for a test key. With a wrong
// checksum, or a SHA256SUMS the release key did not sign, the program must
// never run; with both right it must.

// docSigner is the release key the install docs publish (allowed_signers).
const docSigner = "court-gateway ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJAe4CDRNNnk++UtmLOdDcQcRc0jK56eYJPo+DfdyK8y"

// The install docs publish one release key.
func TestInstallDocsPublishOneReleaseKey(t *testing.T) {
	for _, doc := range []string{"README.md", "AGENTS.md", "deploy/cloud-init.yaml"} {
		text := readDoc(t, doc)
		if n := strings.Count(text, "ssh-ed25519 "); n == 0 || n != strings.Count(text, docSigner) {
			t.Errorf("%s: %d release keys, %d of them %q", doc, n, strings.Count(text, docSigner), docSigner)
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

// newInstallWorld puts a stand-in release file named release, a
// SHA256SUMS with its sum (or a wrong one) and SHA256SUMS.sig, the
// signature of it by a test release key (or of another SHA256SUMS), where
// the stand-in curl finds them; sudo, runuser and the system tools are
// stand-ins too. w.signer is the test key's allowed_signers line.
func newInstallWorld(t *testing.T, release string, rightSum, signed bool) *installWorld {
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
	if !rightSum {
		hexSum = strings.Repeat("0", 64)
	}
	write := func(path, data string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(w.fixtures, release), program, 0o644)
	sums := filepath.Join(w.fixtures, "SHA256SUMS")
	write(sums, hexSum+"  "+release+"\n"+strings.Repeat("1", 64)+"  court-gateway-9.9.9-other\n", 0o644)
	key := filepath.Join(w.dir, "release-key")
	for _, args := range [][]string{{"-q", "-t", "ed25519", "-N", "", "-C", "", "-f", key}, {"-q", "-Y", "sign", "-f", key, "-n", "court-gateway", sums}} {
		if out, err := exec.Command(keygen, args...).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen %v: %v\n%s", args, err, out)
		}
	}
	pub, _ := os.ReadFile(key + ".pub")
	w.signer = "court-gateway " + strings.TrimSpace(string(pub))
	if !signed { // a SHA256SUMS that matches the file, but not the signature
		write(sums, strings.Repeat("2", 64)+"  "+release+"\n", 0o644)
		if rightSum {
			write(sums, hexSum+"  "+release+"\n", 0o644)
		}
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
	write(filepath.Join(w.bin, "sudo"), "#!/bin/sh\nexec \"$@\"\n", 0o755)
	write(filepath.Join(w.bin, "runuser"), "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done; shift; exec \"$@\"\n", 0o755)
	for _, noop := range []string{"useradd", "chown", "sleep", "id"} {
		write(filepath.Join(w.bin, noop), "#!/bin/sh\nexit 0\n", 0o755)
	}
	write(filepath.Join(w.bin, "dpkg"), "#!/bin/sh\necho amd64\n", 0o755)
	// both checksum tools, whichever this machine has
	write(filepath.Join(w.bin, "sha256sum"), "#!/bin/sh\nexec "+map[bool]string{true: sum, false: sum + " -a 256"}[filepath.Base(sum) == "sha256sum"]+" \"$@\"\n", 0o755)
	write(filepath.Join(w.bin, "shasum"), "#!/bin/sh\n[ \"$1\" = -a ] && shift 2\nexec "+map[bool]string{true: sum, false: sum + " -a 256"}[filepath.Base(sum) == "sha256sum"]+" \"$@\"\n", 0o755)
	w.env = []string{"PATH=" + w.bin + ":/usr/bin:/bin", "HOME=" + filepath.Join(w.dir, "home"), "LC_ALL=C"}
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

func TestReadmeInstallStopsOnChecksumMismatch(t *testing.T) {
	block := fencedBlock(t, "README.md", "### macOS и Linux", "sh")
	for _, c := range []struct{ name, release string }{
		{"macOS", "court-gateway-9.9.9-darwin-arm64"},
		{"Linux", "court-gateway-9.9.9-linux-amd64"},
	} {
		script := "F=" + c.release + "\n" + block[strings.Index(block, "\n")+1:]
		for _, shell := range []string{"sh", "bash", "zsh"} {
			if _, err := exec.LookPath(shell); err != nil {
				continue
			}
			t.Run(c.name+"/"+shell, func(t *testing.T) {
				for name, bad := range map[string]*installWorld{"wrong checksum": newInstallWorld(t, c.release, false, true),
					"not signed": newInstallWorld(t, c.release, true, false)} {
					if rc, out := bad.run(t, shell, bad.dir, script); rc == 0 || bad.ran() != "" {
						t.Fatalf("%s: exit %d, program ran %q\n%s", name, rc, bad.ran(), out)
					}
				}
				good := newInstallWorld(t, c.release, true, true)
				rc, out := good.run(t, shell, good.dir, script)
				if rc != 0 || good.ran() != "setup\ninstall-service\ndoctor\n" {
					t.Fatalf("right checksum: exit %d, program ran %q\n%s", rc, good.ran(), out)
				}
			})
		}
	}
}

func TestAgentsVerifyStepStopsOnChecksumMismatch(t *testing.T) {
	block := fencedBlock(t, "AGENTS.md", "\nLinux and macOS:\n", "sh")
	for _, c := range []struct{ os, release string }{
		{"Linux", "court-gateway-9.9.9-linux-amd64"},
		{"macOS", "court-gateway-9.9.9-darwin-arm64"},
	} {
		script := "F=" + c.release + "\n" + block[strings.Index(block, "\n")+1:]
		for _, right := range []bool{false, true} {
			w := newInstallWorld(t, c.release, right, right) // the block downloads the files itself
			rc, out := w.run(t, "sh", w.dir, script+"[ -x court-gateway ] && ./court-gateway version\n")
			ok := strings.Contains(out, "checksum-ok")
			if right != ok || right != (w.ran() == "version\n") || right != (rc == 0) {
				t.Fatalf("%s right checksum=%v: exit %d, program ran %q\n%s", c.os, right, rc, w.ran(), out)
			}
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
		name          string
		right, signed bool
		secrets       string
		wantRC        int
		wantRan       string
	}{
		{"wrong checksum", false, true, "CRM_PASSWORD='x'\nGATEWAY_JOIN_CODE='y'\n", 1, ""},
		{"not signed", true, false, "CRM_PASSWORD='x'\nGATEWAY_JOIN_CODE='y'\n", 1, ""},
		{"no secrets: setup over SSH", true, true, doc.WriteFiles[0].Content, 0, ""},
		{"secrets given", true, true, "CRM_PASSWORD='x'\nGATEWAY_JOIN_CODE='y'\n", 0, "setup --non-interactive --json --config DIR/gateway.yaml\ninstall-service -config DIR/gateway.yaml --run-as court-gateway\ndoctor -config DIR/gateway.yaml\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newInstallWorld(t, "court-gateway-9.9.9-linux-amd64", c.right, c.signed)
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
// line), and the hash comparison throws before anything renames or runs
// the file.
func TestWindowsInstallBlocksCheckTheHashFirst(t *testing.T) {
	for doc, marker := range map[string]string{"README.md": "### Windows", "AGENTS.md": "\nWindows: first run"} {
		block := strings.TrimSpace(fencedBlock(t, doc, marker, "powershell"))
		body := block[strings.Index(block, "& {"):]
		sig, check := strings.Index(body, "ssh-keygen -Y verify"), strings.Index(body, "Get-FileHash")
		throws := func(i int) bool { // the line after i throws on failure
			next := body[i:][strings.Index(body[i:], "\n")+1:]
			return strings.Contains(next[:strings.Index(next, "\n")], "throw")
		}
		if !strings.HasPrefix(body, "& {") || !strings.HasSuffix(body, "}") || sig < 0 || check < sig || !throws(sig) ||
			!strings.Contains(body[check:strings.Index(body[check:], "\n")+check], "throw") || !strings.Contains(body, docSigner) {
			t.Fatalf("%s: the signature check, then the hash check, must throw inside one script block:\n%s", doc, block)
		}
		if doc == "AGENTS.md" && strings.Count(body[:sig], "Invoke-WebRequest") != 3 {
			t.Fatalf("%s: the files must be downloaded before the checks:\n%s", doc, block)
		}
		for _, use := range []string{"Move-Item", ".exe setup", "court-gateway.exe"} {
			if i := strings.Index(body, use); i >= 0 && i < check {
				t.Fatalf("%s: %q comes before the hash check:\n%s", doc, use, block)
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
