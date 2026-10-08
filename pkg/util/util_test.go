package util

import (
	"testing"
)

func TestVersum(t *testing.T) {
	t.Run("1.0.1", testVersumFunc(SemanticVersion{major: 1, minor: 0, patch: 1}, int64(1000001)))
	t.Run("1.2.31", testVersumFunc(SemanticVersion{major: 1, minor: 2, patch: 31}, int64(1002031)))
	t.Run("1.4", testVersumFunc(SemanticVersion{major: 1, minor: 4}, int64(1004000)))
	t.Run("10.126.422-rc.3", testVersumFunc(SemanticVersion{major: 10, minor: 126, patch: 422, pre: "rc", itr: 3}, int64(10126422)))
}

func testVersumFunc(v SemanticVersion, expected int64) func(*testing.T) {
	return func(t *testing.T) {
		actual := v.versum()
		if actual != expected {
			t.Errorf("Expected the sum of %s to be %d but instead got %d", v.String(), expected, actual)
		}
	}
}

func TestCompare(t *testing.T) {
	t.Run("1.0 vs 1.1", testCompareFunc(SemanticVersion{major: 1}, SemanticVersion{major: 1, minor: 1}, -1))
	t.Run("1.0.1 vs 1.1", testCompareFunc(SemanticVersion{major: 1, patch: 1}, SemanticVersion{major: 1, minor: 1}, -1))
	t.Run("1.1.1 vs 1.1", testCompareFunc(SemanticVersion{major: 1, minor: 1, patch: 1}, SemanticVersion{major: 1, minor: 1}, 1))
	t.Run("1.2 vs 1.1", testCompareFunc(SemanticVersion{major: 1, minor: 2}, SemanticVersion{major: 1, minor: 1}, 1))
	t.Run("2.0 vs 1.1", testCompareFunc(SemanticVersion{major: 2}, SemanticVersion{major: 1, minor: 1}, 1))
	t.Run("1.1.1 vs 1.1.1-rc", testCompareFunc(SemanticVersion{major: 1, minor: 1, patch: 1}, SemanticVersion{major: 1, minor: 1, patch: 1, pre: "rc"}, 1))
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
		"3_2_3-rc_1": "3.2.3-rc.1", "v4_124_822-SNAPSHOT": "4.124.822-SNAPSHOT", "1.3.2": "1.3.2", "2.6.24-rc": "2.6.24-rc"}
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
