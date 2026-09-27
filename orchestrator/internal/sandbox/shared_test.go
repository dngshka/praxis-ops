package sandbox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testImageID = "d0ded7f913f9a39ffafd78ce5d30e354788c117785e79ac3b0b84b294c78cc66"

func sharedRunbook(paths ...string) Runbook {
	rb := DefaultRunbook()
	rb.Image = "praxis/med-06@sha256:" + testImageID
	rb.SharedFromImage = paths
	return rb
}

func TestRunbook_SharedFromImageJSON(t *testing.T) {
	var rb Runbook
	if err := json.Unmarshal([]byte(`{"shared_from_image":["/opt/medusa/node_modules"]}`), &rb); err != nil {
		t.Fatal(err)
	}
	if len(rb.SharedFromImage) != 1 || rb.SharedFromImage[0] != "/opt/medusa/node_modules" {
		t.Fatalf("shared_from_image not decoded: %#v", rb.SharedFromImage)
	}
}

func TestRunbookValidate_SharedFromImage(t *testing.T) {
	for _, ok := range [][]string{nil, {"/opt/medusa/node_modules"}, {"/usr/lib/node_modules", "/opt/x"}} {
		if err := sharedRunbook(ok...).Validate(); err != nil {
			t.Errorf("%v: unexpected error %v", ok, err)
		}
	}
	bad := [][]string{
		{"/"}, {"opt/medusa"}, {"/opt/../etc"}, {"/opt/medusa/"}, {"/opt//medusa"},
		{"/proc"}, {"/sys/fs"}, {"/dev/shm"},
		{"/a", "/b", "/c", "/d", "/e", "/f", "/g", "/h", "/i"},
	}
	for _, b := range bad {
		if err := sharedRunbook(b...).Validate(); !errors.Is(err, ErrInvalidRunbook) {
			t.Errorf("%v: want ErrInvalidRunbook, got %v", b, err)
		}
	}
}

func TestSharedMounts(t *testing.T) {
	root := t.TempDir()
	nm := filepath.Join(root, testImageID, "opt", "medusa", "node_modules")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	b := &ContainerBackend{}

	if got := b.sharedMounts(sharedRunbook("/opt/medusa/node_modules")); got != nil {
		t.Fatalf("no shared dir configured: want no mounts, got %v", got)
	}
	if err := b.SetSharedDir(root); err != nil {
		t.Fatal(err)
	}

	got := b.sharedMounts(sharedRunbook("/opt/medusa/node_modules", "/opt/missing"))
	if len(got) != 1 {
		t.Fatalf("want exactly the path the host has a copy of, got %v", got)
	}
	want, _ := filepath.EvalSymlinks(nm)
	if got[0].Source != want || got[0].Target != "/opt/medusa/node_modules" || !got[0].ReadOnly || got[0].Type != "bind" {
		t.Fatalf("mount = %+v", got[0])
	}

	// Another image has no copy: nothing is mounted.
	other := sharedRunbook("/opt/medusa/node_modules")
	other.Image = "praxis/med-05@sha256:" + strings.Repeat("a", 64)
	if got := b.sharedMounts(other); got != nil {
		t.Fatalf("image without a copy: want no mounts, got %v", got)
	}

	// A tag is never resolved to a directory.
	tagged := sharedRunbook("/opt/medusa/node_modules")
	tagged.Image = "praxis/med-06:local"
	if got := b.sharedMounts(tagged); got != nil {
		t.Fatalf("tagged image: want no mounts, got %v", got)
	}
}

func TestSharedMounts_RefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	dir := filepath.Join(root, testImageID, "opt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "medusa")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	b := &ContainerBackend{}
	if err := b.SetSharedDir(root); err != nil {
		t.Fatal(err)
	}
	if got := b.sharedMounts(sharedRunbook("/opt/medusa")); got != nil {
		t.Fatalf("a copy reached through a symlink must not be mounted, got %v", got)
	}
}

func TestSetSharedDir_Errors(t *testing.T) {
	b := &ContainerBackend{}
	if err := b.SetSharedDir(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("missing dir: want error")
	}
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.SetSharedDir(f); err == nil {
		t.Fatal("file instead of dir: want error")
	}
	if b.sharedDir != "" {
		t.Fatal("a failed SetSharedDir must leave the feature off")
	}
}
