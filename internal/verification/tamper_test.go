package verification

import (
	"strings"
	"testing"
)

func TestTestIntegrityWarnings(t *testing.T) {
	goBefore := "package x\nfunc TestA(t *testing.T) { if f() != 1 { t.Fatal(\"a\") } }\nfunc TestB(t *testing.T) { if g() { t.Errorf(\"b\") }; if h() { t.Error(\"c\") } }\n"
	cases := []struct {
		name   string
		change TestFileChange
		want   string // substring; "" = no warning
	}{
		{"deleted test file", TestFileChange{Path: "pkg/a_test.go", Before: goBefore, BeforeExists: true}, "deleted"},
		{"removed test func", TestFileChange{Path: "a_test.go", Before: goBefore, BeforeExists: true, AfterExists: true,
			After: "package x\nfunc TestA(t *testing.T) { if f() != 1 { t.Fatal(\"a\") } }\n"}, "test cases went from 2 to 1"},
		{"added skip", TestFileChange{Path: "a_test.go", Before: goBefore, BeforeExists: true, AfterExists: true,
			After: strings.Replace(goBefore, "{ if g()", "{ t.Skip(\"flaky\"); if g()", 1)}, "skip"},
		{"gutted assertions", TestFileChange{Path: "a_test.go", Before: goBefore, BeforeExists: true, AfterExists: true,
			After: "package x\nfunc TestA(t *testing.T) { f() }\nfunc TestB(t *testing.T) { g() }\n"}, "assertions went from 3 to 0"},
		{"python xfail", TestFileChange{Path: "tests/test_x.py", BeforeExists: true, AfterExists: true,
			Before: "def test_a():\n    assert f()\n", After: "import pytest\n@pytest.mark.xfail\ndef test_a():\n    assert f()\n"}, "skip/xfail"},
		{"js it.skip", TestFileChange{Path: "src/a.test.ts", BeforeExists: true, AfterExists: true,
			Before: "it('a', () => { expect(f()).toBe(1) })\n", After: "it.skip('a', () => { expect(f()).toBe(1) })\n"}, "skip"},
		{"new conftest", TestFileChange{Path: "conftest.py", AfterExists: true, After: "def pytest_runtest_makereport(): ..."}, "conftest"},
		{"ci edited", TestFileChange{Path: ".github/workflows/ci.yml", BeforeExists: true, AfterExists: true, Before: "a", After: "b"}, "CI configuration"},
		{"new test file is fine", TestFileChange{Path: "b_test.go", AfterExists: true, After: goBefore}, ""},
		{"added a test is fine", TestFileChange{Path: "a_test.go", Before: goBefore, BeforeExists: true, AfterExists: true,
			After: goBefore + "func TestC(t *testing.T) { t.Fatal(\"c\") }\n"}, ""},
		{"non-test file ignored", TestFileChange{Path: "main.go", Before: "x", BeforeExists: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TestIntegrityWarnings([]TestFileChange{tc.change})
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want no warning, got %v", got)
				}
				return
			}
			if len(got) == 0 || !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Fatalf("want warning containing %q, got %v", tc.want, got)
			}
		})
	}
}

func TestIsTestPath(t *testing.T) {
	for path, want := range map[string]bool{
		"a_test.go": true, "tests/test_a.py": true, "a.spec.tsx": true, "conftest.py": true,
		".github/workflows/ci.yml": true, "main.go": false, "testdata/x.txt": false,
	} {
		if got := IsTestPath(path); got != want {
			t.Errorf("IsTestPath(%q) = %v, want %v", path, got, want)
		}
	}
}
