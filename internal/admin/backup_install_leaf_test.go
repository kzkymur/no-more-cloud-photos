//go:build linux

package admin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBackupInstallExactLeaf tests GNU install's destination-leaf semantics with
// nonsecret canaries only. It does not establish source authenticity, systemd
// lifecycle safety, or protection against a hostile concurrent same-UID process.
func TestBackupInstallExactLeaf(t *testing.T) {
	binary := os.Getenv("TEST_BACKUP_GNU_INSTALL")
	explicit := binary != ""
	if !explicit {
		binary = "/usr/bin/install"
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("GNU install override must be absolute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	versionCmd := exec.CommandContext(ctx, binary, "--version")
	versionCmd.WaitDelay = time.Second
	version, err := versionCmd.Output()
	cancel()
	if err != nil || !strings.HasPrefix(string(version), "install (GNU coreutils) 9.4\n") {
		if explicit {
			t.Fatal("explicit GNU install must execute and report coreutils 9.4")
		}
		t.Skip("GNU coreutils 9.4 unavailable: actual exact-leaf canaries NOT executed")
	}
	const sourceBytes = "NONSECRET-install-source-canary\n"
	const victimBytes = "NONSECRET-install-victim-canary\n"
	cases := []struct {
		name, kind string
		exact      bool
	}{
		{"without_T_redirects_symlink_directory", "symlink-directory", false},
		{"with_T_refuses_directory", "directory", true},
		{"with_T_replaces_symlink_directory", "symlink-directory", true},
		{"with_T_replaces_symlink_file", "symlink-file", true},
		{"with_T_replaces_dangling", "dangling", true},
		{"with_T_replaces_hardlink", "hardlink", true},
		{"with_T_replaces_stale_regular", "stale", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal("resolve owned temporary directory")
			}
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal("protect owned directory")
			}
			root, err = filepath.Abs(root)
			if err != nil {
				t.Fatal("resolve absolute owned directory")
			}
			inside := func(path string) {
				rel, err := filepath.Rel(root, path)
				if err != nil || !filepath.IsAbs(path) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
					t.Fatal("path escaped owned directory")
				}
			}
			// Resolve even a dangling leaf by checking its existing parent and link
			// target. All links and targets here are exclusively created by this test.
			verify := func(path string) {
				inside(path)
				parent, err := filepath.EvalSymlinks(filepath.Dir(path))
				if err != nil {
					t.Fatal("resolve intended parent")
				}
				inside(parent)
				info, err := os.Lstat(path)
				if os.IsNotExist(err) {
					return
				}
				if err != nil {
					t.Fatal("inspect intended leaf")
				}
				if info.Mode()&os.ModeSymlink != 0 {
					target, err := os.Readlink(path)
					if err != nil {
						t.Fatal("inspect owned link target")
					}
					if !filepath.IsAbs(target) {
						target = filepath.Join(parent, target)
					}
					inside(filepath.Clean(target))
					targetParent, err := filepath.EvalSymlinks(filepath.Dir(target))
					if err != nil {
						t.Fatal("resolve link target parent")
					}
					inside(targetParent)
				}
				resolved, err := filepath.EvalSymlinks(path)
				if os.IsNotExist(err) {
					return
				}
				if err != nil {
					t.Fatal("resolve intended leaf")
				}
				inside(resolved)
			}
			write := func(path, content string) {
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal("write nonsecret canary")
				}
			}
			mkdir := func(path string) {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal("create owned directory")
				}
			}
			link := func(target, path string) {
				if err := os.Symlink(target, path); err != nil {
					t.Fatal("create owned symlink")
				}
			}
			source, dest, victim := filepath.Join(root, "source"), filepath.Join(root, "destination"), filepath.Join(root, "victim")
			write(source, sourceBytes)
			var victimBefore, sentinelBefore os.FileInfo
			var oldFile *os.File
			var oldInfo os.FileInfo
			switch tc.kind {
			case "directory":
				mkdir(dest)
			case "symlink-directory":
				mkdir(victim)
				write(filepath.Join(victim, "sentinel"), victimBytes)
				link(victim, dest)
			case "symlink-file":
				write(victim, victimBytes)
				link(victim, dest)
			case "dangling":
				link(victim, dest)
			case "hardlink":
				write(victim, victimBytes)
				if err := os.Link(victim, dest); err != nil {
					t.Fatal("create owned hardlink")
				}
			case "stale":
				write(dest, "NONSECRET-stale-canary\n")
				oldFile, err = os.Open(dest)
				if err != nil {
					t.Fatal("pin old regular inode")
				}
				defer oldFile.Close()
				oldInfo, err = oldFile.Stat()
				if err != nil {
					t.Fatal("inspect pinned old inode")
				}
			}
			if tc.kind == "symlink-directory" || tc.kind == "symlink-file" || tc.kind == "hardlink" {
				victimBefore, err = os.Stat(victim)
				if err != nil {
					t.Fatal("snapshot victim inode")
				}
			}
			if tc.kind == "symlink-directory" {
				sentinelBefore, err = os.Stat(filepath.Join(victim, "sentinel"))
				if err != nil {
					t.Fatal("snapshot sentinel inode")
				}
			}
			// Verify intended absolute AND resolved paths immediately before install
			// may unlink/replace the leaf; none can refer outside this owned tree.
			for _, path := range []string{source, dest, victim} {
				verify(path)
			}
			args := []string{"-m", "0600", source, dest}
			if tc.exact {
				args = append([]string{"-T"}, args...)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.WaitDelay = time.Second
			err = cmd.Run() // discard command output; failures expose closed metadata only
			timedOut := ctx.Err() != nil
			cancel()
			if timedOut {
				t.Fatal("GNU install exceeded bounded command deadline")
			}
			if tc.kind == "directory" {
				if err == nil {
					t.Fatal("exact-leaf install accepted directory")
				}
				entries, readErr := os.ReadDir(dest)
				if readErr != nil || len(entries) != 0 {
					t.Fatal("directory destination changed")
				}
				return
			}
			if err != nil {
				t.Fatal("GNU install canary command failed")
			}
			checkBytes := func(path, expected string) {
				content, err := os.ReadFile(path)
				if err != nil || string(content) != expected {
					t.Fatal("canary bytes mismatch")
				}
			}
			if !tc.exact {
				info, err := os.Lstat(dest)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("negative control did not preserve redirecting symlink")
				}
				checkBytes(filepath.Join(victim, "source"), sourceBytes)
			} else {
				info, err := os.Lstat(dest)
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
					t.Fatal("destination is not private regular leaf")
				}
				metadata, ok := info.Sys().(*syscall.Stat_t)
				if !ok || metadata.Uid != uint32(os.Geteuid()) || metadata.Nlink != 1 {
					t.Fatal("destination ownership or link count mismatch")
				}
				checkBytes(dest, sourceBytes)
				if oldInfo != nil && os.SameFile(oldInfo, info) {
					t.Fatal("stale regular inode was not replaced")
				}
			}
			if victimBefore != nil {
				after, err := os.Stat(victim)
				if err != nil || !os.SameFile(victimBefore, after) {
					t.Fatal("victim inode changed")
				}
				// A hardlink victim naturally changes nlink from two to one; its inode
				// and bytes, not its link count, are required to remain unchanged.
				if tc.kind != "symlink-directory" {
					checkBytes(victim, victimBytes)
				}
			}
			if tc.kind == "symlink-directory" {
				sentinel := filepath.Join(victim, "sentinel")
				after, err := os.Stat(sentinel)
				if err != nil || !os.SameFile(sentinelBefore, after) {
					t.Fatal("sentinel inode changed")
				}
				checkBytes(sentinel, victimBytes)
				entries, err := os.ReadDir(victim)
				expected := 1
				if !tc.exact {
					expected = 2
				}
				if err != nil || len(entries) != expected {
					t.Fatal("unexpected victim directory entries")
				}
				if tc.exact {
					if _, err := os.Lstat(filepath.Join(victim, "source")); !os.IsNotExist(err) {
						t.Fatal("exact-leaf install redirected into victim directory")
					}
				}
			}
			if tc.kind == "dangling" {
				if _, err := os.Lstat(victim); !os.IsNotExist(err) {
					t.Fatal("dangling target was created")
				}
			}
		})
	}
	t.Log("Actual GNU coreutils 9.4: redirect negative control and exact-leaf canaries passed; filesystem leaf semantics only")
}
