// Package image builds and caches Arch Linux golden ext4 images with
// pacstrap(8) from arch-install-scripts. A cached golden image is reflink
// copied for each container and grown to the requested disk size.
package image

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/process"
	"github.com/common-creation/sandboxxing/internal/progress"
)

// ErrUnsupported is returned for image names that are not usable.
var ErrUnsupported = errors.New("unsupported image")

// Builder builds and caches golden ext4 images.
type Builder struct {
	cfg *config.Config
	log *slog.Logger
}

// New creates a Builder for the given configuration.
func New(cfg *config.Config, log *slog.Logger) *Builder {
	return &Builder{cfg: cfg, log: log}
}

// Image returns the cached golden ext4 image of the named image, building it
// with pacstrap(8) when it does not exist yet. The golden image is kept at a
// minimal variable size; callers reflink copy it and grow the copy to the
// requested disk size.
func (b *Builder) Image(ctx context.Context, name string) (string, error) {
	if name == "" {
		name = b.cfg.Image
	}
	if err := validateName(name); err != nil {
		return "", err
	}
	if err := b.verify(); err != nil {
		return "", err
	}

	img := filepath.Join(b.cfg.ImageDir, name+".img")
	if st, err := os.Stat(img); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
		return img, nil
	}

	b.log.Info("building image with pacstrap", "image", name, "packages", len(b.cfg.ImagePackages))
	progress.From(ctx).Step("building base image %q with pacstrap (the first run downloads packages)", name)
	tmpTree := filepath.Join(b.cfg.ImageDir, "."+name+".tmp.d")
	tmpImg := filepath.Join(b.cfg.ImageDir, "."+name+".tmp.img")
	if err := os.RemoveAll(tmpTree); err != nil {
		return "", err
	}
	os.Remove(tmpImg)
	if err := os.MkdirAll(tmpTree, 0o755); err != nil {
		return "", err
	}
	if err := b.pacstrap(ctx, tmpTree); err != nil {
		os.RemoveAll(tmpTree)
		os.Remove(tmpImg)
		return "", err
	}
	if err := b.prepare(tmpTree); err != nil {
		os.RemoveAll(tmpTree)
		os.Remove(tmpImg)
		return "", err
	}
	if err := PackExt4(ctx, tmpTree, tmpImg, goldenDiskSize(tmpTree)); err != nil {
		os.RemoveAll(tmpTree)
		os.Remove(tmpImg)
		return "", err
	}
	os.RemoveAll(tmpTree)

	// Another daemon may have finished first; keep whatever is complete.
	if st, err := os.Stat(img); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
		os.Remove(tmpImg)
		return img, nil
	}
	if err := os.Rename(tmpImg, img); err != nil {
		os.Remove(tmpImg)
		return "", err
	}
	// Drop the legacy tree cache left by older releases.
	os.RemoveAll(filepath.Join(b.cfg.ImageDir, name))
	return img, nil
}

// List returns the cached image names.
func (b *Builder) List() ([]string, error) {
	entries, err := os.ReadDir(b.cfg.ImageDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		// Golden ext4 images.
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".img") {
			name := strings.TrimSuffix(e.Name(), ".img")
			if validateName(name) != nil {
				continue
			}
			if info, err := e.Info(); err != nil || info.Size() == 0 {
				continue
			}
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
			continue
		}
		// Legacy tree caches left by older releases.
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(b.cfg.ImageDir, e.Name(), ".sbx-complete")); err != nil {
				continue
			}
			if !seen[e.Name()] {
				seen[e.Name()] = true
				names = append(names, e.Name())
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// Delete removes a cached golden image and any legacy tree cache.
func (b *Builder) Delete(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	os.Remove(filepath.Join(b.cfg.ImageDir, name+".img"))
	os.Remove(filepath.Join(b.cfg.ImageDir, "."+name+".tmp.img"))
	os.RemoveAll(filepath.Join(b.cfg.ImageDir, "."+name+".tmp.d"))
	return os.RemoveAll(filepath.Join(b.cfg.ImageDir, name))
}

// verify checks the tools and host data that image building depends on.
func (b *Builder) verify() error {
	for _, tool := range []string{"pacstrap", "pacman-key", "mke2fs"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s not found: install arch-install-scripts and e2fsprogs", tool)
		}
	}
	return nil
}

// keyringDir is the location of the keyring inside the new installation.
const keyringDir = "etc/pacman.d/gnupg"

// bootstrapKeyring creates and populates the pacman keyring of a new
// installation. pacstrap's own keyring handling only initialises an empty
// keyring (-K) or copies the host's directory, and the host keyring of a
// freshly installed system does not contain the Arch Linux package signing
// keys yet. Populating from /usr/share/pacman/keyrings is what makes package
// verification work without contacting a key server.
func (b *Builder) bootstrapKeyring(ctx context.Context, rootfs string) error {
	gpgDir := filepath.Join(rootfs, keyringDir)
	if err := os.MkdirAll(gpgDir, 0o755); err != nil {
		return err
	}
	steps := [][]string{
		{"--gpgdir", gpgDir, "--init"},
		{"--gpgdir", gpgDir, "--populate", "archlinux"},
	}
	sink := progress.From(ctx)
	for _, step := range steps {
		cmd := process.Command(ctx, "pacman-key", step...)
		cmd.Stdout, cmd.Stderr = sink.Err(), sink.Err()
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("pacman-key %s: %w", strings.Join(step[:len(step)-1], " "), err)
		}
	}
	return nil
}

// pacstrap installs the configured packages into rootfs. The keyring is
// bootstrapped beforehand (-K keeps pacstrap from copying or reinitialising
// it), so that pacman can verify the package signatures offline.
func (b *Builder) pacstrap(ctx context.Context, rootfs string) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.PacstrapTimeout.Duration())
	defer cancel()

	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		return err
	}
	if err := b.bootstrapKeyring(ctx, rootfs); err != nil {
		return err
	}

	conf := b.cfg.PacmanConfig
	if conf == "" {
		generated, cleanup, err := b.writePacmanConfig()
		if err != nil {
			return err
		}
		defer cleanup()
		conf = generated
	}

	args := []string{"-K", "-C", conf, rootfs}
	args = append(args, b.cfg.ImagePackages...)

	// pacman reports its progress on stderr, which is forwarded to the SSH
	// client that started the operation.
	sink := progress.From(ctx)
	cmd := process.Command(ctx, "pacstrap", args...)
	cmd.Stdout, cmd.Stderr = sink.Err(), sink.Err()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pacstrap: %w", err)
	}
	return nil
}

// writePacmanConfig generates a minimal pacman configuration that points at
// the configured mirror.
func (b *Builder) writePacmanConfig() (string, func(), error) {
	f, err := os.CreateTemp("", "sandboxxing-pacman-*.conf")
	if err != nil {
		return "", func() {}, err
	}
	defer f.Close()
	mirror := b.cfg.Mirror
	if !strings.Contains(mirror, "$repo") {
		mirror = strings.TrimRight(mirror, "/") + "/$repo/os/$arch"
	}
	conf := fmt.Sprintf(`[options]
Architecture = %s
HoldPkg = pacman glibc
CheckSpace
SigLevel = Required DatabaseOptional
LocalFileSigLevel = Optional

[core]
Server = %s

[extra]
Server = %s
`, b.cfg.Arch, mirror, mirror)
	if err := os.WriteFile(f.Name(), []byte(conf), 0o600); err != nil {
		os.Remove(f.Name())
		return "", func() {}, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// prepare configures the freshly installed tree.
func (b *Builder) prepare(rootfs string) error {
	files := []struct {
		path string
		data string
		mode os.FileMode
	}{
		{"/etc/hostname", "sandbox\n", 0o644},
		{"/etc/locale.gen", "en_US.UTF-8 UTF-8\n", 0o644},
		{"/etc/locale.conf", "LANG=en_US.UTF-8\n", 0o644},
		{"/etc/vconsole.conf", "KEYMAP=us\n", 0o644},
		{"/etc/pacman.d/mirrorlist", b.mirrorlist(), 0o644},
		// systemd-networkd brings up the static address written per
		// container. The preset keeps the container minimal: no sshd, no
		// console getty and no first-boot prompts. A preset applies when
		// packages are installed later as well.
		{"/etc/systemd/system-preset/99-sandboxxing.preset", "disable sshd.service\ndisable sshd.socket\ndisable console-getty.service\ndisable serial-getty@ttyS0.service\ndisable systemd-firstboot.service\n", 0o644},
	}
	for _, f := range files {
		full := filepath.Join(rootfs, f.path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(f.data), f.mode); err != nil {
			return err
		}
	}

	// The machine-id must be empty so that systemd generates a unique
	// identity from the UUID passed by systemd-nspawn on first boot.
	if err := os.WriteFile(filepath.Join(rootfs, "etc/machine-id"), nil, 0o644); err != nil {
		return err
	}

	for _, unit := range []string{"systemd-networkd.service"} {
		if err := enableUnit(rootfs, unit); err != nil {
			return err
		}
	}
	// The container is entered from the host through nsenter(1): no sshd and
	// no console getty are needed inside. The preset keeps newly installed
	// packages from enabling them again.
	for _, unit := range []string{"sshd.service", "sshd.socket", "console-getty.service", "serial-getty@ttyS0.service"} {
		disableUnit(rootfs, unit)
	}
	return nil
}

// mirrorlist renders the pacman mirror list used inside the containers.
func (b *Builder) mirrorlist() string {
	mirror := b.cfg.Mirror
	if strings.Contains(mirror, "$repo") {
		return fmt.Sprintf("Server = %s\n", mirror)
	}
	return fmt.Sprintf("Server = %s/$repo/os/$arch\n", strings.TrimRight(mirror, "/"))
}

// enableUnit creates the multi-user.target.wants symlink for unit so that the
// service starts on boot.
func enableUnit(rootfs, unit string) error {
	wants := filepath.Join(rootfs, "etc/systemd/system/multi-user.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		return err
	}
	link := filepath.Join(wants, unit)
	if _, err := os.Lstat(link); err == nil {
		return nil
	}
	return os.Symlink(filepath.Join("/usr/lib/systemd/system", unit), link)
}

// disableUnit removes any enablement symlink of unit.
func disableUnit(rootfs, unit string) {
	for _, dir := range []string{
		"etc/systemd/system/multi-user.target.wants",
		"etc/systemd/system/sockets.target.wants",
		"etc/systemd/system/graphical.target.wants",
		"etc/systemd/system/getty.target.wants",
		"etc/systemd/system/sysinit.target.wants",
	} {
		os.Remove(filepath.Join(rootfs, dir, unit))
	}
}

// fallbackGoldenSize is used when the tree size cannot be measured. An Arch
// installation is around 1.5GB, so 4GB leaves ample margin.
const fallbackGoldenSize = 4 << 30

// goldenMinSize is the lower bound for a golden image. ext4 metadata and the
// journal need room beyond the file payload, so even a tiny tree is packed
// with headroom.
const goldenMinSize = 3 << 30

// goldenDiskSize estimates a minimal variable size for the golden image from
// the pacstrap tree: payload plus 1GB plus a quarter for ext4 metadata,
// rounded up to 64MB. It falls back to fallbackGoldenSize on any error.
func goldenDiskSize(tree string) int64 {
	used, err := treeDiskUsage(tree)
	if err != nil || used <= 0 {
		return fallbackGoldenSize
	}
	size := used + (1 << 30) + used/4
	const align = 64 << 20
	size = (size + align - 1) / align * align
	if size < goldenMinSize {
		size = goldenMinSize
	}
	return size
}

// treeDiskUsage sums the payload of a tree, rounding each file up to a 4K
// block and counting 4K per directory for metadata.
func treeDiskUsage(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			total += 4096
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sz := info.Size()
		if sz < 0 {
			return nil
		}
		total += (sz + 4095) / 4096 * 4096
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// PackExt4 creates an ext4 file system of size bytes from the tree directory.
// A zero size means the caller sized the file beforehand.
func PackExt4(ctx context.Context, tree, dst string, size int64) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// The journal stays enabled: a container disk image may be copied while
	// the container is running, and the journal is what makes such a copy
	// recoverable by the file system check.
	cmd := process.Command(ctx, "mke2fs",
		"-q", "-t", "ext4", "-F", "-m", "1",
		"-E", "lazy_itable_init=1",
		"-d", tree,
		dst,
	)
	cmd.Stdout, cmd.Stderr = progress.From(ctx).Err(), progress.From(ctx).Err()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mke2fs %s: %w", dst, err)
	}
	return nil
}

// validateName rejects image names that would escape the image directory.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty image name", ErrUnsupported)
	}
	if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") || name != filepath.Base(name) {
		return fmt.Errorf("%w: invalid image name %q", ErrUnsupported, name)
	}
	return nil
}
