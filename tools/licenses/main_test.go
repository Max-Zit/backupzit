package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeaderComment(t *testing.T) {
	// The notice ends at the first line that is not a comment; CRLF files
	// give the same text.
	src := "// Copyright (c) 2014 - Someone\r\n//\r\n// Redistribution and use in source and binary forms\r\n//    are permitted.\r\n\r\n// Package scram implements SCRAM.\r\npackage scram\r\n"
	want := "Copyright (c) 2014 - Someone\n\nRedistribution and use in source and binary forms\n   are permitted."
	if got := headerComment(src); got != want {
		t.Errorf("headerComment = %q, want %q", got, want)
	}
	if got := headerComment(strings.ReplaceAll(src, "\r\n", "\n")); got != want {
		t.Errorf("headerComment (LF) = %q, want %q", got, want)
	}
	// No comment at the top: no notice.
	if got := headerComment("package edge\n\n// This code has been adapted from elsewhere.\n"); got != "" {
		t.Errorf("headerComment without a header = %q, want nothing", got)
	}
}

func TestSameNotice(t *testing.T) {
	lic := "The MIT License (MIT)\n\nCopyright © 2013-2017 Someone\n\nPermission is hereby granted, free of charge, to any person obtaining a copy\nof this software.\n"
	src := "package edge\n\n// This code has been adapted from: https://example.com/project\n\n/*\n\nThe MIT License (MIT)\n\nCopyright © 2013-2017 Someone\n\nPermission is hereby granted, free of charge, to any\nperson obtaining a copy of this software.\n\n*/\n"
	if !sameNotice(src, lic) {
		t.Error("the same notice with other line breaks was not recognized")
	}
	if sameNotice(strings.Replace(src, "2017 Someone", "2017 Someone else", 1), lic) {
		t.Error("a notice naming another copyright holder passed as the same")
	}
	if sameNotice(src, "") {
		t.Error("a missing license file passed as the same notice")
	}
}

func TestEmbeddedFiles(t *testing.T) {
	// The paths are relative to the repository root, where the tool runs.
	for _, f := range embedded {
		b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(f.file)))
		if err != nil || len(strings.TrimSpace(string(b))) == 0 {
			t.Errorf("%s: %v (or empty)", f.file, err)
		}
	}
	b, err := os.ReadFile("ms-rl.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Microsoft Reciprocal License (MS-RL)", "(A) Reciprocal Grants-", "(F) The software is licensed \"as-is.\""} {
		if !strings.Contains(string(b), s) {
			t.Errorf("ms-rl.txt lacks %q", s)
		}
	}
}
