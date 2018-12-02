// Copyright (C) 2018 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// +build windows

package fs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPaths(t *testing.T) {
	testCases := []struct {
		input        string
		expectedRoot string
		expectedURI  string
	}{
		{`e:\`, `\\?\e:\`, `e:\`},
		{`\\?\e:\`, `\\?\e:\`, `e:\`},
		{`\\192.0.2.22\network\share`, `\\192.0.2.22\network\share`, `\\192.0.2.22\network\share`},
	}

	for _, testCase := range testCases {
		fs := newBasicFilesystem(testCase.input)
		if fs.root != testCase.expectedRoot {
			t.Errorf("root %q != %q", fs.root, testCase.expectedRoot)
		}
		if fs.URI() != testCase.expectedURI {
			t.Errorf("uri %q != %q", fs.URI(), testCase.expectedURI)
		}
	}

	fs := newBasicFilesystem(`relative\path`)
	if fs.root == `relative\path` || !strings.HasPrefix(fs.root, "\\\\?\\") {
		t.Errorf("%q == %q, expected absolutification", fs.root, `relative\path`)
	}
}

func TestResolveWindows83(t *testing.T) {
	fs, dir := setup(t)
	defer os.RemoveAll(dir)

	shortAbs, _ := fs.rooted("LFDATA~1")
	long := "LFDataTool"
	longAbs, _ := fs.rooted(long)
	deleted, _ := fs.rooted(filepath.Join("foo", "LFDATA~1"))
	notShort, _ := fs.rooted(filepath.Join("foo", "bar", "baz"))

	fd, err := fs.Create(long)
	if err != nil {
		t.Fatal(err)
	}
	fd.Close()

	if res := fs.resolveWin83(shortAbs); res != longAbs {
		t.Errorf(`Resolving for 8.3 names of "%v" resulted in "%v", expected "%v"`, shortAbs, res, longAbs)
	}
	if res := fs.resolveWin83(deleted); res != filepath.Dir(deleted) {
		t.Errorf(`Resolving for 8.3 names of "%v" resulted in "%v", expected "%v"`, deleted, res, filepath.Dir(deleted))
	}
	if res := fs.resolveWin83(notShort); res != notShort {
		t.Errorf(`Resolving for 8.3 names of "%v" resulted in "%v", expected "%v"`, notShort, res, notShort)
	}
}

func TestIsWindows83(t *testing.T) {
	fs, dir := setup(t)
	defer os.RemoveAll(dir)

	tempTop, _ := fs.rooted(TempName("baz"))
	tempBelow, _ := fs.rooted(filepath.Join("foo", "bar", TempName("baz")))
	short, _ := fs.rooted(filepath.Join("LFDATA~1", TempName("baz")))
	tempAndShort, _ := fs.rooted(filepath.Join("LFDATA~1", TempName("baz")))

	for _, f := range []string{tempTop, tempBelow} {
		if isMaybeWin83(f) {
			t.Errorf(`"%v" is not a windows 8.3 path"`, f)
		}
	}

	for _, f := range []string{short, tempAndShort} {
		if !isMaybeWin83(f) {
			t.Errorf(`"%v" is not a windows 8.3 path"`, f)
		}
	}
}

func TestRelUnrootedCheckedWindows(t *testing.T) {
	testCases := []struct {
		root        string
		abs         string
		expectedRel string
	}{
		{`c:\`, `c:\foo`, `foo`},
		{`C:\`, `c:\foo`, `foo`},
		{`C:\`, `C:\foo`, `foo`},
		{`c:\`, `C:\foo`, `foo`},
		{`\\?c:\`, `\\?c:\foo`, `foo`},
		{`\\?C:\`, `\\?c:\foo`, `foo`},
		{`\\?C:\`, `\\?C:\foo`, `foo`},
		{`\\?c:\`, `\\?C:\foo`, `foo`},
		{`c:\foo`, `c:\foo\bar`, `bar`},
		{`c:\foo`, `c:\foo\bAr`, `bAr`},
		{`c:\foO`, `c:\Foo\bar`, `bar`},
		{`c:\foO`, `c:\fOo\bAr`, `bAr`},
		{`c:\foO`, `c:\fOo`, ``},
		{`C:\foO`, `c:\fOo`, ``},
	}

	for _, tc := range testCases {
		if res := rel(tc.abs, tc.root); res != tc.expectedRel {
			t.Errorf(`rel("%v", "%v") == "%v", expected "%v"`, tc.abs, tc.root, res, tc.expectedRel)
		}

		// unrootedChecked really just wraps rel, and does not care about
		// the actual root of that filesystem, but should not panic on these
		// test cases.
		fs := BasicFilesystem{root: tc.root}
		if res := fs.unrootedChecked(tc.abs, tc.root); res != tc.expectedRel {
			t.Errorf(`unrootedChecked("%v", "%v") == "%v", expected "%v"`, tc.abs, tc.root, res, tc.expectedRel)
		}
		fs = BasicFilesystem{root: strings.ToLower(tc.root)}
		if res := fs.unrootedChecked(tc.abs, tc.root); res != tc.expectedRel {
			t.Errorf(`unrootedChecked("%v", "%v") == "%v", expected "%v"`, tc.abs, tc.root, res, tc.expectedRel)
		}
		fs = BasicFilesystem{root: strings.ToUpper(tc.root)}
		if res := fs.unrootedChecked(tc.abs, tc.root); res != tc.expectedRel {
			t.Errorf(`unrootedChecked("%v", "%v") == "%v", expected "%v"`, tc.abs, tc.root, res, tc.expectedRel)
		}
	}
}
