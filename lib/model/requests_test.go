// Copyright (C) 2016 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"bytes"
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/db"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/ignore"
	"github.com/syncthing/syncthing/lib/protocol"
)

func TestRequestSimple(t *testing.T) {
	// Verify that the model performs a request and creates a file based on
	// an incoming index update.

	m, fc, tmpDir, w := setupModelWithConnection()
	defer func() {
		m.Stop()
		os.RemoveAll(tmpDir)
		os.Remove(w.ConfigPath())
	}()

	// We listen for incoming index updates and trigger when we see one for
	// the expected test file.
	done := make(chan struct{})
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		for _, f := range fs {
			if f.Name == "testfile" {
				close(done)
				return
			}
		}
	}
	fc.mut.Unlock()

	// Send an update for the test file, wait for it to sync and be reported back.
	contents := []byte("test file contents\n")
	fc.addFile("testfile", 0644, protocol.FileInfoTypeFile, contents)
	fc.sendIndexUpdate()
	<-done

	// Verify the contents
	if err := equalContents(filepath.Join(tmpDir, "testfile"), contents); err != nil {
		t.Error("File did not sync correctly:", err)
	}
}

func TestSymlinkTraversalRead(t *testing.T) {
	// Verify that a symlink can not be traversed for reading.

	if runtime.GOOS == "windows" {
		t.Skip("no symlink support on CI")
		return
	}

	m, fc, tmpDir, w := setupModelWithConnection()
	defer func() {
		m.Stop()
		os.RemoveAll(tmpDir)
		os.Remove(w.ConfigPath())
	}()

	// We listen for incoming index updates and trigger when we see one for
	// the expected test file.
	done := make(chan struct{})
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		for _, f := range fs {
			if f.Name == "symlink" {
				close(done)
				return
			}
		}
	}
	fc.mut.Unlock()

	// Send an update for the symlink, wait for it to sync and be reported back.
	contents := []byte("..")
	fc.addFile("symlink", 0644, protocol.FileInfoTypeSymlink, contents)
	fc.sendIndexUpdate()
	<-done

	// Request a file by traversing the symlink
	res, err := m.Request(device1, "default", "symlink/requests_test.go", 10, 0, nil, 0, false)
	if err == nil || res != nil {
		t.Error("Managed to traverse symlink")
	}
}

func TestSymlinkTraversalWrite(t *testing.T) {
	// Verify that a symlink can not be traversed for writing.

	if runtime.GOOS == "windows" {
		t.Skip("no symlink support on CI")
		return
	}

	m, fc, tmpDir, w := setupModelWithConnection()
	defer func() {
		m.Stop()
		os.RemoveAll(tmpDir)
		os.Remove(w.ConfigPath())
	}()

	// We listen for incoming index updates and trigger when we see one for
	// the expected names.
	done := make(chan struct{}, 1)
	badReq := make(chan string, 1)
	badIdx := make(chan string, 1)
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		for _, f := range fs {
			if f.Name == "symlink" {
				done <- struct{}{}
				return
			}
			if strings.HasPrefix(f.Name, "symlink") {
				badIdx <- f.Name
				return
			}
		}
	}
	fc.requestFn = func(folder, name string, offset int64, size int, hash []byte, fromTemporary bool) ([]byte, error) {
		if name != "symlink" && strings.HasPrefix(name, "symlink") {
			badReq <- name
		}
		return fc.fileData[name], nil
	}
	fc.mut.Unlock()

	// Send an update for the symlink, wait for it to sync and be reported back.
	contents := []byte("..")
	fc.addFile("symlink", 0644, protocol.FileInfoTypeSymlink, contents)
	fc.sendIndexUpdate()
	<-done

	// Send an update for things behind the symlink, wait for requests for
	// blocks for any of them to come back, or index entries. Hopefully none
	// of that should happen.
	contents = []byte("testdata testdata\n")
	fc.addFile("symlink/testfile", 0644, protocol.FileInfoTypeFile, contents)
	fc.addFile("symlink/testdir", 0644, protocol.FileInfoTypeDirectory, contents)
	fc.addFile("symlink/testsyml", 0644, protocol.FileInfoTypeSymlink, contents)
	fc.sendIndexUpdate()

	select {
	case name := <-badReq:
		t.Fatal("Should not have requested the data for", name)
	case name := <-badIdx:
		t.Fatal("Should not have sent the index entry for", name)
	case <-time.After(3 * time.Second):
		// Unfortunately not much else to trigger on here. The puller sleep
		// interval is 1s so if we didn't get any requests within two
		// iterations we should be fine.
	}
}

func TestRequestCreateTmpSymlink(t *testing.T) {
	// Test that an update for a temporary file is invalidated

	m, fc, tmpDir, w := setupModelWithConnection()
	defer func() {
		m.Stop()
		os.RemoveAll(tmpDir)
		os.Remove(w.ConfigPath())
	}()

	// We listen for incoming index updates and trigger when we see one for
	// the expected test file.
	goodIdx := make(chan struct{})
	name := fs.TempName("testlink")
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		for _, f := range fs {
			if f.Name == name {
				if f.IsInvalid() {
					goodIdx <- struct{}{}
				} else {
					t.Fatal("Received index with non-invalid temporary file")
				}
				return
			}
		}
	}
	fc.mut.Unlock()

	// Send an update for the test file, wait for it to sync and be reported back.
	fc.addFile(name, 0644, protocol.FileInfoTypeSymlink, []byte(".."))
	fc.sendIndexUpdate()

	select {
	case <-goodIdx:
	case <-time.After(3 * time.Second):
		t.Fatal("Timed out without index entry being sent")
	}
}

func TestRequestVersioningSymlinkAttack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no symlink support on Windows")
	}

	// Sets up a folder with trashcan versioning and tries to use a
	// deleted symlink to escape

	tmpDir := createTmpDir()
	defer os.RemoveAll(tmpDir)

	cfg := defaultCfgWrapper.RawCopy()
	cfg.Devices = append(cfg.Devices, config.NewDeviceConfiguration(device2, "device2"))
	cfg.Folders[0] = config.NewFolderConfiguration(protocol.LocalDeviceID, "default", "default", fs.FilesystemTypeBasic, tmpDir)
	cfg.Folders[0].Devices = []config.FolderDeviceConfiguration{
		{DeviceID: device1},
		{DeviceID: device2},
	}
	cfg.Folders[0].Versioning = config.VersioningConfiguration{
		Type: "trashcan",
	}
	w := createTmpWrapper(cfg)
	defer os.Remove(w.ConfigPath())

	db := db.OpenMemory()
	m := NewModel(w, device1, "syncthing", "dev", db, nil)
	m.AddFolder(cfg.Folders[0])
	m.ServeBackground()
	m.StartFolder("default")
	defer m.Stop()

	defer os.RemoveAll(tmpDir)

	fc := addFakeConn(m, device2)
	fc.folder = "default"

	// Create a temporary directory that we will use as target to see if
	// we can escape to it
	tmpdir, err := ioutil.TempDir("", "syncthing-test")
	if err != nil {
		t.Fatal(err)
	}

	// We listen for incoming index updates and trigger when we see one for
	// the expected test file.
	idx := make(chan int)
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		idx <- len(fs)
	}
	fc.mut.Unlock()

	// Send an update for the test file, wait for it to sync and be reported back.
	fc.addFile("foo", 0644, protocol.FileInfoTypeSymlink, []byte(tmpdir))
	fc.sendIndexUpdate()

	for updates := 0; updates < 1; updates += <-idx {
	}

	// Delete the symlink, hoping for it to get versioned
	fc.deleteFile("foo")
	fc.sendIndexUpdate()
	for updates := 0; updates < 1; updates += <-idx {
	}

	// Recreate foo and a file in it with some data
	fc.updateFile("foo", 0755, protocol.FileInfoTypeDirectory, nil)
	fc.addFile("foo/test", 0644, protocol.FileInfoTypeFile, []byte("testtesttest"))
	fc.sendIndexUpdate()
	for updates := 0; updates < 1; updates += <-idx {
	}

	// Remove the test file and see if it escaped
	fc.deleteFile("foo/test")
	fc.sendIndexUpdate()
	for updates := 0; updates < 1; updates += <-idx {
	}

	path := filepath.Join(tmpdir, "test")
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("File escaped to", path)
	}
}

func TestPullInvalidIgnoredSO(t *testing.T) {
	pullInvalidIgnored(t, config.FolderTypeSendOnly)

}

func TestPullInvalidIgnoredSR(t *testing.T) {
	pullInvalidIgnored(t, config.FolderTypeSendReceive)
}

// This test checks that (un-)ignored/invalid/deleted files are treated as expected.
func pullInvalidIgnored(t *testing.T, ft config.FolderType) {
	t.Helper()

	tmpDir := createTmpDir()
	cfg := defaultCfgWrapper.RawCopy()
	cfg.Devices = append(cfg.Devices, config.NewDeviceConfiguration(device2, "device2"))
	cfg.Folders[0] = config.NewFolderConfiguration(protocol.LocalDeviceID, "default", "default", fs.FilesystemTypeBasic, tmpDir)
	cfg.Folders[0].Devices = []config.FolderDeviceConfiguration{
		{DeviceID: device1},
		{DeviceID: device2},
	}
	cfg.Folders[0].Type = ft
	m, fc, w := setupModelWithConnectionManual(cfg)
	defer func() {
		m.Stop()
		os.RemoveAll(tmpDir)
		os.Remove(w.ConfigPath())
	}()

	// Reach in and update the ignore matcher to one that always does
	// reloads when asked to, instead of checking file mtimes. This is
	// because we might be changing the files on disk often enough that the
	// mtimes will be unreliable to determine change status.
	m.fmut.Lock()
	m.folderIgnores["default"] = ignore.New(cfg.Folders[0].Filesystem(), ignore.WithChangeDetector(newAlwaysChanged()))
	m.fmut.Unlock()

	if err := m.SetIgnores("default", []string{"*ignored*"}); err != nil {
		panic(err)
	}

	contents := []byte("test file contents\n")
	otherContents := []byte("other test file contents\n")

	invIgn := "invalid:ignored"
	invDel := "invalid:deleted"
	ign := "ignoredNonExisting"
	ignExisting := "ignoredExisting"

	fc.addFile(invIgn, 0644, protocol.FileInfoTypeFile, contents)
	fc.addFile(invDel, 0644, protocol.FileInfoTypeFile, contents)
	fc.deleteFile(invDel)
	fc.addFile(ign, 0644, protocol.FileInfoTypeFile, contents)
	fc.addFile(ignExisting, 0644, protocol.FileInfoTypeFile, contents)
	if err := ioutil.WriteFile(filepath.Join(tmpDir, ignExisting), otherContents, 0644); err != nil {
		panic(err)
	}

	done := make(chan struct{})
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		expected := map[string]struct{}{invIgn: {}, ign: {}, ignExisting: {}}
		for _, f := range fs {
			if _, ok := expected[f.Name]; !ok {
				t.Errorf("Unexpected file %v was added to index", f.Name)
			}
			if !f.IsInvalid() {
				t.Errorf("File %v wasn't marked as invalid", f.Name)
			}
			delete(expected, f.Name)
		}
		for name := range expected {
			t.Errorf("File %v wasn't added to index", name)
		}
		done <- struct{}{}
	}
	fc.mut.Unlock()

	sub := events.Default.Subscribe(events.FolderErrors)
	defer events.Default.Unsubscribe(sub)

	fc.sendIndexUpdate()

	timeout := time.NewTimer(5 * time.Second)
	select {
	case ev := <-sub.C():
		t.Fatalf("Errors while pulling: %v", ev)
	case <-timeout.C:
		t.Fatalf("timed out before index was received")
	case <-done:
		return
	}

	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		expected := map[string]struct{}{ign: {}, ignExisting: {}}
		for _, f := range fs {
			if _, ok := expected[f.Name]; !ok {
				t.Fatalf("Unexpected file %v was updated in index", f.Name)
			}
			if f.IsInvalid() {
				t.Errorf("File %v is still marked as invalid", f.Name)
			}
			// The unignored files should only have a local version,
			// to mark them as in conflict with any other existing versions.
			ev := protocol.Vector{}.Update(device1.Short())
			if v := f.Version; !v.Equal(ev) {
				t.Errorf("File %v has version %v, expected %v", f.Name, v, ev)
			}
			if f.Name == ign {
				if !f.Deleted {
					t.Errorf("File %v was not marked as deleted", f.Name)
				}
			} else if f.Deleted {
				t.Errorf("File %v is marked as deleted", f.Name)
			}
			delete(expected, f.Name)
		}
		for name := range expected {
			t.Errorf("File %v wasn't updated in index", name)
		}
		done <- struct{}{}
	}
	// Make sure pulling doesn't interfere, as index updates are racy and
	// thus we cannot distinguish between scan and pull results.
	fc.requestFn = func(folder, name string, offset int64, size int, hash []byte, fromTemporary bool) ([]byte, error) {
		return nil, nil
	}
	fc.mut.Unlock()

	if err := m.SetIgnores("default", []string{"*:ignored*"}); err != nil {
		panic(err)
	}

	timeout = time.NewTimer(5 * time.Second)
	select {
	case <-timeout.C:
		t.Fatalf("timed out before index was received")
	case <-done:
		return
	}
}

func TestIssue4841(t *testing.T) {
	m, fc, tmpDir, w := setupModelWithConnection()
	defer func() {
		m.Stop()
		os.RemoveAll(tmpDir)
		os.Remove(w.ConfigPath())
	}()

	received := make(chan protocol.FileInfo)
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		if len(fs) != 1 {
			t.Fatalf("Sent index with %d files, should be 1", len(fs))
		}
		if fs[0].Name != "foo" {
			t.Fatalf(`Sent index with file %v, should be "foo"`, fs[0].Name)
		}
		received <- fs[0]
		return
	}
	fc.mut.Unlock()

	// Setup file from remote that was ignored locally
	m.updateLocals(defaultFolderConfig.ID, []protocol.FileInfo{{
		Name:       "foo",
		Type:       protocol.FileInfoTypeFile,
		LocalFlags: protocol.FlagLocalIgnored,
		Version:    protocol.Vector{}.Update(device2.Short()),
	}})
	<-received

	// Scan without ignore patterns with "foo" not existing locally
	if err := m.ScanFolder("default"); err != nil {
		t.Fatal("Failed scanning:", err)
	}

	f := <-received
	if expected := (protocol.Vector{}.Update(device1.Short())); !f.Version.Equal(expected) {
		t.Errorf("Got Version == %v, expected %v", f.Version, expected)
	}
}

func TestRescanIfHaveInvalidContent(t *testing.T) {
	m, fc, tmpDir, w := setupModelWithConnection()
	defer func() {
		m.Stop()
		os.RemoveAll(tmpDir)
		os.Remove(w.ConfigPath())
	}()

	payload := []byte("hello")

	if err := ioutil.WriteFile(filepath.Join(tmpDir, "foo"), payload, 0777); err != nil {
		t.Fatal(err)
	}

	received := make(chan protocol.FileInfo)
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		if len(fs) != 1 {
			t.Fatalf("Sent index with %d files, should be 1", len(fs))
		}
		if fs[0].Name != "foo" {
			t.Fatalf(`Sent index with file %v, should be "foo"`, fs[0].Name)
		}
		received <- fs[0]
		return
	}
	fc.mut.Unlock()

	// Scan without ignore patterns with "foo" not existing locally
	if err := m.ScanFolder("default"); err != nil {
		t.Fatal("Failed scanning:", err)
	}

	f := <-received
	if f.Blocks[0].WeakHash != 103547413 {
		t.Fatalf("unexpected weak hash: %d != 103547413", f.Blocks[0].WeakHash)
	}

	res, err := m.Request(device2, "default", "foo", int32(len(payload)), 0, f.Blocks[0].Hash, f.Blocks[0].WeakHash, false)
	if err != nil {
		t.Fatal(err)
	}
	buf := res.Data()
	if !bytes.Equal(buf, payload) {
		t.Errorf("%s != %s", buf, payload)
	}

	payload = []byte("bye")
	buf = make([]byte, len(payload))

	if err := ioutil.WriteFile(filepath.Join(tmpDir, "foo"), payload, 0777); err != nil {
		t.Fatal(err)
	}

	res, err = m.Request(device2, "default", "foo", int32(len(payload)), 0, f.Blocks[0].Hash, f.Blocks[0].WeakHash, false)
	if err == nil {
		t.Fatalf("expected failure")
	}

	select {
	case f := <-received:
		if f.Blocks[0].WeakHash != 41943361 {
			t.Fatalf("unexpected weak hash: %d != 41943361", f.Blocks[0].WeakHash)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out")
	}
}

func TestParentDeletion(t *testing.T) {
	m, fc, tmpDir, w := setupModelWithConnection()
	defer func() {
		m.Stop()
		os.RemoveAll(tmpDir)
		os.Remove(w.ConfigPath())
	}()

	parent := "foo"
	child := filepath.Join(parent, "bar")
	testFs := fs.NewFilesystem(fs.FilesystemTypeBasic, tmpDir)

	received := make(chan []protocol.FileInfo)
	fc.addFile(parent, 0777, protocol.FileInfoTypeDirectory, nil)
	fc.addFile(child, 0777, protocol.FileInfoTypeDirectory, nil)
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		received <- fs
		return
	}
	fc.mut.Unlock()
	fc.sendIndexUpdate()

	// Get back index from initial setup
	select {
	case fs := <-received:
		if len(fs) != 2 {
			t.Fatalf("Sent index with %d files, should be 2", len(fs))
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out")
	}

	// Delete parent dir
	if err := testFs.RemoveAll(parent); err != nil {
		t.Fatal(err)
	}

	// Scan only the child dir (not the parent)
	if err := m.ScanFolderSubdirs("default", []string{child}); err != nil {
		t.Fatal("Failed scanning:", err)
	}

	select {
	case fs := <-received:
		if len(fs) != 1 {
			t.Fatalf("Sent index with %d files, should be 1", len(fs))
		}
		if fs[0].Name != child {
			t.Fatalf(`Sent index with file "%v", should be "%v"`, fs[0].Name, child)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out")
	}

	// Recreate the child dir on the remote
	fc.updateFile(child, 0777, protocol.FileInfoTypeDirectory, nil)
	fc.sendIndexUpdate()

	// Wait for the child dir to be recreated and sent to the remote
	select {
	case fs := <-received:
		l.Debugln("sent:", fs)
		found := false
		for _, f := range fs {
			if f.Name == child {
				if f.Deleted {
					t.Fatalf(`File "%v" is still deleted`, child)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf(`File "%v" is missing in index`, child)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out")
	}
}

// TestRequestSymlinkWindows checks that symlinks aren't marked as deleted on windows
// Issue: https://github.com/syncthing/syncthing/issues/5125
func TestRequestSymlinkWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows specific test")
	}

	m, fc, tmpDir, w := setupModelWithConnection()
	defer func() {
		m.Stop()
		os.RemoveAll(tmpDir)
		os.Remove(w.ConfigPath())
	}()

	first := make(chan struct{})
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		// expected first index
		if len(fs) != 1 {
			t.Fatalf("Expected just one file in index, got %v", fs)
		}
		f := fs[0]
		if f.Name != "link" {
			t.Fatalf(`Got file info with path "%v", expected "link"`, f.Name)
		}
		if !f.IsInvalid() {
			t.Errorf(`File info was not marked as invalid`)
		}
		close(first)
	}
	fc.mut.Unlock()

	fc.addFile("link", 0644, protocol.FileInfoTypeSymlink, nil)
	fc.sendIndexUpdate()

	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatalf("timed out before pull was finished")
	}

	sub := events.Default.Subscribe(events.StateChanged | events.LocalIndexUpdated)
	defer events.Default.Unsubscribe(sub)

	m.ScanFolder("default")

	for {
		select {
		case ev := <-sub.C():
			switch data := ev.Data.(map[string]interface{}); {
			case ev.Type == events.LocalIndexUpdated:
				t.Fatalf("Local index was updated unexpectedly: %v", data)
			case ev.Type == events.StateChanged:
				if data["from"] == "scanning" {
					return
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Timed out before scan finished")
		}
	}
}

func setupModelWithConnection() (*Model, *fakeConnection, string, *config.Wrapper) {
	tmpDir := createTmpDir()
	cfg := defaultCfgWrapper.RawCopy()
	cfg.Devices = append(cfg.Devices, config.NewDeviceConfiguration(device2, "device2"))
	cfg.Folders[0] = config.NewFolderConfiguration(protocol.LocalDeviceID, "default", "default", fs.FilesystemTypeBasic, tmpDir)
	cfg.Folders[0].Devices = []config.FolderDeviceConfiguration{
		{DeviceID: device1},
		{DeviceID: device2},
	}
	m, fc, w := setupModelWithConnectionManual(cfg)
	return m, fc, tmpDir, w
}

func setupModelWithConnectionManual(cfg config.Configuration) (*Model, *fakeConnection, *config.Wrapper) {
	w := createTmpWrapper(cfg)

	db := db.OpenMemory()
	m := NewModel(w, device1, "syncthing", "dev", db, nil)
	m.AddFolder(cfg.Folders[0])
	m.ServeBackground()
	m.StartFolder("default")

	fc := addFakeConn(m, device2)
	fc.folder = "default"

	m.ScanFolder("default")

	return m, fc, w
}

func createTmpDir() string {
	tmpDir, err := ioutil.TempDir("testdata", "_request-")
	if err != nil {
		panic("Failed to create temporary testing dir")
	}
	return tmpDir
}

func equalContents(path string, contents []byte) error {
	if bs, err := ioutil.ReadFile(path); err != nil {
		return err
	} else if !bytes.Equal(bs, contents) {
		return errors.New("incorrect data")
	}
	return nil
}

func TestRequestRemoteRenameChanged(t *testing.T) {
	m, fc, tmpDir, w := setupModelWithConnection()
	defer func() {
		m.Stop()
		os.RemoveAll(tmpDir)
		os.Remove(w.ConfigPath())
	}()
	tfs := fs.NewFilesystem(fs.FilesystemTypeBasic, tmpDir)

	done := make(chan struct{})
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		if len(fs) != 2 {
			t.Fatalf("Received index with %v indexes instead of 2", len(fs))
		}
		close(done)
	}
	fc.mut.Unlock()

	// setup
	a := "a"
	b := "b"
	data := map[string][]byte{
		a: []byte("aData"),
		b: []byte("bData"),
	}
	for _, n := range [2]string{a, b} {
		fc.addFile(n, 0644, protocol.FileInfoTypeFile, data[n])
	}
	fc.sendIndexUpdate()
	select {
	case <-done:
		done = make(chan struct{})
		fc.mut.Lock()
		fc.indexFn = func(folder string, fs []protocol.FileInfo) {
			close(done)
		}
		fc.mut.Unlock()
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}

	for _, n := range [2]string{a, b} {
		if err := equalContents(filepath.Join(tmpDir, n), data[n]); err != nil {
			t.Fatal(err)
		}
	}

	fd, err := tfs.OpenFile(b, fs.OptReadWrite, 0644)
	if err != nil {
		t.Fatal(err)
	}
	otherData := []byte("otherData")
	if _, err = fd.Write(otherData); err != nil {
		t.Fatal(err)
	}
	fd.Close()

	// rename
	fc.deleteFile(a)
	fc.updateFile(b, 0644, protocol.FileInfoTypeFile, data[a])
	fc.sendIndexUpdate()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}

	// Check outcome
	tfs.Walk(".", func(path string, info fs.FileInfo, err error) error {
		switch {
		case path == a:
			t.Errorf(`File "a" was not removed`)
		case path == b:
			if err := equalContents(filepath.Join(tmpDir, b), otherData); err != nil {
				t.Errorf(`Modified file "b" was overwritten`)
			}
		}
		return nil
	})
}

func TestRequestRemoteRenameConflict(t *testing.T) {
	m, fc, tmpDir, w := setupModelWithConnection()
	defer func() {
		m.Stop()
		os.RemoveAll(tmpDir)
		os.Remove(w.ConfigPath())
	}()
	tfs := fs.NewFilesystem(fs.FilesystemTypeBasic, tmpDir)

	recv := make(chan int)
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		recv <- len(fs)
	}
	fc.mut.Unlock()

	// setup
	a := "a"
	b := "b"
	data := map[string][]byte{
		a: []byte("aData"),
		b: []byte("bData"),
	}
	for _, n := range [2]string{a, b} {
		fc.addFile(n, 0644, protocol.FileInfoTypeFile, data[n])
	}
	fc.sendIndexUpdate()
	select {
	case i := <-recv:
		if i != 2 {
			t.Fatalf("received %v items in index, expected 1", i)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}

	for _, n := range [2]string{a, b} {
		if err := equalContents(filepath.Join(tmpDir, n), data[n]); err != nil {
			t.Fatal(err)
		}
	}

	fd, err := tfs.OpenFile(b, fs.OptReadWrite, 0644)
	if err != nil {
		t.Fatal(err)
	}
	otherData := []byte("otherData")
	if _, err = fd.Write(otherData); err != nil {
		t.Fatal(err)
	}
	fd.Close()
	m.ScanFolders()
	select {
	case i := <-recv:
		if i != 1 {
			t.Fatalf("received %v items in index, expected 1", i)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}

	// make sure the following rename is more recent (not concurrent)
	time.Sleep(2 * time.Second)

	// rename
	fc.deleteFile(a)
	fc.updateFile(b, 0644, protocol.FileInfoTypeFile, data[a])
	fc.sendIndexUpdate()
	select {
	case <-recv:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}

	// Check outcome
	foundB := false
	foundBConfl := false
	tfs.Walk(".", func(path string, info fs.FileInfo, err error) error {
		switch {
		case path == a:
			t.Errorf(`File "a" was not removed`)
		case path == b:
			foundB = true
		case strings.HasPrefix(path, b) && strings.Contains(path, ".sync-conflict-"):
			foundBConfl = true
		}
		return nil
	})
	if !foundB {
		t.Errorf(`File "b" was removed`)
	}
	if !foundBConfl {
		t.Errorf(`No conflict file for "b" was created`)
	}
}

func TestRequestDeleteChanged(t *testing.T) {
	m, fc, tmpDir, w := setupModelWithConnection()
	defer func() {
		m.Stop()
		os.RemoveAll(tmpDir)
		os.Remove(w.ConfigPath())
	}()
	tfs := fs.NewFilesystem(fs.FilesystemTypeBasic, tmpDir)

	done := make(chan struct{})
	fc.mut.Lock()
	fc.indexFn = func(folder string, fs []protocol.FileInfo) {
		close(done)
	}
	fc.mut.Unlock()

	// setup
	a := "a"
	data := []byte("aData")
	fc.addFile(a, 0644, protocol.FileInfoTypeFile, data)
	fc.sendIndexUpdate()
	select {
	case <-done:
		done = make(chan struct{})
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}

	fd, err := tfs.OpenFile(a, fs.OptReadWrite, 0644)
	if err != nil {
		t.Fatal(err)
	}
	otherData := []byte("otherData")
	if _, err = fd.Write(otherData); err != nil {
		t.Fatal(err)
	}
	fd.Close()

	// rename
	fc.deleteFile(a)
	fc.sendIndexUpdate()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}

	// Check outcome
	if _, err := tfs.Lstat(a); err != nil {
		if os.IsNotExist(err) {
			t.Error(`Modified file "a" was removed`)
		} else {
			t.Error(`Error stating file "a":`, err)
		}
	}
}
