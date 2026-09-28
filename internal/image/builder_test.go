package image

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/common-creation/sandboxxing/internal/config"
)

func testBuilder(t *testing.T, imageDir string) *Builder {
	t.Helper()
	cfg := config.Default()
	cfg.ImageDir = imageDir
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestGoldenDiskSizeFallback(t *testing.T) {
	if got := goldenDiskSize(filepath.Join(t.TempDir(), "missing")); got != fallbackGoldenSize {
		t.Errorf("goldenDiskSize(missing) = %d, want %d", got, fallbackGoldenSize)
	}
}

func TestGoldenDiskSizeVariable(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "file"), make([]byte, 10<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	got := goldenDiskSize(tree)
	if got < goldenMinSize {
		t.Errorf("golden size %d is below minimum %d", got, goldenMinSize)
	}
	if got%(64<<20) != 0 {
		t.Errorf("golden size %d is not 64MB aligned", got)
	}
	used, err := treeDiskUsage(tree)
	if err != nil {
		t.Fatal(err)
	}
	if got < used {
		t.Errorf("golden size %d is smaller than payload %d", got, used)
	}
	// A larger tree must yield a larger golden image (2GB exceeds the minimum).
	big := t.TempDir()
	f, err := os.OpenFile(filepath.Join(big, "file"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(2 << 30); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if bigSize := goldenDiskSize(big); bigSize <= got {
		t.Errorf("larger tree gave %d, want more than %d", bigSize, got)
	}
}

func TestListGoldenImages(t *testing.T) {
	dir := t.TempDir()
	b := testBuilder(t, dir)
	write := func(name string, size int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("arch.img", 1024)
	write("empty.img", 0)
	write(".hidden.img", 1024)
	write("notes.txt", 1024)
	legacy := filepath.Join(dir, "legacy")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, ".sbx-complete"), []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "incomplete"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := b.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "arch" || got[1] != "legacy" {
		t.Errorf("List() = %q, want [arch legacy]", got)
	}
}

func TestDeleteRemovesGoldenAndLegacy(t *testing.T) {
	dir := t.TempDir()
	b := testBuilder(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "arch.img"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "arch")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete("arch"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "arch.img")); !os.IsNotExist(err) {
		t.Error("golden image was not removed")
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("legacy tree was not removed")
	}
	if err := b.Delete("../escape"); err == nil {
		t.Error("Delete must reject escaping names")
	}
}
