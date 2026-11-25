package safeshell

// parse.go turns a shell command line into the list of simple commands it
// runs, using a real shell parser (mvdan.cc/sh) instead of splitting strings.
//
// The contract is fail-closed. Parse returns every simple command it could
// read as literal argv, and sets Complex whenever anything was seen that it
// cannot reduce to literals (substitutions, compound commands, backgrounding,
// code-running builtins, unparseable text, odd bytes ...). Callers treat
// Complex as "not covered by an allow rule, rejected in explore mode", while
// deny/ask rules still match on whatever commands were extractable.
//
// Commands are executed with `sh -c` (see tools/sandbox.go). We parse with
// the POSIX grammar, and ALSO with the bash grammar: /bin/sh is dash on some
// systems and bash on others (macOS), and a line the two dialects read
// differently (brace expansion, `&>`, `<(...)`, `[[`) is exactly the sort of
// parser/executor disagreement that bypasses allowlists. If the dialects do
// not agree exactly, the line is Complex.

import (
	"fmt"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"mvdan.cc/sh/v3/syntax"
)

// Assign is a `NAME=value` prefix on a command, or a bare assignment.
type Assign struct {
	Name  string
	Value string
	Dyn   bool // value contains a parameter expansion
}

// Redirect is one redirection on a command.
type Redirect struct {
	Op      string // ">", ">>", "<", ">&", "<<", "<<<", "&>", ...
	FD      string // explicit descriptor ("2" in 2>file), or ""
	Target  string // literal target, or its source text when Dyn
	Write   bool   // can create, truncate or append to a file
	Read    bool   // reads a file
	Dup     bool   // descriptor duplication or close (2>&1, >&-)
	Heredoc bool   // << / <<- / <<< (Target is empty)
	Dyn     bool
}

// Command is one simple command with its words resolved to literals.
type Command struct {
	Argv      []string // quotes and escapes resolved; `$X` stays as source text when Dyn
	Dyn       []bool   // Dyn[i]: Argv[i] contains a parameter expansion
	Glob      []bool   // Glob[i]: Argv[i] has unquoted glob characters
	Assigns   []Assign
	Redirects []Redirect
	// Inner marks a command that was found inside another one: the program a
	// wrapper (env, nice, timeout, sudo ...) runs, or the script text given to
	// `sh -c`, `eval` or `find -exec`. It is present so deny rules can see it.
	Inner bool
}

// Parsed is the result of Parse.
type Parsed struct {
	Commands []Command
	// Complex is set when anything was not fully understood.
	Complex bool
	Reasons []string
	// SetsVars is set when a bare assignment (`X=1; ...`) precedes later
	// commands, so a later `$X` is not the user's environment any more.
	SetsVars bool
}

// Understood reports whether the whole line was reduced to literal commands.
func (p Parsed) Understood() bool { return !p.Complex }

func (p *Parsed) fail(reason string) {
	p.Complex = true
	if slices.Contains(p.Reasons, reason) {
		return
	}
	p.Reasons = append(p.Reasons, reason)
}

// Reason is a short human-readable cause for Complex, or "".
func (p Parsed) Reason() string {
	if len(p.Reasons) == 0 {
		return ""
	}
	return p.Reasons[0]
}

const maxParseDepth = 4

// Parse extracts the simple commands from a shell command line. See the file
// comment for the fail-closed contract.
func Parse(src string) Parsed { return parseDepth(src, 0) }

func parseDepth(src string, depth int) Parsed {
	var out Parsed
	if r := badInput(src); r != "" {
		out.fail(r)
	}
	if depth > maxParseDepth {
		out.fail("commands nested too deeply")
		return out
	}
	posix, perr := extract(src, syntax.LangPOSIX, depth)
	bash, berr := extract(src, syntax.LangBash, depth)
	switch {
	case perr != nil && berr != nil:
		out.fail("could not parse the command (" + perr.Error() + ")")
	case perr == nil && berr != nil:
		out.merge(posix)
		out.fail("not valid bash, so sh and bash may read it differently")
	case perr != nil && berr == nil:
		out.merge(bash)
		out.fail("bash-only syntax that sh does not accept")
	default:
		out.merge(posix)
		if !reflect.DeepEqual(posix, bash) {
			// Keep bash's extra commands visible to deny rules.
			for _, c := range bash.Commands {
				if !hasCommand(out.Commands, c) {
					out.Commands = append(out.Commands, c)
				}
			}
			out.fail("sh and bash read this command differently")
		}
	}
	return out
}

func hasCommand(list []Command, c Command) bool {
	for _, x := range list {
		if reflect.DeepEqual(x, c) {
			return true
		}
	}
	return false
}

func (p *Parsed) merge(o Parsed) {
	p.Commands = append(p.Commands, o.Commands...)
	p.SetsVars = p.SetsVars || o.SetsVars
	if o.Complex {
		p.Complex = true
		for _, r := range o.Reasons {
			p.fail(r)
		}
	}
}

// badInput rejects bytes whose meaning differs between the parser, the shell
// and the person reading the approval prompt.
func badInput(src string) string {
	if !utf8.ValidString(src) {
		return "invalid UTF-8"
	}
	for _, r := range src {
		switch {
		case r == 0:
			return "NUL byte"
		case r == '\t' || r == '\n' || r == ' ':
		case r < 0x20 || r == 0x7f:
			return "control character"
		case r > 0x7f && (unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) || unicode.IsControl(r)):
			return "unicode whitespace or invisible character"
		}
	}
	return ""
}

type extractor struct {
	out   Parsed
	depth int
	lang  syntax.LangVariant
}

func extract(src string, lang syntax.LangVariant, depth int) (Parsed, error) {
	f, err := syntax.NewParser(syntax.Variant(lang)).Parse(strings.NewReader(src), "")
	if err != nil {
		return Parsed{}, err
	}
	e := &extractor{depth: depth, lang: lang}
	syntax.Walk(f, e.visit)
	if e.out.SetsVars {
		// A bare assignment only matters if some other command follows it.
		n := 0
		for _, c := range e.out.Commands {
			if len(c.Argv) > 0 {
				n++
			}
		}
		e.out.SetsVars = n > 0
	}
	return e.out, nil
}

func (e *extractor) visit(n syntax.Node) bool {
	switch n := n.(type) {
	case *syntax.Stmt:
		e.stmt(n)
	case *syntax.Subshell:
		e.out.fail("subshell")
	case *syntax.Block:
		e.out.fail("command group { ... }")
	case *syntax.IfClause, *syntax.WhileClause, *syntax.ForClause, *syntax.CaseClause:
		e.out.fail("control-flow construct (if/while/for/case)")
	case *syntax.FuncDecl:
		e.out.fail("function definition")
	case *syntax.ArithmCmd, *syntax.LetClause:
		e.out.fail("arithmetic command")
	case *syntax.TestClause, *syntax.TestDecl:
		e.out.fail("test clause")
	case *syntax.DeclClause:
		e.out.fail("declaration command")
	case *syntax.TimeClause:
		e.out.fail("time clause")
	case *syntax.CoprocClause:
		e.out.fail("coprocess")
	case *syntax.CmdSubst:
		e.out.fail("command substitution")
	case *syntax.ProcSubst:
		e.out.fail("process substitution")
	case *syntax.ArithmExp:
		e.out.fail("arithmetic expansion")
	case *syntax.ExtGlob:
		e.out.fail("extended glob")
	case *syntax.BraceExp:
		e.out.fail("brace expansion")
	case *syntax.ArrayExpr:
		e.out.fail("array assignment")
	}
	return true
}

func (e *extractor) stmt(s *syntax.Stmt) {
	if s.Background || s.Disown {
		e.out.fail("background command (&)")
	}
	if s.Coprocess {
		e.out.fail("coprocess")
	}
	call, isCall := s.Cmd.(*syntax.CallExpr)
	if s.Cmd != nil && !isCall {
		// Compound or binary command: its children are visited on their own;
		// only redirections on this statement are ours.
		if len(s.Redirs) == 0 {
			return
		}
	}
	var c Command
	if isCall {
		for _, a := range call.Assigns {
			c.Assigns = append(c.Assigns, e.assign(a))
		}
		for i, w := range call.Args {
			r := e.word(w)
			if i == 0 && (r.dyn || r.glob) {
				e.out.fail("command name is not a literal")
			}
			c.Argv = append(c.Argv, r.val)
			c.Dyn = append(c.Dyn, r.dyn)
			c.Glob = append(c.Glob, r.glob)
		}
		if len(c.Argv) == 0 && len(c.Assigns) > 0 {
			e.out.SetsVars = true
		}
	}
	for _, r := range s.Redirs {
		c.Redirects = append(c.Redirects, e.redirect(r))
	}
	if len(c.Argv) == 0 && len(c.Assigns) == 0 && len(c.Redirects) == 0 {
		return
	}
	e.out.Commands = append(e.out.Commands, c)
	if len(c.Argv) > 0 {
		e.unwrap(c)
	}
}

func (e *extractor) assign(a *syntax.Assign) Assign {
	out := Assign{}
	if a.Name != nil {
		out.Name = a.Name.Value
	}
	if a.Index != nil {
		e.out.fail("array element assignment")
	}
	if a.Array != nil {
		e.out.fail("array assignment")
	}
	if a.Value != nil {
		r := e.word(a.Value)
		out.Value, out.Dyn = r.val, r.dyn
	}
	return out
}

func (e *extractor) redirect(r *syntax.Redirect) Redirect {
	out := Redirect{Op: r.Op.String()}
	if r.N != nil {
		out.FD = r.N.Value
	}
	switch r.Op {
	case syntax.Hdoc, syntax.DashHdoc:
		out.Heredoc, out.Read = true, true
		if r.Hdoc != nil {
			e.word(r.Hdoc) // flags substitutions inside an unquoted body
		}
		return out
	case syntax.WordHdoc:
		out.Heredoc, out.Read = true, true
		if r.Word != nil {
			e.word(r.Word)
		}
		return out
	}
	t := e.word(r.Word)
	out.Target, out.Dyn = t.val, t.dyn
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.RdrAll, syntax.AppAll, syntax.RdrInOut:
		out.Write = true
		out.Read = r.Op == syntax.RdrInOut
	case syntax.RdrIn:
		out.Read = true
	case syntax.DplOut, syntax.DplIn:
		if t.val == "-" || allDigits(t.val) && !t.dyn {
			out.Dup = true
		} else if r.Op == syntax.DplOut {
			out.Write = true // `>&file` writes to file
		} else {
			out.Read = true
		}
	default:
		e.out.fail("unknown redirection " + out.Op)
	}
	return out
}

func allDigits(s string) bool { return AllASCIIDigits(s) }

type wordRes struct {
	val  string
	dyn  bool
	glob bool
}

func (e *extractor) word(w *syntax.Word) wordRes {
	var r wordRes
	if w != nil {
		// The parser leaves {a,b} and {1..3} as literal text; the shell
		// expands them into several words. Split a copy: Walk must not see
		// the BraceExp nodes. A lone `{}` (find -exec) is not an expansion.
		cp := &syntax.Word{Parts: slices.Clone(w.Parts)}
		syntax.SplitBraces(cp)
		for _, p := range cp.Parts {
			if b, ok := p.(*syntax.BraceExp); ok && (b.Sequence || len(b.Elems) > 1) {
				e.out.fail("brace expansion")
			}
		}
		e.parts(w.Parts, false, &r)
	}
	return r
}

func (e *extractor) parts(ps []syntax.WordPart, dq bool, r *wordRes) {
	for _, p := range ps {
		switch p := p.(type) {
		case *syntax.Lit:
			if dq {
				r.val += unescapeDouble(p.Value)
			} else {
				v, glob := unescapeLit(p.Value)
				r.val += v
				r.glob = r.glob || glob
			}
		case *syntax.SglQuoted:
			if p.Dollar {
				v, ok := ansiC(p.Value)
				if !ok {
					e.out.fail("unsupported ANSI-C quoted string")
				}
				r.val += v
			} else {
				r.val += p.Value
			}
		case *syntax.DblQuoted:
			if p.Dollar {
				e.out.fail("locale-translated string")
			}
			e.parts(p.Parts, true, r)
		case *syntax.ParamExp:
			if !simpleParam(p) {
				e.out.fail("parameter expansion with operators")
			}
			r.dyn = true
			r.val += printNode(p)
		default:
			// CmdSubst, ProcSubst, ArithmExp, ExtGlob, BraceExp: flagged
			// when visited. Keep the text so the argv is still informative.
			e.out.fail("expansion that cannot be resolved to a literal")
			r.dyn = true
			r.val += printNode(p)
		}
	}
}

func simpleParam(p *syntax.ParamExp) bool {
	return p.Param != nil && !p.Excl && !p.Length && !p.Width && !p.IsSet &&
		p.Flags == nil && p.Split == 0 && p.GlobSubst == 0 && p.RcExpand == 0 &&
		p.Index == nil && p.Slice == nil && p.Repl == nil && p.Exp == nil && p.Names == 0
}

func printNode(n syntax.Node) string {
	var b strings.Builder
	if err := syntax.NewPrinter().Print(&b, n); err != nil {
		return "$?"
	}
	return b.String()
}

// unescapeLit resolves backslash escapes in an unquoted literal and reports
// unescaped glob characters.
func unescapeLit(s string) (string, bool) {
	if !strings.ContainsAny(s, `\*?[`) {
		return s, false
	}
	var b strings.Builder
	glob := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			} else {
				b.WriteByte(c)
			}
		case '*', '?', '[':
			glob = true
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), glob
}

// unescapeDouble resolves the only escapes a double-quoted string honours.
func unescapeDouble(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0 {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ansiC decodes the body of $'...'. ok is false for results the shell would
// truncate (NUL) or that we do not model.
func ansiC(s string) (string, bool) {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b = append(b, c)
			continue
		}
		i++
		switch d := s[i]; d {
		case 'a':
			b = append(b, 7)
		case 'b':
			b = append(b, 8)
		case 'e', 'E':
			b = append(b, 27)
		case 'f':
			b = append(b, 12)
		case 'n':
			b = append(b, '\n')
		case 'r':
			b = append(b, '\r')
		case 't':
			b = append(b, '\t')
		case 'v':
			b = append(b, 11)
		case '\\', '\'', '"', '?':
			b = append(b, d)
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i
			for j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7' {
				j++
			}
			v, _ := strconv.ParseUint(s[i:j], 8, 16)
			b = append(b, byte(v))
			i = j - 1
		case 'x', 'u', 'U':
			max := map[byte]int{'x': 2, 'u': 4, 'U': 8}[d]
			j := i + 1
			for j < len(s) && j < i+1+max && isHex(s[j]) {
				j++
			}
			if j == i+1 {
				b = append(b, '\\', d)
				continue
			}
			v, _ := strconv.ParseUint(s[i+1:j], 16, 32)
			if d == 'x' {
				b = append(b, byte(v))
			} else {
				b = utf8.AppendRune(b, rune(v))
			}
			i = j - 1
		case 'c':
			if i+1 < len(s) {
				i++
				b = append(b, s[i]&0x1f)
			} else {
				return "", false
			}
		default:
			b = append(b, '\\', d)
		}
	}
	if slices.Contains(b, 0) {
		return "", false
	}
	return string(b), true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// ---- wrappers -----------------------------------------------------------

var shellNames = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true, "ash": true}

// unwrap adds the commands hidden behind a wrapper (env, nice, sudo, sh -c,
// eval, find -exec ...) as Inner commands so deny rules can see them.
func (e *extractor) unwrap(c Command) {
	argv := c.Argv
	for hops := 0; len(argv) > 0 && hops < 8; hops++ {
		name := path.Base(argv[0])
		var inner []string
		switch name {
		case "env":
			inner = skipEnv(argv[1:], e)
		case "nice":
			inner = skipOpts(argv[1:], "-n", "--adjustment")
		case "nohup", "builtin", "setsid", "chronic":
			inner = argv[1:]
		case "time":
			inner = skipOpts(argv[1:], "")
		case "stdbuf":
			inner = skipOpts(argv[1:], "")
		case "timeout":
			inner = skipOpts(argv[1:], "-s", "--signal", "-k", "--kill-after")
			if len(inner) > 0 { // the duration
				inner = inner[1:]
			}
		case "sudo":
			inner = skipOpts(argv[1:], "-u", "-g", "-C", "-D", "-h", "-p", "-r", "-t", "-T", "-U", "-R")
		case "doas":
			inner = skipOpts(argv[1:], "-u", "-C")
		case "command":
			if len(argv) > 1 && (argv[1] == "-v" || argv[1] == "-V") {
				return
			}
			inner = skipOpts(argv[1:], "")
		case "exec":
			e.out.fail("exec replaces the shell with another program")
			inner = skipOpts(argv[1:], "-a")
		case "xargs":
			e.out.fail("xargs builds its command from input")
			inner = skipOpts(argv[1:], "-I", "-i", "-n", "-P", "-s", "-L", "-d", "-E", "-a", "-l")
		case "eval":
			e.out.fail("eval runs constructed code")
			e.script(strings.Join(argv[1:], " "), c)
			return
		case "source", ".":
			e.out.fail("source runs a script file")
			return
		case "trap":
			e.out.fail("trap runs code later")
			if len(argv) > 1 {
				e.script(argv[1], c)
			}
			return
		case "find", "gfind":
			e.findExec(argv)
			return
		default:
			if shellNames[name] {
				e.shell(argv, c)
			}
			return
		}
		if len(inner) == 0 {
			return
		}
		e.innerCommand(inner, c)
		argv = inner
	}
	if len(argv) > 0 {
		e.out.fail("wrappers nested too deeply")
	}
}

// innerCommand records inner as a command. Dyn/Glob flags are recomputed by
// position from the outer command.
func (e *extractor) innerCommand(inner []string, outer Command) {
	off := len(outer.Argv) - len(inner)
	ic := Command{Argv: append([]string(nil), inner...), Inner: true}
	for i := range inner {
		ic.Dyn = append(ic.Dyn, outer.Dyn[off+i])
		ic.Glob = append(ic.Glob, outer.Glob[off+i])
	}
	if ic.Dyn[0] || ic.Glob[0] {
		e.out.fail("wrapped command name is not a literal")
	}
	e.out.Commands = append(e.out.Commands, ic)
}

// skipOpts drops leading -options (and the values of those named in withVal).
func skipOpts(args []string, withVal ...string) []string {
	for len(args) > 0 {
		a := args[0]
		if a == "--" {
			return args[1:]
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			return args
		}
		args = args[1:]
		for _, v := range withVal {
			if v != "" && a == v && len(args) > 0 {
				args = args[1:]
				break
			}
		}
	}
	return args
}

func skipEnv(args []string, e *extractor) []string {
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "--":
			return args[1:]
		case a == "-u" || a == "-C" || a == "--unset" || a == "--chdir":
			if len(args) < 2 {
				return nil
			}
			args = args[2:]
		case a == "-S" || strings.HasPrefix(a, "--split-string") || strings.HasPrefix(a, "-S"):
			e.out.fail("env -S splits a string into a command")
			return nil
		case strings.HasPrefix(a, "-") && a != "-":
			args = args[1:]
		case strings.Contains(a, "="):
			args = args[1:]
		default:
			return args
		}
	}
	return nil
}

func (e *extractor) shell(argv []string, outer Command) {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "--" || !strings.HasPrefix(a, "-") {
			break
		}
		if !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
			if i+1 < len(argv) {
				e.out.fail("nested shell (-c) runs a string as code")
				e.script(argv[i+1], outer)
				return
			}
		}
	}
	e.out.fail("nested shell runs other code")
}

// script parses text that a command will run as shell code and records its
// commands as Inner.
func (e *extractor) script(text string, _ Command) {
	p := parseDepth(text, e.depth+1)
	for _, c := range p.Commands {
		c.Inner = true
		e.out.Commands = append(e.out.Commands, c)
	}
	if p.Complex {
		e.out.fail("nested script: " + p.Reason())
	}
	e.out.SetsVars = e.out.SetsVars || p.SetsVars
}

// findExec extracts `find ... -exec CMD ... ;` payloads.
func (e *extractor) findExec(argv []string) {
	for i, a := range argv {
		switch a {
		case "-exec", "-execdir", "-ok", "-okdir":
			var inner []string
			for _, x := range argv[i+1:] {
				if x == ";" || x == "+" {
					break
				}
				inner = append(inner, x)
			}
			if len(inner) > 0 {
				e.out.Commands = append(e.out.Commands, Command{
					Argv: inner, Dyn: make([]bool, len(inner)), Glob: make([]bool, len(inner)), Inner: true,
				})
				e.unwrapInner(inner)
			}
		}
	}
}

func (e *extractor) unwrapInner(inner []string) {
	c := Command{Argv: inner, Dyn: make([]bool, len(inner)), Glob: make([]bool, len(inner))}
	e.unwrap(c)
}

// Text is the command as a single line, for matching against rule patterns.
func (c Command) Text() string { return strings.Join(c.Argv, " ") }

// Bin is the program name with any directory removed.
func (c Command) Bin() string {
	if len(c.Argv) == 0 {
		return ""
	}
	return path.Base(c.Argv[0])
}

// String renders the commands one per line, for diagnostics.
func (p Parsed) String() string {
	var b strings.Builder
	for _, c := range p.Commands {
		fmt.Fprintf(&b, "%q\n", c.Argv)
	}
	return b.String()
}
