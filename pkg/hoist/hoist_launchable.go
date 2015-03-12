package hoist

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/square/p2/pkg/artifact"
	"github.com/square/p2/pkg/cgroups"
	"github.com/square/p2/pkg/runit"
	"github.com/square/p2/pkg/uri"
	"github.com/square/p2/pkg/user"
	"github.com/square/p2/pkg/util"
)

type Fetcher func(string, string) error

// A HoistLaunchable represents a particular install of a hoist artifact.
type Launchable struct {
	Location     string         // A URL where we can download the artifact from.
	Id           string         // A unique identifier for this launchable, used when creating runit services
	RunAs        string         // The user to assume when launching the executable
	ConfigDir    string         // The value for chpst -e. See http://smarden.org/runit/chpst.8.html
	FetchToFile  Fetcher        // Callback that downloads the file from the remote location.
	RootDir      string         // The root directory of the launchable, containing N:N>=1 installs.
	Chpst        string         // The path to chpst
	Cgexec       string         // The path to cgexec
	CgroupConfig cgroups.Config // Cgroup parameters to use with cgexec
}

func DefaultFetcher() Fetcher {
	return uri.URICopy
}

func (hl *Launchable) Halt(serviceBuilder *runit.ServiceBuilder, sv *runit.SV) error {

	// probably want to do something with output at some point
	_, err := hl.Disable()
	if err != nil {
		return err
	}

	// probably want to do something with output at some point
	err = hl.Stop(serviceBuilder, sv)
	if err != nil {
		return err
	}

	err = hl.MakeLast()
	if err != nil {
		return err
	}

	return nil
}

func (hl *Launchable) Launch(serviceBuilder *runit.ServiceBuilder, sv *runit.SV) error {
	err := cgroups.Default.Write(hl.CgroupConfig)
	if err != nil {
		return util.Errorf("Could not configure cgroup %s: %s", hl.CgroupConfig.Name, err)
	}

	// Should probably do something with output at some point
	// probably want to do something with output at some point
	err = hl.Start(serviceBuilder, sv)
	if err != nil {
		return util.Errorf("Could not launch %s: %s", hl.Id, err)
	}

	_, err = hl.Enable()
	return err
}

func (hl *Launchable) PostActivate() (string, error) {
	output, err := hl.invokeBinScript("post-activate")

	// providing a post-activate script is optional, ignore those errors
	if err != nil && !os.IsNotExist(err) {
		return output, err
	}

	return output, nil
}

func (hl *Launchable) Disable() (string, error) {
	output, err := hl.invokeBinScript("disable")

	// providing a disable script is optional, ignore those errors
	if err != nil && !os.IsNotExist(err) {
		return output, err
	}

	return output, nil
}

func (hl *Launchable) Enable() (string, error) {
	output, err := hl.invokeBinScript("enable")

	// providing an enable script is optional, ignore those errors
	if err != nil && !os.IsNotExist(err) {
		return output, err
	}

	return output, nil
}

func (hl *Launchable) invokeBinScript(script string) (string, error) {
	cmdPath := filepath.Join(hl.InstallDir(), "bin", script)
	_, err := os.Stat(cmdPath)
	if err != nil {
		return "", err
	}

	cmd := exec.Command(hl.Chpst, "-u", hl.RunAs, "-e", hl.ConfigDir, cmdPath)
	buffer := bytes.Buffer{}
	cmd.Stdout = &buffer
	cmd.Stderr = &buffer
	err = cmd.Run()
	if err != nil {
		return buffer.String(), err
	}

	return buffer.String(), nil
}

func (hl *Launchable) Stop(serviceBuilder *runit.ServiceBuilder, sv *runit.SV) error {
	executables, err := hl.Executables(serviceBuilder)
	if err != nil {
		return err
	}

	for _, executable := range executables {
		// if we use sv -w to wait for the service to stop and then SIGKILL, we
		// will also kill the preparer itself before it can restart. do not use
		// sv -w yet.
		_, err := sv.Stop(&executable.Service)
		if err != nil {
			// TODO: FAILURE SCENARIO (what should we do here?)
			// 1) does `sv stop` ever exit nonzero?
			// 2) should we keep stopping them all anyway?
			return err
		}
	}
	return nil
}

// Start will take a launchable and start every runit service associated with the launchable.
// All services will attempt to be started.
func (hl *Launchable) Start(serviceBuilder *runit.ServiceBuilder, sv *runit.SV) error {

	executables, err := hl.Executables(serviceBuilder)
	if err != nil {
		return err
	}

	for _, executable := range executables {
		_, err := sv.Restart(&executable.Service)
		if err != runit.SuperviseOkMissing {
			return err
		}
	}

	return nil
}

func (hl *Launchable) Executables(serviceBuilder *runit.ServiceBuilder) ([]Executable, error) {
	if !hl.Installed() {
		return []Executable{}, util.Errorf("%s is not installed", hl.Id)
	}

	binLaunchPath := filepath.Join(hl.InstallDir(), "bin", "launch")

	binLaunchInfo, err := os.Stat(binLaunchPath)
	if err != nil {
		return nil, util.Errorf("%s", err)
	}

	// we support bin/launch being a file, or a directory, so we check here.
	services := []os.FileInfo{binLaunchInfo}
	serviceDir := filepath.Dir(binLaunchPath)
	if binLaunchInfo.IsDir() {
		serviceDir = binLaunchPath
		services, err = ioutil.ReadDir(binLaunchPath)
		if err != nil {
			return nil, err
		}
	}

	var executables []Executable
	for _, service := range services {
		serviceName := fmt.Sprintf("%s__%s", hl.Id, service.Name())

		executables = append(executables, Executable{
			Service: runit.Service{
				Path: filepath.Join(serviceBuilder.RunitRoot, serviceName),
				Name: serviceName,
			},
			ExecPath:     filepath.Join(serviceDir, service.Name()),
			Chpst:        hl.Chpst,
			Cgexec:       hl.Cgexec,
			CgroupConfig: hl.CgroupConfig,
			Nolimit:      "/usr/bin/nolimit",
			RunAs:        hl.RunAs,
			ConfigDir:    hl.ConfigDir,
		})
	}
	return executables, nil
}

func (hl *Launchable) Installed() bool {
	installDir := hl.InstallDir()
	_, err := os.Stat(installDir)
	return err == nil
}

func (hl *Launchable) Install() error {
	if hl.Installed() {
		// install is idempotent, no-op if already installed
		return nil
	}

	outDir, err := ioutil.TempDir("", hl.Version())
	defer os.RemoveAll(outDir)
	if err != nil {
		return util.Errorf("Could not create temporary directory to install %s: %s", hl.Version(), err)
	}

	outPath := filepath.Join(outDir, hl.Version())

	err = hl.FetchToFile(hl.Location, outPath)
	if err != nil {
		return err
	}

	fd, err := os.Open(outPath)
	if err != nil {
		return err
	}
	defer fd.Close()

	err = hl.extractTarGz(fd, hl.InstallDir())
	if err != nil {
		return err
	}
	return nil
}

// The version of the artifact is currently derived from the location, using
// the naming scheme <the-app>_<unique-version-string>.tar.gz
func (hl *Launchable) Version() string {
	fileName := filepath.Base(hl.Location)
	return fileName[:len(fileName)-len(".tar.gz")]
}

func (*Launchable) Type() string {
	return "hoist"
}

func (hl *Launchable) AppManifest() (*artifact.AppManifest, error) {
	if !hl.Installed() {
		return nil, util.Errorf("%s has not been installed yet", hl.Id)
	}
	manPath := filepath.Join(hl.InstallDir(), "app-manifest.yaml")
	if _, err := os.Stat(manPath); os.IsNotExist(err) {
		manPath = filepath.Join(hl.InstallDir(), "app-manifest.yml")
		if _, err = os.Stat(manPath); os.IsNotExist(err) {
			return nil, util.Errorf("No app manifest was found in the Hoist launchable %s", hl.Id)
		}
	}
	return artifact.ManifestFromPath(manPath)
}

func (hl *Launchable) CurrentDir() string {
	return filepath.Join(hl.RootDir, "current")
}

func (hl *Launchable) MakeCurrent() error {
	return hl.flipSymlink(hl.CurrentDir())
}

func (hl *Launchable) LastDir() string {
	return filepath.Join(hl.RootDir, "last")
}

func (hl *Launchable) MakeLast() error {
	return hl.flipSymlink(hl.LastDir())
}

func (hl *Launchable) flipSymlink(newLinkPath string) error {
	dir, err := ioutil.TempDir(hl.RootDir, hl.Id)
	if err != nil {
		return util.Errorf("Couldn't create temporary directory for symlink: %s", err)
	}
	defer os.RemoveAll(dir)
	tempLinkPath := filepath.Join(dir, hl.Id)
	err = os.Symlink(hl.InstallDir(), tempLinkPath)
	if err != nil {
		return util.Errorf("Couldn't create symlink for hoist launchable %s: %s", hl.Id, err)
	}

	uid, gid, err := user.IDs(hl.RunAs)
	if err != nil {
		return util.Errorf("Couldn't retrieve UID/GID for hoist launchable %s user %s: %s", hl.Id, hl.RunAs, err)
	}
	err = os.Lchown(tempLinkPath, uid, gid)
	if err != nil {
		return util.Errorf("Couldn't lchown symlink for hoist launchable %s: %s", hl.Id, err)
	}

	return os.Rename(tempLinkPath, newLinkPath)
}

func (hl *Launchable) InstallDir() string {
	launchableName := hl.Version()
	return filepath.Join(hl.RootDir, "installs", launchableName)
}

func (hl *Launchable) extractTarGz(fp *os.File, dest string) (err error) {
	fz, err := gzip.NewReader(fp)
	if err != nil {
		return util.Errorf("Unable to create gzip reader: %s", err)
	}
	defer fz.Close()

	tr := tar.NewReader(fz)
	uid, gid, err := user.IDs(hl.RunAs)
	if err != nil {
		return err
	}

	err = util.MkdirChownAll(dest, uid, gid, 0755)
	if err != nil {
		return util.Errorf("Unable to create root directory %s when unpacking %s: %s", dest, fp.Name(), err)
	}
	err = os.Chown(dest, uid, gid)
	if err != nil {
		return util.Errorf("Unable to chown root directory %s to %s when unpacking %s: %s", dest, hl.RunAs, fp.Name(), err)
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return util.Errorf("Unable to read %s: %s", fp.Name(), err)
		}
		fpath := filepath.Join(dest, hdr.Name)

		switch hdr.Typeflag {
		case tar.TypeSymlink:
			err = os.Symlink(hdr.Linkname, fpath)
			if err != nil {
				return util.Errorf("Unable to create destination symlink %s (to %s) when unpacking %s: %s", fpath, hdr.Linkname, fp.Name(), err)
			}
		case tar.TypeLink:
			// hardlink paths are encoded relative to the tarball root, rather than
			// the path of the link itself, so we need to resolve that path
			linkTarget, err := filepath.Rel(filepath.Dir(hdr.Name), hdr.Linkname)
			if err != nil {
				return util.Errorf("Unable to resolve relative path for hardlink %s (to %s) when unpacking %s: %s", fpath, hdr.Linkname, fp.Name(), err)
			}
			// we can't make the hardlink right away because the target might not
			// exist, so we'll just make a symlink instead
			err = os.Symlink(linkTarget, fpath)
			if err != nil {
				return util.Errorf("Unable to create destination symlink %s (resolved %s to %s) when unpacking %s: %s", fpath, linkTarget, hdr.Linkname, fp.Name(), err)
			}
		case tar.TypeDir:
			err = os.Mkdir(fpath, hdr.FileInfo().Mode())
			if err != nil && !os.IsExist(err) {
				return util.Errorf("Unable to create destination directory %s when unpacking %s: %s", fpath, fp.Name(), err)
			}

			err = os.Chown(fpath, uid, gid)
			if err != nil {
				return util.Errorf("Unable to chown destination directory %s to %s when unpacking %s: %s", fpath, hl.RunAs, fp.Name(), err)
			}
		case tar.TypeReg, tar.TypeRegA:
			f, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, hdr.FileInfo().Mode())
			if err != nil {
				return util.Errorf("Unable to open destination file %s when unpacking %s: %s", fpath, fp.Name(), err)
			}
			defer f.Close()

			err = f.Chown(uid, gid) // this operation may cause tar unpacking to become significantly slower. Refactor as necessary.
			if err != nil {
				return util.Errorf("Unable to chown destination file %s to %s when unpacking %s: %s", fpath, hl.RunAs, fp.Name(), err)
			}

			_, err = io.Copy(f, tr)
			if err != nil {
				return util.Errorf("Unable to copy into destination file %s when unpacking %s: %s", fpath, fp.Name(), err)
			}
			f.Close() // eagerly release file descriptors rather than letting them pile up
		default:
			return util.Errorf("Unhandled type flag %q (header %v) when unpacking %s", hdr.Typeflag, hdr, fp.Name())
		}
	}
	return nil
}
