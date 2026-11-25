package safeshell

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func argvs(p Parsed) [][]string {
	var out [][]string
	for _, c := range p.Commands {
		if len(c.Argv) > 0 {
			out = append(out, c.Argv)
		}
	}
	return out
}

func TestParseResolvesQuoting(t *testing.T) {
	cases := map[string][]string{
		`'g''it' push`:          {"git", "push"},
		`g\it push`:             {"git", "push"},
		`"g"it "a b"`:           {"git", "a b"},
		`echo "a\$b" 'c\d'`:     {"echo", "a$b", `c\d`},
		"git st\\\natus":        {"git", "status"},
		`FOO=1 BAR="x y" ls -l`: {"ls", "-l"},
		`ls -la  # comment`:     {"ls", "-la"},
	}
	for in, want := range cases {
		p := Parse(in)
		if p.Complex {
			t.Errorf("Parse(%q) complex: %v", in, p.Reasons)
			continue
		}
		if got := p.Commands[0].Argv; !reflect.DeepEqual(got, want) {
			t.Errorf("Parse(%q) argv = %q, want %q", in, got, want)
		}
	}
}

func TestParseComplexAndStillExtracts(t *testing.T) {
	cases := []struct {
		in      string
		complex bool
		bin     string // a command that must still be extracted
	}{
		{"git status $(rm x)", true, "rm"},
		{"git status `rm x`", true, "rm"},
		{"cat <(rm x)", true, "rm"},
		{"(rm x)", true, "rm"},
		{"{ rm x; }", true, "rm"},
		{"f(){ rm x; }; f", true, "rm"},
		{"env git push", false, "git"},
		{"sudo -u root git push", false, "git"},
		{"timeout 5 nice -n 5 git push", false, "git"},
		{"bash -c 'git push'", true, "git"},
		{`eval "git push"`, true, "git"},
		{"xargs -n1 rm", true, "rm"},
		{`find . -exec rm {} \;`, false, "rm"},
		{"git status & rm x", true, "rm"},
		{"a && b || c | d; e", false, "e"},
		{"git status\nrm x", false, "rm"},
		{"$CMD x", true, ""},
		{`$'g\x69t' push`, true, ""}, // dash has no $'...', so sh and bash disagree
		{"git status\x00", true, "git"},
	}
	for _, c := range cases {
		p := Parse(c.in)
		if p.Complex != c.complex {
			t.Errorf("Parse(%q).Complex = %v (%v), want %v", c.in, p.Complex, p.Reasons, c.complex)
		}
		if c.bin == "" {
			continue
		}
		found := false
		for _, cmd := range p.Commands {
			found = found || cmd.Bin() == c.bin
		}
		if !found {
			t.Errorf("Parse(%q) did not extract %q: %s", c.in, c.bin, p)
		}
	}
}

func TestParseRedirects(t *testing.T) {
	p := Parse("cmd 2>&1 > out.txt < in.txt >> log")
	if p.Complex {
		t.Fatal(p.Reasons)
	}
	var got []string
	for _, r := range p.Commands[0].Redirects {
		kind := "other"
		switch {
		case r.Dup:
			kind = "dup"
		case r.Write:
			kind = "write"
		case r.Read:
			kind = "read"
		}
		got = append(got, r.Op+" "+r.Target+" "+kind)
	}
	want := []string{">& 1 dup", "> out.txt write", "< in.txt read", ">> log write"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redirects = %q, want %q", got, want)
	}
}

func TestExploreBypassTable(t *testing.T) {
	for _, cmd := range BypassAttempts {
		if ok, _ := IsExploreReadOnlyShell(cmd); ok {
			t.Errorf("explore mode accepted %q", cmd)
		}
	}
	for _, cmd := range SafeCommands {
		if ok, why := IsExploreReadOnlyShell(cmd); !ok && !strings.Contains(cmd, "/dev/null") {
			t.Errorf("explore mode rejected %q: %s", cmd, why)
		}
	}
}

func TestGitSubcommandSkipsGlobals(t *testing.T) {
	sub, idx, ok, _ := GitSubcommand([]string{"-C", "dir", "--no-pager", "log", "-3"})
	if !ok || sub != "log" || idx != 3 {
		t.Fatalf("got %q %d %v", sub, idx, ok)
	}
	if _, _, ok, _ := GitSubcommand([]string{"-c", "core.pager=sh", "status"}); ok {
		t.Fatal("git -c must not yield a trusted subcommand")
	}
	got := GitStripGlobals([]string{"git", "-c", "a=b", "-C", "d", "--no-pager", "push"}, true)
	if !reflect.DeepEqual(got, []string{"git", "push"}) {
		t.Fatalf("strip all = %q", got)
	}
	got = GitStripGlobals([]string{"git", "-C", "d", "-c", "a=b", "push"}, false)
	if !reflect.DeepEqual(got, []string{"git", "-c", "a=b", "push"}) {
		t.Fatalf("strip safe = %q", got)
	}
}

func TestNoNetworkProbesInAllowlist(t *testing.T) {
	for _, bin := range []string{"dig", "nslookup", "host", "ping", "curl", "wget", "nc", "ncat", "telnet", "ssh", "scp", "traceroute", "whois"} {
		if exploreShellAllowedBins[bin] {
			t.Errorf("%s can exfiltrate data and must not be in the read-only allowlist", bin)
		}
		if ok, _ := IsExploreReadOnlyShell(bin + " example.com"); ok {
			t.Errorf("explore mode accepted %s", bin)
		}
	}
}

// requote renders argv so that parsing it again must give the same words.
func requote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}

func FuzzExtract(f *testing.F) {
	for _, s := range BypassAttempts {
		f.Add(s)
	}
	for _, s := range SafeCommands {
		f.Add(s)
	}
	f.Add("echo 'a'\"b\"$'c'")
	f.Fuzz(func(t *testing.T, src string) {
		p := Parse(src) // must not panic
		if p.Complex || !utf8.ValidString(src) {
			return
		}
		// Fully understood: re-printing the commands and re-parsing must give
		// the same argv list.
		var lines []string
		for _, c := range p.Commands {
			if c.Inner {
				continue
			}
			for _, d := range c.Dyn {
				if d {
					return // the source text of $X is not a literal word
				}
			}
			if len(c.Argv) > 0 {
				lines = append(lines, requote(c.Argv))
			}
		}
		q := Parse(strings.Join(lines, "\n"))
		if !reflect.DeepEqual(argvs(p), argvs(q)) {
			t.Fatalf("round trip mismatch for %q:\n first: %q\nsecond: %q\n(reprinted %q)", src, argvs(p), argvs(q), strings.Join(lines, "\n"))
		}
	})
}
