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

// Small fixtures: the phrases classify looks at, not whole license texts.
const (
	mitText       = "MIT License\n\nCopyright (c) 2020 Someone\n\nPermission is hereby granted, free of charge, to any person obtaining a copy\nof this software ... subject to the following conditions:\n\nTHE SOFTWARE IS PROVIDED \"AS IS\", WITHOUT WARRANTY OF ANY KIND."
	apacheText    = "                                 Apache License\n                           Version 2.0, January 2004\n                        http://www.apache.org/licenses/\n\n   TERMS AND CONDITIONS FOR USE, REPRODUCTION, AND DISTRIBUTION"
	bsdText       = "Copyright (c) 2014 Someone\nAll rights reserved.\n\nRedistribution and use in source and binary forms, with or without\nmodification, are permitted provided that the following conditions are met:"
	mplText       = "Mozilla Public License Version 2.0\n==================================\n\n1.12. \"Secondary License\"\n    means either the GNU General Public License, Version 2.0, the GNU\n    Lesser General Public License, Version 2.1, the GNU Affero General\n    Public License, Version 3.0, or any later versions of those\n    licenses.\n\nExhibit B - \"Incompatible With Secondary Licenses\" Notice"
	gplText       = "                    GNU GENERAL PUBLIC LICENSE\n                       Version 2, June 1991\n\n Copyright (C) 1989, 1991 Free Software Foundation, Inc."
	commonsClause = "\"Commons Clause\" License Condition v1.0\n\nThe Software is provided to you by the Licensor under the License, as defined below, subject to the following condition.\n\nWithout limiting other conditions in the License, the grant of rights under the License will not include, and the License does not grant to you, the right to Sell the Software."
)

func TestClassify(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"MIT", mitText, "MIT License"},
		{"Apache", apacheText, "Apache License 2.0"},
		{"BSD", bsdText, "BSD License"},
		{"MPL naming the GNU licenses as secondary licenses", mplText, "Mozilla Public License 2.0"},
		{"MPL with CRLF line ends", strings.ReplaceAll(mplText, "\n", "\r\n"), "Mozilla Public License 2.0"},
		{"Apache and BSD in one file", apacheText + "\n\n" + bsdText, "Apache License 2.0"},
		{"ISC", "ISC License\n\nPermission to use, copy, modify, and/or distribute this software for any\npurpose with or without fee is hereby granted", "ISC License"},
		{"unknown text", "All rights reserved. Ask us before you use this.", ""},
		{"empty", "", ""},

		// Riders and source-available licenses that name a permissive one.
		{"Apache with a Commons Clause rider", commonsClause + "\n\nLicense: Apache 2.0\n\n" + apacheText, ""},
		{"MIT with the Commons Clause condition, unnamed", mitText + "\n\nThe License does not grant to you, the right to Sell the Software.", ""},
		{"BUSL changing to Apache", "Business Source License 1.1\n\nParameters\n\nLicensor: Someone\nChange Date: 2030-01-01\nChange License: Apache License, Version 2.0", ""},
		{"BUSL changing to the MPL", "License text copyright (c) 2023 MariaDB plc, All Rights Reserved.\n\"Business Source License\" is a trademark of MariaDB plc.\n\nChange License: Mozilla Public License, version 2.0", ""},
		{"BUSL by its SPDX name", "SPDX-License-Identifier: BUSL-1.1\n" + apacheText, ""},
		{"SSPL naming Apache", "                     Server Side Public License\n                     VERSION 1, OCTOBER 16, 2018\n\nParts of this program are under the Apache License, Version 2.0", ""},
		{"SSPL by its SPDX name", "SPDX-License-Identifier: SSPL-1.0 OR Apache-2.0\n" + apacheText, ""},
		{"Elastic License", "Elastic License 2.0 (ELv2)\n\nAcceptance\nBy using the software, you agree to all of the terms and conditions below.", ""},
		{"Functional Source License changing to Apache", "Functional Source License, Version 1.1, Apache 2.0 Future License\n\n" + apacheText, ""},
		{"MIT with a no-evil rider (JSON license)", mitText + "\n\nThe Software shall be used for Good, not Evil.", ""},
		{"MIT for non-commercial use only", mitText + "\n\nFor non-commercial use only.", ""},

		// GNU licenses, also next to a permissive one.
		{"GPL", gplText, ""},
		{"MIT or GPL (dual license)", "This project is dual-licensed under the MIT License or the GNU General Public License v2.\n\n" + mitText, ""},
		{"MIT file combined with GPL code", mitText + "\n\nThe file decode.c is licensed under GPLv2.", ""},
		{"BSD with an LGPL part", bsdText + "\n\nSPDX-License-Identifier: LGPL-2.1-or-later", ""},
		{"MPL with an extra GPL grant", mplText + "\n\nThis file may also be used under the terms of the GNU General Public License version 2.", ""},
		{"AGPL", "GNU AFFERO GENERAL PUBLIC LICENSE\nVersion 3, 19 November 2007", ""},
	} {
		if got := classify(c.text); got != c.want {
			t.Errorf("%s: classify = %q, want %q", c.name, got, c.want)
		}
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
