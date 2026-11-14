package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompare(t *testing.T) {
	t.Run("1.0 vs 1.1", testCompareFunc(SemanticVersion{major: 1}, SemanticVersion{major: 1, minor: 1}, -1))
	t.Run("1.0.1 vs 1.1", testCompareFunc(SemanticVersion{major: 1, patch: 1}, SemanticVersion{major: 1, minor: 1}, -1))
	t.Run("1.1.1 vs 1.1", testCompareFunc(SemanticVersion{major: 1, minor: 1, patch: 1}, SemanticVersion{major: 1, minor: 1}, 1))
	t.Run("1.2 vs 1.1", testCompareFunc(SemanticVersion{major: 1, minor: 2}, SemanticVersion{major: 1, minor: 1}, 1))
	t.Run("2.0 vs 1.1", testCompareFunc(SemanticVersion{major: 2}, SemanticVersion{major: 1, minor: 1}, 1))
	t.Run("1.1.1 vs 1.1.1-rc", testCompareFunc(SemanticVersion{major: 1, minor: 1, patch: 1}, SemanticVersion{major: 1, minor: 1, patch: 1, pre: "rc"}, 1))
	t.Run("1.1000.0 vs 2.0.0", testCompareFunc(SemanticVersion{major: 1, minor: 1000}, SemanticVersion{major: 2}, -1))
	t.Run("1.0.1000 vs 1.1.0", testCompareFunc(SemanticVersion{major: 1, patch: 1000}, SemanticVersion{major: 1, minor: 1}, -1))
	t.Run("1.2.3 vs 1.2.3", testCompareFunc(SemanticVersion{major: 1, minor: 2, patch: 3}, SemanticVersion{major: 1, minor: 2, patch: 3}, 0))
	t.Run("1.3.4-rc vs 1.3.4-rc.1", testCompareFunc(SemanticVersion{major: 1, minor: 3, patch: 4, pre: "rc"}, SemanticVersion{major: 1, minor: 3, patch: 4, pre: "rc", itr: 1}, -1))
}

func testCompareFunc(a SemanticVersion, b SemanticVersion, expected int) func(*testing.T) {
	return func(t *testing.T) {
		actual := a.Compare(b)
		if actual != expected {
			t.Errorf("Expected comparison between %s and %s was expected to be %d but instead got %d", a.String(), b.String(), expected, actual)
		}
	}
}

func TestExtractVersion(t *testing.T) {
	testMap := map[string]string{"v1_22.json": "1.22.0", "v1_8_1": "1.8.1", "v1_0_33-rc.json": "1.0.33-rc", "2_2_2.json": "2.2.2",
		"3_2_3-rc_1": "3.2.3-rc.1", "v4_124_822-SNAPSHOT": "4.124.822-SNAPSHOT", "1.3.2": "1.3.2", "2.6.24-rc": "2.6.24-rc",
		// pre-release labels ending in letters from ".json" were stripped by TrimRight
		"v1_0_0-son.json": "1.0.0-son", "v3_0_0-json_2.json": "3.0.0-json.2"}
	for k, v := range testMap {
		t.Run(k, testExtractVersionFunc(k, v))
	}
}

func testExtractVersionFunc(txt string, expected string) func(*testing.T) {
	return func(t *testing.T) {
		v := ExtractVersion(txt)
		if v.String() != expected {
			t.Errorf("Expected that the string %s would return %s but instead returned %s", txt, expected, v.String())
		}
	}
}

func TestNewerVersions(t *testing.T) {
	allVersions := []string{"/workdir/deltas/v1_0_1.json", "v1_0_2.json", "v1_0_3.json", "v1_1_0.json", "v1_2.json", "v1_2_1.json", "v1_2_2.json"}
	outOfOrder := []string{"/workdir/deltas/v1_0_10.json", "/workdir/deltas/v1_0_9.json"}
	correctOrder := []string{"/workdir/deltas/v1_0_9.json", "/workdir/deltas/v1_0_10.json"}
	t.Run("1.2.0", testNewerVersionsFunc(ExtractVersion("1.2.0"), allVersions, 2))
	t.Run("1.2.1", testNewerVersionsFunc(ExtractVersion("1.2.1"), allVersions, 1))
	t.Run("1.2.2", testNewerVersionsFunc(ExtractVersion("1.2.2"), allVersions, 0))
	t.Run("NewVersionOrdered", testNewerVersionSortFunc(ExtractVersion("1.0.8"), outOfOrder, correctOrder))
	t.Run("NewVersionOrdered", testNewerVersionSortFunc(ExtractVersion("1.0.8"), correctOrder, correctOrder))
}

func testNewerVersionsFunc(current SemanticVersion, all []string, expected int) func(t *testing.T) {
	return func(t *testing.T) {
		v := NewerVersions(current, all)
		actual := len(v)
		if actual != expected {
			t.Errorf("Expected number of newer versions to be %d but instead returned %d", expected, actual)
		}
	}
}
func testNewerVersionSortFunc(current SemanticVersion, all []string, expected []string) func(t *testing.T) {
	return func(t *testing.T) {
		v := NewerVersions(current, all)
		for idx, path := range v {
			if path != expected[idx] {
				t.Errorf("Expected path value at index %d to be %s but the actual value was %s", idx, expected[idx], path)
			}
		}
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	if err := WriteFileAtomic(path, []byte("one"), 0o640); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("new file - EXPECTED mode 640  ACTUAL: %o", info.Mode().Perm())
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(content) != "two" || info.Mode().Perm() != 0o600 {
		t.Errorf("existing file - EXPECTED content two with mode 600 kept  ACTUAL: %q %o", content, info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("EXPECTED no temporary files left  ACTUAL: %d entries", len(entries))
	}
}

func TestBumpAndFileName(t *testing.T) {
	cases := []struct{ from, part, next, file string }{
		{"1.0.3", "patch", "1.0.4", "v1_0_4.json"},
		{"1.0.3", "minor", "1.1.0", "v1_1_0.json"},
		{"1.4.3", "major", "2.0.0", "v2_0_0.json"},
		{"1.0.4-rc.1", "patch", "1.0.4", "v1_0_4.json"},
		{"1.0.4-rc.1", "minor", "1.1.0", "v1_1_0.json"},
	}
	for _, c := range cases {
		next, err := ExtractVersion(c.from).Bump(c.part)
		if err != nil || next.String() != c.next || next.FileName() != c.file {
			t.Errorf("%s bump %s - EXPECTED %s (%s)  ACTUAL: %s (%s) %v", c.from, c.part, c.next, c.file, next.String(), next.FileName(), err)
		}
	}
	if _, err := ExtractVersion("1.0.0").Bump("build"); err == nil {
		t.Error("EXPECTED an error for an unknown part")
	}
	// file names round-trip through ExtractVersion
	for _, version := range []string{"1.0.4", "1.1.0-rc.1", "2.0.0-beta", "0.9.12"} {
		v := ExtractVersion(version)
		if again := ExtractVersion(v.FileName()); again.Compare(v) != 0 || again.String() != v.String() {
			t.Errorf("%s - file name %s parses back as %s", version, v.FileName(), again.String())
		}
	}
}
