// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux

package vms

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	expect "github.com/google/goexpect"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"
	"golang.org/x/sync/semaphore"
	"inet.af/netaddr"
	"tailscale.com/net/interfaces"
	"tailscale.com/tstest"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
)

const (
	securePassword = "hunter2"
	bucketName     = "tailscale-integration-vm-images"
)

var (
	runVMTests = flag.Bool("run-vm-tests", false, "if set, run expensive VM based integration tests")
	noS3       = flag.Bool("no-s3", false, "if set, always download images from the public internet (risks breaking)")
	vmRamLimit = flag.Int("ram-limit", 4096, "the maximum number of megabytes of ram that can be used for VMs, must be greater than or equal to 1024")
	useVNC     = flag.Bool("use-vnc", false, "if set, display guest vms over VNC")
	distroRex  = func() *regexValue {
		result := &regexValue{r: regexp.MustCompile(`.*`)}
		flag.Var(result, "distro-regex", "The regex that matches what distros should be run")
		return result
	}()
)

type Harness struct {
	testerDialer   proxy.Dialer
	testerDir      string
	bins           *integration.Binaries
	signer         ssh.Signer
	cs             *testcontrol.Server
	loginServerURL string
}

type Distro struct {
	name           string // amazon-linux
	url            string // URL to a qcow2 image
	sha256sum      string // hex-encoded sha256 sum of contents of URL
	mem            int    // VM memory in megabytes
	packageManager string // yum/apt/dnf/zypper
	initSystem     string // systemd/openrc
}

func (d *Distro) InstallPre() string {
	switch d.packageManager {
	case "yum":
		return ` - [ yum, update, gnupg2 ]
 - [ yum, "-y", install, iptables ]`
	case "zypper":
		return ` - [ zypper, in, "-y", iptables ]`

	case "dnf":
		return ` - [ dnf, install, "-y", iptables ]`

	case "apt":
		return ` - [ apt-get, update ]
 - [ apt-get, "-y", install, curl, "apt-transport-https", gnupg2 ]`

	case "apk":
		return ` - [ apk, "-U", add, curl, "ca-certificates", iptables ]
 - [ modprobe, tun ]`
	}

	return ""
}

func TestDownloadImages(t *testing.T) {
	if !*runVMTests {
		t.Skip("not running integration tests (need --run-vm-tests)")
	}

	bins := integration.BuildTestBinaries(t)

	for _, d := range distros {
		distro := d
		t.Run(distro.name, func(t *testing.T) {
			if !distroRex.Unwrap().MatchString(distro.name) {
				t.Skipf("distro name %q doesn't match regex: %s", distro.name, distroRex)
			}

			if strings.HasPrefix(distro.name, "nixos") {
				t.Skip("NixOS is built on the fly, no need to download it")
			}

			t.Parallel()

			(Harness{bins: bins}).fetchDistro(t, distro)
		})
	}
}

var distros = []Distro{
	// NOTE(Xe): If you run into issues getting the autoconfig to work, run
	// this test with the flag `--distro-regex=alpine-edge`. Connect with a VNC
	// client with a command like this:
	//
	//    $ vncviewer :0
	//
	// On NixOS you can get away with something like this:
	//
	//    $ env NIXPKGS_ALLOW_UNFREE=1 nix-shell -p tigervnc --run 'vncviewer :0'
	//
	// Login as root with the password root. Then look in
	// /var/log/cloud-init-output.log for what you messed up.

	// NOTE(Xe): These images are not official images created by the Alpine Linux
	// cloud team because the cloud team hasn't created any official images yet.
	// These images were created under the guidance of the cloud team and contain
	// few notable differences from what they would end up shipping. The Alpine
	// Linux cloud team probably won't have official images up until a year or so
	// after this comment is written (2021-06-11), but overall they will be
	// compatible with these images. These images were created using the setup in
	// this repo: https://github.com/Xe/alpine-image. I hereby promise to not break
	// these links.
	{"alpine-3-13-5", "https://xena.greedo.xeserv.us/pkg/alpine/img/alpine-3.13.5-cloud-init-within.qcow2", "a2665c16724e75899723e81d81126bd0254a876e5de286b0b21553734baec287", 256, "apk", "openrc"},
	{"alpine-edge", "https://xena.greedo.xeserv.us/pkg/alpine/img/alpine-edge-2021-05-18-cloud-init-within.qcow2", "b3bb15311c0bd3beffa1b554f022b75d3b7309b5fdf76fb146fe7c72b83b16d0", 256, "apk", "openrc"},

	// NOTE(Xe): All of the following images are official images straight from each
	// distribution's official documentation.
	{"amazon-linux", "https://cdn.amazonlinux.com/os-images/2.0.20210427.0/kvm/amzn2-kvm-2.0.20210427.0-x86_64.xfs.gpt.qcow2", "6ef9daef32cec69b2d0088626ec96410cd24afc504d57278bbf2f2ba2b7e529b", 512, "yum", "systemd"},
	{"arch", "https://mirror.pkgbuild.com/images/v20210515.22945/Arch-Linux-x86_64-cloudimg-20210515.22945.qcow2", "e4077f5ba3c5d545478f64834bc4852f9f7a2e05950fce8ecd0df84193162a27", 512, "pacman", "systemd"},
	{"centos-7", "https://cloud.centos.org/centos/7/images/CentOS-7-x86_64-GenericCloud-2003.qcow2c", "b7555ecf90b24111f2efbc03c1e80f7b38f1e1fc7e1b15d8fee277d1a4575e87", 512, "yum", "systemd"},
	{"centos-8", "https://cloud.centos.org/centos/8/x86_64/images/CentOS-8-GenericCloud-8.3.2011-20201204.2.x86_64.qcow2", "7ec97062618dc0a7ebf211864abf63629da1f325578868579ee70c495bed3ba0", 768, "dnf", "systemd"},
	{"debian-9", "http://cloud.debian.org/images/cloud/OpenStack/9.13.22-20210531/debian-9.13.22-20210531-openstack-amd64.qcow2", "c36e25f2ab0b5be722180db42ed9928476812f02d053620e1c287f983e9f6f1d", 512, "apt", "systemd"},
	{"debian-10", "https://cdimage.debian.org/images/cloud/buster/20210329-591/debian-10-generic-amd64-20210329-591.qcow2", "70c61956095870c4082103d1a7a1cb5925293f8405fc6cb348588ec97e8611b0", 768, "apt", "systemd"},
	{"fedora-34", "https://download.fedoraproject.org/pub/fedora/linux/releases/34/Cloud/x86_64/images/Fedora-Cloud-Base-34-1.2.x86_64.qcow2", "b9b621b26725ba95442d9a56cbaa054784e0779a9522ec6eafff07c6e6f717ea", 768, "dnf", "systemd"},
	{"opensuse-leap-15-1", "https://download.opensuse.org/repositories/Cloud:/Images:/Leap_15.1/images/openSUSE-Leap-15.1-OpenStack.x86_64.qcow2", "40bc72b8ee143364fc401f2c9c9a11ecb7341a29fa84c6f7bf42fc94acf19a02", 512, "zypper", "systemd"},
	{"opensuse-leap-15-2", "https://download.opensuse.org/repositories/Cloud:/Images:/Leap_15.2/images/openSUSE-Leap-15.2-OpenStack.x86_64.qcow2", "4df9cee9281d1f57d20f79dc65d76e255592b904760e73c0dd44ac753a54330f", 512, "zypper", "systemd"},
	{"opensuse-leap-15-3", "http://mirror.its.dal.ca/opensuse/distribution/leap/15.3/appliances/openSUSE-Leap-15.3-JeOS.x86_64-OpenStack-Cloud.qcow2", "22e0392e4d0becb523d1bc5f709366140b7ee20d6faf26de3d0f9046d1ee15d5", 512, "zypper", "systemd"},
	{"opensuse-tumbleweed", "https://download.opensuse.org/tumbleweed/appliances/openSUSE-Tumbleweed-JeOS.x86_64-OpenStack-Cloud.qcow2", "79e610bba3ed116556608f031c06e4b9260e3be2b193ce1727914ba213afac3f", 512, "zypper", "systemd"},
	{"oracle-linux-7", "https://yum.oracle.com/templates/OracleLinux/OL7/u9/x86_64/OL7U9_x86_64-olvm-b86.qcow2", "2ef4c10c0f6a0b17844742adc9ede7eb64a2c326e374068b7175f2ecbb1956fb", 512, "yum", "systemd"},
	{"oracle-linux-8", "https://yum.oracle.com/templates/OracleLinux/OL8/u4/x86_64/OL8U4_x86_64-olvm-b85.qcow2", "b86e1f1ea8fc904ed763a85ba12e9f12f4291c019c8435d0e4e6133392182b0b", 768, "dnf", "systemd"},
	{"ubuntu-16-04", "https://cloud-images.ubuntu.com/xenial/20210429/xenial-server-cloudimg-amd64-disk1.img", "50a21bc067c05e0c73bf5d8727ab61152340d93073b3dc32eff18b626f7d813b", 512, "apt", "systemd"},
	{"ubuntu-18-04", "https://cloud-images.ubuntu.com/bionic/20210526/bionic-server-cloudimg-amd64.img", "389ffd5d36bbc7a11bf384fd217cda9388ccae20e5b0cb7d4516733623c96022", 512, "apt", "systemd"},
	{"ubuntu-20-04", "https://cloud-images.ubuntu.com/focal/20210603/focal-server-cloudimg-amd64.img", "1c0969323b058ba8b91fec245527069c2f0502fc119b9138b213b6bfebd965cb", 512, "apt", "systemd"},
	{"ubuntu-20-10", "https://cloud-images.ubuntu.com/groovy/20210604/groovy-server-cloudimg-amd64.img", "2196df5f153faf96443e5502bfdbcaa0baaefbaec614348fec344a241855b0ef", 512, "apt", "systemd"},
	{"ubuntu-21-04", "https://cloud-images.ubuntu.com/hirsute/20210603/hirsute-server-cloudimg-amd64.img", "bf07f36fc99ff521d3426e7d257e28f0c81feebc9780b0c4f4e25ae594ff4d3b", 512, "apt", "systemd"},

	// NOTE(Xe): We build fresh NixOS images for every test run, so the URL being
	// used here is actually the URL of the NixOS channel being built from and the
	// shasum is meaningless. This `channel:name` syntax is documented at [1].
	//
	// [1]: https://nixos.org/manual/nix/unstable/command-ref/env-common.html
	{"nixos-21-05", "channel:nixos-21.05", "lolfakesha", 512, "nix", "systemd"},

	// // NOTE(Xe): disabled until https://github.com/NixOS/nixpkgs/issues/128783
	// // is fixed.
	// {"nixos-unstable", "channel:nixos-unstable", "lolfakesha", 512, "nix", "systemd"},
}

// fetchFromS3 fetches a distribution image from Amazon S3 or reports whether
// it is unable to. It can fail to fetch from S3 if there is either no AWS
// configuration (in ~/.aws/credentials) or if the `-no-s3` flag is passed. In
// that case the test will fall back to downloading distribution images from the
// public internet.
//
// Like fetching from HTTP, the test will fail if an error is encountered during
// the downloading process.
//
// This function writes the distribution image to fout. It is always closed. Do
// not expect fout to remain writable.
func fetchFromS3(t *testing.T, fout *os.File, d Distro) bool {
	t.Helper()

	if *noS3 {
		t.Log("you asked to not use S3, not using S3")
		return false
	}

	sess, err := session.NewSession(&aws.Config{
		Region: aws.String("us-east-1"),
	})
	if err != nil {
		t.Logf("can't make AWS session: %v", err)
		return false
	}

	dler := s3manager.NewDownloader(sess, func(d *s3manager.Downloader) {
		d.PartSize = 64 * 1024 * 1024 // 64MB per part
	})

	t.Logf("fetching s3://%s/%s", bucketName, d.sha256sum)

	_, err = dler.Download(fout, &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(d.sha256sum),
	})
	if err != nil {
		fout.Close()
		t.Fatalf("can't get s3://%s/%s: %v", bucketName, d.sha256sum, err)
	}

	err = fout.Close()
	if err != nil {
		t.Fatalf("can't close fout: %v", err)
	}

	return true
}

// fetchDistro fetches a distribution from the internet if it doesn't already exist locally. It
// also validates the sha256 sum from a known good hash.
func (h Harness) fetchDistro(t *testing.T, resultDistro Distro) string {
	t.Helper()

	cdir, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("can't find cache dir: %v", err)
	}
	cdir = filepath.Join(cdir, "tailscale", "vm-test")

	if strings.HasPrefix(resultDistro.name, "nixos") {
		return h.makeNixOSImage(t, resultDistro, cdir)
	}

	qcowPath := filepath.Join(cdir, "qcow2", resultDistro.sha256sum)

	_, err = os.Stat(qcowPath)
	if err == nil {
		hash := checkCachedImageHash(t, resultDistro, cdir)
		if hash != resultDistro.sha256sum {
			t.Logf("hash for %s (%s) doesn't match expected %s, re-downloading", resultDistro.name, qcowPath, resultDistro.sha256sum)
			err = errors.New("some fake non-nil error to force a redownload")

			if err := os.Remove(qcowPath); err != nil {
				t.Fatalf("can't delete wrong cached image: %v", err)
			}
		}
	}

	if err != nil {
		t.Logf("downloading distro image %s to %s", resultDistro.url, qcowPath)
		fout, err := os.Create(qcowPath)
		if err != nil {
			t.Fatal(err)
		}

		if !fetchFromS3(t, fout, resultDistro) {
			resp, err := http.Get(resultDistro.url)
			if err != nil {
				t.Fatalf("can't fetch qcow2 for %s (%s): %v", resultDistro.name, resultDistro.url, err)
			}

			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				t.Fatalf("%s replied %s", resultDistro.url, resp.Status)
			}

			_, err = io.Copy(fout, resp.Body)
			if err != nil {
				t.Fatalf("download of %s failed: %v", resultDistro.url, err)
			}

			resp.Body.Close()
			err = fout.Close()
			if err != nil {
				t.Fatalf("can't close fout: %v", err)
			}

			hash := checkCachedImageHash(t, resultDistro, cdir)

			if hash != resultDistro.sha256sum {
				t.Fatalf("hash mismatch, want: %s, got: %s", resultDistro.sha256sum, hash)
			}
		}
	}

	return qcowPath
}

func checkCachedImageHash(t *testing.T, d Distro, cacheDir string) (gotHash string) {
	t.Helper()

	qcowPath := filepath.Join(cacheDir, "qcow2", d.sha256sum)

	fin, err := os.Open(qcowPath)
	if err != nil {
		t.Fatal(err)
	}

	hasher := sha256.New()
	if _, err := io.Copy(hasher, fin); err != nil {
		t.Fatal(err)
	}
	hash := hex.EncodeToString(hasher.Sum(nil))

	if hash != d.sha256sum {
		t.Fatalf("hash mismatch, got: %q, want: %q", hash, d.sha256sum)
	}

	gotHash = hash

	return
}

// run runs a command or fails the test.
func run(t *testing.T, dir, prog string, args ...string) {
	t.Helper()
	t.Logf("running: %s %s", prog, strings.Join(args, " "))
	tstest.FixLogs(t)

	cmd := exec.Command(prog, args...)
	cmd.Stdout = logger.FuncWriter(t.Logf)
	cmd.Stderr = logger.FuncWriter(t.Logf)
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}

// mkLayeredQcow makes a layered qcow image that allows us to keep the upstream
// VM images pristine and only do our changes on an overlay.
func mkLayeredQcow(t *testing.T, tdir string, d Distro, qcowBase string) {
	t.Helper()

	run(t, tdir, "qemu-img", "create",
		"-f", "qcow2",
		"-o", "backing_file="+qcowBase,
		filepath.Join(tdir, d.name+".qcow2"),
	)
}

var (
	metaDataTempl = template.Must(template.New("meta-data.yaml").Parse(metaDataTemplate))
	userDataTempl = template.Must(template.New("user-data.yaml").Parse(userDataTemplate))
)

// mkSeed makes the cloud-init seed ISO that is used to configure a VM with
// tailscale.
func mkSeed(t *testing.T, d Distro, sshKey, hostURL, tdir string, port int) {
	t.Helper()

	dir := filepath.Join(tdir, d.name, "seed")
	os.MkdirAll(dir, 0700)

	// make meta-data
	{
		fout, err := os.Create(filepath.Join(dir, "meta-data"))
		if err != nil {
			t.Fatal(err)
		}

		err = metaDataTempl.Execute(fout, struct {
			ID       string
			Hostname string
		}{
			ID:       "31337",
			Hostname: d.name,
		})
		if err != nil {
			t.Fatal(err)
		}

		err = fout.Close()
		if err != nil {
			t.Fatal(err)
		}
	}

	// make user-data
	{
		fout, err := os.Create(filepath.Join(dir, "user-data"))
		if err != nil {
			t.Fatal(err)
		}

		err = userDataTempl.Execute(fout, struct {
			SSHKey     string
			HostURL    string
			Hostname   string
			Port       int
			InstallPre string
			Password   string
		}{
			SSHKey:     strings.TrimSpace(sshKey),
			HostURL:    hostURL,
			Hostname:   d.name,
			Port:       port,
			InstallPre: d.InstallPre(),
			Password:   securePassword,
		})
		if err != nil {
			t.Fatal(err)
		}

		err = fout.Close()
		if err != nil {
			t.Fatal(err)
		}
	}

	args := []string{
		"-output", filepath.Join(dir, "seed.iso"),
		"-volid", "cidata", "-joliet", "-rock",
		filepath.Join(dir, "meta-data"),
		filepath.Join(dir, "user-data"),
	}

	if hackOpenSUSE151UserData(t, d, dir) {
		args = append(args, filepath.Join(dir, "openstack"))
	}

	run(t, tdir, "genisoimage", args...)
}

// mkVM makes a KVM-accelerated virtual machine and prepares it for introduction
// to the testcontrol server. The function it returns is for killing the virtual
// machine when it is time for it to die.
func (h Harness) mkVM(t *testing.T, n int, d Distro, sshKey, hostURL, tdir string) {
	t.Helper()

	cdir, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("can't find cache dir: %v", err)
	}
	cdir = filepath.Join(cdir, "tailscale", "vm-test")
	os.MkdirAll(filepath.Join(cdir, "qcow2"), 0755)

	port, err := getProbablyFreePortNumber()
	if err != nil {
		t.Fatal(err)
	}

	mkLayeredQcow(t, tdir, d, h.fetchDistro(t, d))
	mkSeed(t, d, sshKey, hostURL, tdir, port)

	driveArg := fmt.Sprintf("file=%s,if=virtio", filepath.Join(tdir, d.name+".qcow2"))

	args := []string{
		"-machine", "pc-q35-5.1,accel=kvm,usb=off,vmport=off,dump-guest-core=off",
		"-netdev", fmt.Sprintf("user,hostfwd=::%d-:22,id=net0", port),
		"-device", "virtio-net-pci,netdev=net0,id=net0,mac=8a:28:5c:30:1f:25",
		"-m", fmt.Sprint(d.mem),
		"-boot", "c",
		"-drive", driveArg,
		"-cdrom", filepath.Join(tdir, d.name, "seed", "seed.iso"),
		"-smbios", "type=1,serial=ds=nocloud;h=" + d.name,
	}

	if *useVNC {
		// test listening on VNC port
		ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(5900+n)))
		if err != nil {
			t.Fatalf("would not be able to listen on the VNC port for the VM: %v", err)
		}
		ln.Close()
		args = append(args, "-vnc", fmt.Sprintf(":%d", n))
	} else {
		args = append(args, "-display", "none")
	}

	t.Logf("running: qemu-system-x86_64 %s", strings.Join(args, " "))

	cmd := exec.Command("qemu-system-x86_64", args...)
	cmd.Stdout = logger.FuncWriter(t.Logf)
	cmd.Stderr = logger.FuncWriter(t.Logf)
	err = cmd.Start()

	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Second)

	// NOTE(Xe): In Unix if you do a kill with signal number 0, the kernel will do
	// all of the access checking for the process (existence, permissions, etc) but
	// nothing else. This is a way to ensure that qemu's process is active.
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("qemu is not running: %v", err)
	}

	t.Cleanup(func() {
		err := cmd.Process.Kill()
		if err != nil {
			t.Errorf("can't kill %s (%d): %v", d.name, cmd.Process.Pid, err)
		}

		cmd.Wait()
	})
}

// ipMapping maps a hostname, SSH port and SSH IP together
type ipMapping struct {
	name string
	port int
	ip   string
}

// getProbablyFreePortNumber does what it says on the tin, but as a side effect
// it is a kind of racy function. Do not use this carelessly.
//
// This is racy because it does not "lock" the port number with the OS. The
// "random" port number that is returned here is most likely free to use, however
// it is difficult to be 100% sure. This function should be used with care. It
// will probably do what you want, but it is very easy to hold this wrong.
func getProbablyFreePortNumber() (int, error) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}

	defer l.Close()

	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		return 0, err
	}

	portNum, err := strconv.Atoi(port)
	if err != nil {
		return 0, err
	}

	return portNum, nil
}

// TestVMIntegrationEndToEnd creates a virtual machine with qemu, installs
// tailscale on it and then ensures that it connects to the network
// successfully.
func TestVMIntegrationEndToEnd(t *testing.T) {
	if !*runVMTests {
		t.Skip("not running integration tests (need --run-vm-tests)")
	}

	os.Setenv("CGO_ENABLED", "0")

	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		t.Logf("hint: nix-shell -p go -p qemu -p cdrkit --run 'go test --v --timeout=60m --run-vm-tests'")
		t.Fatalf("missing dependency: %v", err)
	}

	if _, err := exec.LookPath("genisoimage"); err != nil {
		t.Logf("hint: nix-shell -p go -p qemu -p cdrkit --run 'go test --v --timeout=60m --run-vm-tests'")
		t.Fatalf("missing dependency: %v", err)
	}

	dir := t.TempDir()

	rex := distroRex.Unwrap()

	bindHost := deriveBindhost(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(bindHost, "0"))
	if err != nil {
		t.Fatalf("can't make TCP listener: %v", err)
	}
	defer ln.Close()
	t.Logf("host:port: %s", ln.Addr())

	cs := &testcontrol.Server{}

	derpMap := integration.RunDERPAndSTUN(t, t.Logf, bindHost)
	cs.DERPMap = derpMap

	var (
		ipMu  sync.Mutex
		ipMap = map[string]ipMapping{}
	)

	mux := http.NewServeMux()
	mux.Handle("/", cs)
	mux.Handle("/c/", &integration.LogCatcher{})

	// This handler will let the virtual machines tell the host information about that VM.
	// This is used to maintain a list of port->IP address mappings that are known to be
	// working. This allows later steps to connect over SSH. This returns no response to
	// clients because no response is needed.
	mux.HandleFunc("/myip/", func(w http.ResponseWriter, r *http.Request) {
		ipMu.Lock()
		defer ipMu.Unlock()

		name := path.Base(r.URL.Path)
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		port, err := strconv.Atoi(name)
		if err != nil {
			log.Panicf("bad port: %v", port)
		}
		distro := r.UserAgent()
		ipMap[distro] = ipMapping{distro, port, host}
		t.Logf("%s: %v", name, host)
	})

	hs := &http.Server{Handler: mux}
	go hs.Serve(ln)

	run(t, dir, "ssh-keygen", "-t", "ed25519", "-f", "machinekey", "-N", ``)
	pubkey, err := os.ReadFile(filepath.Join(dir, "machinekey.pub"))
	if err != nil {
		t.Fatalf("can't read ssh key: %v", err)
	}

	privateKey, err := os.ReadFile(filepath.Join(dir, "machinekey"))
	if err != nil {
		t.Fatalf("can't read ssh private key: %v", err)
	}

	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		t.Fatalf("can't parse private key: %v", err)
	}

	loginServer := fmt.Sprintf("http://%s", ln.Addr())
	t.Logf("loginServer: %s", loginServer)

	ramsem := semaphore.NewWeighted(int64(*vmRamLimit))
	bins := integration.BuildTestBinaries(t)

	h := &Harness{
		bins:           bins,
		signer:         signer,
		loginServerURL: loginServer,
		cs:             cs,
	}

	h.makeTestNode(t, bins, loginServer)

	t.Run("do", func(t *testing.T) {
		for n, distro := range distros {
			n, distro := n, distro
			if rex.MatchString(distro.name) {
				t.Logf("%s matches %s", distro.name, rex)
			} else {
				continue
			}

			t.Run(distro.name, func(t *testing.T) {
				ctx, done := context.WithCancel(context.Background())
				t.Cleanup(done)

				t.Parallel()

				dir := t.TempDir()

				err := ramsem.Acquire(ctx, int64(distro.mem))
				if err != nil {
					t.Fatalf("can't acquire ram semaphore: %v", err)
				}
				defer ramsem.Release(int64(distro.mem))

				h.mkVM(t, n, distro, string(pubkey), loginServer, dir)
				var ipm ipMapping

				t.Run("wait-for-start", func(t *testing.T) {
					waiter := time.NewTicker(time.Second)
					defer waiter.Stop()
					var ok bool
					for {
						<-waiter.C
						ipMu.Lock()
						if ipm, ok = ipMap[distro.name]; ok {
							ipMu.Unlock()
							break
						}
						ipMu.Unlock()
					}
				})

				h.testDistro(t, distro, ipm)
			})
		}
	})
}

func (h Harness) testDistro(t *testing.T, d Distro, ipm ipMapping) {
	signer := h.signer
	loginServer := h.loginServerURL

	t.Helper()
	port := ipm.port
	hostport := fmt.Sprintf("127.0.0.1:%d", port)
	ccfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer), ssh.Password(securePassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// NOTE(Xe): This deadline loop helps to make things a bit faster, centos
	// sometimes is slow at starting its sshd and will sometimes randomly kill
	// SSH sessions on transition to multi-user.target. I don't know why they
	// don't use socket activation.
	const maxRetries = 5
	var working bool
	for i := 0; i < maxRetries; i++ {
		cli, err := ssh.Dial("tcp", hostport, ccfg)
		if err == nil {
			working = true
			cli.Close()
			break
		}

		time.Sleep(10 * time.Second)
	}

	if !working {
		t.Fatalf("can't connect to %s, tried %d times", hostport, maxRetries)
	}

	t.Logf("about to ssh into 127.0.0.1:%d", port)
	cli, err := ssh.Dial("tcp", hostport, ccfg)
	if err != nil {
		t.Fatal(err)
	}
	h.copyBinaries(t, d, cli)

	timeout := 30 * time.Second

	t.Run("start-tailscale", func(t *testing.T) {
		var batch = []expect.Batcher{
			&expect.BExp{R: `(\#)`},
		}

		switch d.initSystem {
		case "openrc":
			// NOTE(Xe): this is a sin, however openrc doesn't really have the concept
			// of service readiness. If this sleep is removed then tailscale will not be
			// ready once the `tailscale up` command is sent. This is not ideal, but I
			// am not really sure there is a good way around this without a delay of
			// some kind.
			batch = append(batch, &expect.BSnd{S: "rc-service tailscaled start && sleep 2\n"})
		case "systemd":
			batch = append(batch, &expect.BSnd{S: "systemctl start tailscaled.service\n"})
		}

		batch = append(batch, &expect.BExp{R: `(\#)`})

		runTestCommands(t, timeout, cli, batch)
	})

	t.Run("login", func(t *testing.T) {
		runTestCommands(t, timeout, cli, []expect.Batcher{
			&expect.BSnd{S: fmt.Sprintf("tailscale up --login-server=%s\n", loginServer)},
			&expect.BExp{R: `Success.`},
		})
	})

	t.Run("tailscale status", func(t *testing.T) {
		runTestCommands(t, timeout, cli, []expect.Batcher{
			&expect.BSnd{S: "sleep 5 && tailscale status\n"},
			&expect.BExp{R: `100.64.0.1`},
			&expect.BExp{R: `(\#)`},
		})
	})

	t.Run("ping-ipv4", func(t *testing.T) {
		runTestCommands(t, timeout, cli, []expect.Batcher{
			&expect.BSnd{S: "tailscale ping -c 1 100.64.0.1\n"},
			&expect.BExp{R: `pong from.*\(100.64.0.1\)`},
			&expect.BSnd{S: "ping -c 1 100.64.0.1\n"},
			&expect.BExp{R: `bytes`},
		})
	})

	t.Run("outgoing-tcp-ipv4", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		s := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cancel()
				fmt.Fprintln(w, "connection established")
			}),
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("::", "0"))
		if err != nil {
			t.Fatalf("can't make HTTP server: %v", err)
		}
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		go s.Serve(ln)

		runTestCommands(t, timeout, cli, []expect.Batcher{
			&expect.BSnd{S: fmt.Sprintf("curl http://%s:%s\n", "100.64.0.1", port)},
			&expect.BExp{R: `connection established`},
		})
		<-ctx.Done()
	})

	t.Run("incoming-ssh-ipv4", func(t *testing.T) {
		sess, err := cli.NewSession()
		if err != nil {
			t.Fatalf("can't make incoming session: %v", err)
		}
		defer sess.Close()
		ipBytes, err := sess.Output("tailscale ip -4")
		if err != nil {
			t.Fatalf("can't run `tailscale ip -4`: %v", err)
		}
		ip := string(bytes.TrimSpace(ipBytes))

		conn, err := h.testerDialer.Dial("tcp", net.JoinHostPort(ip, "22"))
		if err != nil {
			t.Fatalf("can't dial connection to vm: %v", err)
		}
		defer conn.Close()

		sshConn, chanchan, reqchan, err := ssh.NewClientConn(conn, net.JoinHostPort(ip, "22"), ccfg)
		if err != nil {
			t.Fatalf("can't negotiate connection over tailscale: %v", err)
		}
		defer sshConn.Close()

		cli := ssh.NewClient(sshConn, chanchan, reqchan)
		defer cli.Close()

		sess, err = cli.NewSession()
		if err != nil {
			t.Fatalf("can't make SSH session with VM: %v", err)
		}
		defer sess.Close()

		testIPBytes, err := sess.Output("tailscale ip -4")
		if err != nil {
			t.Fatalf("can't run command on remote VM: %v", err)
		}

		if !bytes.Equal(testIPBytes, ipBytes) {
			t.Fatalf("wanted reported ip to be %q, got: %q", string(ipBytes), string(testIPBytes))
		}
	})

	t.Run("outgoing-udp-ipv4", func(t *testing.T) {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("can't get working directory: %v", err)
		}
		dir := t.TempDir()
		run(t, cwd, "go", "build", "-o", filepath.Join(dir, "udp_tester"), "./udp_tester.go")

		sftpCli, err := sftp.NewClient(cli)
		if err != nil {
			t.Fatalf("can't connect over sftp to copy binaries: %v", err)
		}
		defer sftpCli.Close()

		copyFile(t, sftpCli, filepath.Join(dir, "udp_tester"), "/udp_tester")

		uaddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort("::", "0"))
		if err != nil {
			t.Fatalf("can't resolve udp listener addr: %v", err)
		}

		buf := make([]byte, 1024)

		ln, err := net.ListenUDP("udp", uaddr)
		if err != nil {
			t.Fatalf("can't listen for UDP traffic: %v", err)
		}
		defer ln.Close()

		sess, err := cli.NewSession()
		if err != nil {
			t.Fatalf("can't open session: %v", err)
		}
		defer sess.Close()

		sess.Stdin = strings.NewReader("hi")
		sess.Stdout = logger.FuncWriter(t.Logf)
		sess.Stderr = logger.FuncWriter(t.Logf)

		_, port, _ := net.SplitHostPort(ln.LocalAddr().String())

		go func() {
			cmd := fmt.Sprintf("/udp_tester -client %s\n", net.JoinHostPort("100.64.0.1", port))
			time.Sleep(10 * time.Millisecond)
			t.Logf("sending packet: %s", cmd)
			err := sess.Run(cmd)
			if err != nil {
				t.Errorf("can't send UDP packet: %v", err)
			}
		}()

		t.Log("listening for packet")
		n, _, err := ln.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}

		if n == 0 {
			t.Fatal("got nothing")
		}

		if !bytes.Contains(buf, []byte("hi")) {
			t.Fatal("did not get UDP message")
		}
	})

	t.Run("incoming-udp-ipv4", func(t *testing.T) {
		// vms_test.go:947: can't dial: socks connect udp 127.0.0.1:36497->100.64.0.2:33409: network not implemented
		t.Skip("can't make outgoing sockets over UDP with our socks server")

		sess, err := cli.NewSession()
		if err != nil {
			t.Fatalf("can't open session: %v", err)
		}
		defer sess.Close()

		ip, err := sess.Output("tailscale ip -4")
		if err != nil {
			t.Fatalf("can't nab ipv4 address: %v", err)
		}

		port, err := getProbablyFreePortNumber()
		if err != nil {
			t.Fatalf("unable to fetch port number: %v", err)
		}

		go func() {
			time.Sleep(10 * time.Millisecond)

			conn, err := h.testerDialer.Dial("udp", net.JoinHostPort(string(bytes.TrimSpace(ip)), strconv.Itoa(port)))
			if err != nil {
				t.Errorf("can't dial: %v", err)
			}

			fmt.Fprint(conn, securePassword)
		}()

		sess, err = cli.NewSession()
		if err != nil {
			t.Fatalf("can't open session: %v", err)
		}
		defer sess.Close()
		sess.Stderr = logger.FuncWriter(t.Logf)

		msg, err := sess.Output(
			fmt.Sprintf(
				"/udp_tester -server %s",
				net.JoinHostPort(string(bytes.TrimSpace(ip)), strconv.Itoa(port)),
			),
		)

		if msg := string(bytes.TrimSpace(msg)); msg != securePassword {
			t.Fatalf("wanted %q from vm, got: %q", securePassword, msg)
		}
	})
}

func runTestCommands(t *testing.T, timeout time.Duration, cli *ssh.Client, batch []expect.Batcher) {
	e, _, err := expect.SpawnSSH(cli, timeout,
		expect.Verbose(true),
		expect.VerboseWriter(logger.FuncWriter(t.Logf)),

		// // NOTE(Xe): if you get a timeout, uncomment this region to have the raw
		// // output be sent to the test log quicker.
		// expect.Tee(nopWriteCloser{logger.FuncWriter(t.Logf)}),
	)
	if err != nil {
		t.Fatalf("%s: can't register a shell session: %v", cli.RemoteAddr(), err)
	}
	defer e.Close()

	_, err = e.ExpectBatch(batch, timeout)
	if err != nil {
		sess, terr := cli.NewSession()
		if terr != nil {
			t.Fatalf("can't dump tailscaled logs on failed test: %v", err)
		}
		sess.Stdout = logger.FuncWriter(t.Logf)
		sess.Stderr = logger.FuncWriter(t.Logf)
		terr = sess.Run("journalctl -u tailscaled")
		if terr != nil {
			t.Fatalf("can't dump tailscaled logs on failed test: %v", err)
		}
		t.Fatalf("not successful: %v", err)
	}
}

func (h Harness) copyBinaries(t *testing.T, d Distro, conn *ssh.Client) {
	bins := h.bins
	if strings.HasPrefix(d.name, "nixos") {
		return
	}

	cli, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatalf("can't connect over sftp to copy binaries: %v", err)
	}

	mkdir(t, cli, "/usr/bin")
	mkdir(t, cli, "/usr/sbin")
	mkdir(t, cli, "/etc/default")
	mkdir(t, cli, "/var/lib/tailscale")

	copyFile(t, cli, bins.Daemon, "/usr/sbin/tailscaled")
	copyFile(t, cli, bins.CLI, "/usr/bin/tailscale")

	// TODO(Xe): revisit this assumption before it breaks the test.
	copyFile(t, cli, "../../../cmd/tailscaled/tailscaled.defaults", "/etc/default/tailscaled")

	switch d.initSystem {
	case "openrc":
		mkdir(t, cli, "/etc/init.d")
		copyFile(t, cli, "../../../cmd/tailscaled/tailscaled.openrc", "/etc/init.d/tailscaled")
	case "systemd":
		mkdir(t, cli, "/etc/systemd/system")
		copyFile(t, cli, "../../../cmd/tailscaled/tailscaled.service", "/etc/systemd/system/tailscaled.service")
	}

	fout, err := cli.OpenFile("/etc/default/tailscaled", os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatalf("can't append to defaults for tailscaled: %v", err)
	}
	fmt.Fprintf(fout, "\n\nTS_LOG_TARGET=%s\n", h.loginServerURL)

	t.Log("tailscale installed!")
}

func mkdir(t *testing.T, cli *sftp.Client, name string) {
	t.Helper()

	err := cli.MkdirAll(name)
	if err != nil {
		t.Fatalf("can't make %s: %v", name, err)
	}
}

func copyFile(t *testing.T, cli *sftp.Client, localSrc, remoteDest string) {
	t.Helper()

	fin, err := os.Open(localSrc)
	if err != nil {
		t.Fatalf("can't open: %v", err)
	}
	defer fin.Close()

	fi, err := fin.Stat()
	if err != nil {
		t.Fatalf("can't stat: %v", err)
	}

	fout, err := cli.Create(remoteDest)
	if err != nil {
		t.Fatalf("can't create output file: %v", err)
	}

	err = fout.Chmod(fi.Mode())
	if err != nil {
		fout.Close()
		t.Fatalf("can't chmod fout: %v", err)
	}

	n, err := io.Copy(fout, fin)
	if err != nil {
		fout.Close()
		t.Fatalf("copy failed: %v", err)
	}

	if fi.Size() != n {
		t.Fatalf("incorrect number of bytes copied: wanted: %d, got: %d", fi.Size(), n)
	}

	err = fout.Close()
	if err != nil {
		t.Fatalf("can't close fout on remote host: %v", err)
	}
}

func deriveBindhost(t *testing.T) string {
	t.Helper()

	ifName, err := interfaces.DefaultRouteInterface()
	if err != nil {
		t.Fatal(err)
	}

	var ret string
	err = interfaces.ForeachInterfaceAddress(func(i interfaces.Interface, prefix netaddr.IPPrefix) {
		if ret != "" || i.Name != ifName {
			return
		}
		ret = prefix.IP().String()
	})
	if ret != "" {
		return ret
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal("can't find a bindhost")
	return "unreachable"
}

func TestDeriveBindhost(t *testing.T) {
	t.Log(deriveBindhost(t))
}

func (h *Harness) Tailscale(t *testing.T, args ...string) {
	t.Helper()

	args = append([]string{"--socket=" + filepath.Join(h.testerDir, "sock")}, args...)
	run(t, h.testerDir, h.bins.CLI, args...)
}

// makeTestNode creates a userspace tailscaled running in netstack mode that
// enables us to make connections to and from the tailscale network being
// tested. This mutates the Harness to allow tests to dial into the tailscale
// network as well as control the tester's tailscaled.
func (h *Harness) makeTestNode(t *testing.T, bins *integration.Binaries, controlURL string) {
	dir := t.TempDir()
	h.testerDir = dir

	port, err := getProbablyFreePortNumber()
	if err != nil {
		t.Fatalf("can't get free port: %v", err)
	}

	cmd := exec.Command(
		bins.Daemon,
		"--tun=userspace-networking",
		"--state="+filepath.Join(dir, "state.json"),
		"--socket="+filepath.Join(dir, "sock"),
		fmt.Sprintf("--socks5-server=localhost:%d", port),
	)

	cmd.Env = append(
		os.Environ(),
		"NOTIFY_SOCKET="+filepath.Join(dir, "notify_socket"),
		"TS_LOG_TARGET="+h.loginServerURL,
	)

	err = cmd.Start()
	if err != nil {
		t.Fatalf("can't start tailscaled: %v", err)
	}

	t.Cleanup(func() {
		cmd.Process.Kill()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)

outer:
	for {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for tailscaled to come up")
			return
		case <-ticker.C:
			conn, err := net.Dial("unix", filepath.Join(dir, "sock"))
			if err != nil {
				continue
			}

			conn.Close()
			break outer
		}
	}

	run(t, dir, bins.CLI,
		"--socket="+filepath.Join(dir, "sock"),
		"up",
		"--login-server="+controlURL,
		"--hostname=tester",
	)

	dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), nil, &net.Dialer{})
	if err != nil {
		t.Fatalf("can't make netstack proxy dialer: %v", err)
	}
	h.testerDialer = dialer
}

type nopWriteCloser struct {
	io.Writer
}

func (nwc nopWriteCloser) Close() error { return nil }

const metaDataTemplate = `instance-id: {{.ID}}
local-hostname: {{.Hostname}}`

const userDataTemplate = `#cloud-config
#vim:syntax=yaml

cloud_config_modules:
 - runcmd

cloud_final_modules:
 - [users-groups, always]
 - [scripts-user, once-per-instance]

users:
 - name: root
   ssh-authorized-keys:
    - {{.SSHKey}}
 - name: ts
   plain_text_passwd: {{.Password}}
   groups: [ wheel ]
   sudo: [ "ALL=(ALL) NOPASSWD:ALL" ]
   shell: /bin/sh
   ssh-authorized-keys:
    - {{.SSHKey}}

write_files:
  - path: /etc/cloud/cloud.cfg.d/80_disable_network_after_firstboot.cfg
    content: |
      # Disable network configuration after first boot
      network:
        config: disabled

runcmd:
{{.InstallPre}}
 - [ curl, "{{.HostURL}}/myip/{{.Port}}", "-H", "User-Agent: {{.Hostname}}" ]
`
