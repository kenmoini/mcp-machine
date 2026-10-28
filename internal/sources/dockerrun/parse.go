// Package dockerrun parses "docker run" / "podman run" command lines into a
// container spec.
package dockerrun

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/shell"
	"mvdan.cc/sh/v3/syntax"
)

// Spec is the supported subset of a docker/podman run command line.
type Spec struct {
	Image       string
	Args        []string
	Env         map[string]string
	PassEnv     []string
	Volumes     []string
	Ports       []string
	Name        string
	Network     string
	WorkDir     string
	Interactive bool
	Remove      bool
}

// placeholderEnv re-emits every variable reference as a literal ${NAME} so
// config placeholders survive parsing and the process environment never leaks
// in. IFS is the exception: word splitting consults it, and a placeholder
// there would split on its own characters, so it gets the shell default.
func placeholderEnv(name string) string {
	if name == "IFS" {
		return " \t\n"
	}
	return "${" + name + "}"
}

var plainName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkSyntax rejects shell syntax that would resolve or alter the input
// rather than pass it through: only plain $NAME / ${NAME} references (which
// placeholderEnv keeps verbatim), quoting and literal text are allowed. This
// also keeps constructs the expander cannot handle away from it.
func checkSyntax(line string) error {
	for w, err := range syntax.NewParser().WordsSeq(strings.NewReader(line)) {
		if err != nil {
			return fmt.Errorf("parse command line: %w", err)
		}
		src := wordSource(line, w)
		if lit, ok := firstPart(w).(*syntax.Lit); ok && strings.HasPrefix(lit.Value, "~") {
			return unsupported("tilde expansion", src)
		}
		// Brace expansion is only visible after splitting; this mutates the
		// local copy of the word, which is discarded afterwards (and must not
		// be printed, so src was taken above).
		syntax.SplitBraces(w)
		var what string
		syntax.Walk(w, func(n syntax.Node) bool {
			if what != "" {
				return false
			}
			switch n := n.(type) {
			case *syntax.ParamExp:
				if !isPlainParam(n) {
					what = "parameter expansion other than $NAME or ${NAME}"
				}
				return false
			case *syntax.BraceExp:
				what = "brace expansion"
			case *syntax.CmdSubst:
				what = "command substitution"
			case *syntax.ArithmExp:
				what = "arithmetic expansion"
			case *syntax.ProcSubst:
				what = "process substitution"
			case *syntax.ExtGlob:
				what = "extended glob"
			}
			return what == ""
		})
		if what != "" {
			return unsupported(what, src)
		}
	}
	return nil
}

func firstPart(w *syntax.Word) syntax.WordPart {
	if len(w.Parts) == 0 {
		return nil
	}
	return w.Parts[0]
}

// isPlainParam reports whether p is exactly $NAME or ${NAME}.
func isPlainParam(p *syntax.ParamExp) bool {
	return p.Param != nil && plainName.MatchString(p.Param.Value) &&
		p.Flags == nil && !p.Excl && !p.Length && !p.Width && !p.IsSet &&
		p.Split == syntax.OptUnset && p.GlobSubst == syntax.OptUnset && p.RcExpand == syntax.OptUnset &&
		p.NestedParam == nil && p.Index == nil && len(p.Modifiers) == 0 &&
		p.Slice == nil && p.Repl == nil && p.Names == 0 && p.Exp == nil
}

// wordSource returns the text of w as written in line.
func wordSource(line string, w *syntax.Word) string {
	start, end := int(w.Pos().Offset()), int(w.End().Offset())
	if start < 0 || end > len(line) || start > end {
		return ""
	}
	return line[start:end]
}

func unsupported(what, src string) error {
	return fmt.Errorf("unsupported shell syntax: %s in %q", what, src)
}

// attachedShort lists the short value flags that accept an attached value.
var attachedShort = map[string]bool{"-e": true, "-v": true, "-p": true, "-w": true}

// Parse parses a "docker run ..." or "podman run ..." command line.
func Parse(line string) (spec Spec, err error) {
	// The shell library has panicked on exotic input before; whatever the
	// input, the contract is a Spec or an error.
	defer func() {
		if r := recover(); r != nil {
			spec, err = Spec{}, fmt.Errorf("parse command line: %v", r)
		}
	}()
	if err := checkSyntax(line); err != nil {
		return Spec{}, err
	}
	toks, err := shell.Fields(line, placeholderEnv)
	if err != nil {
		return Spec{}, fmt.Errorf("parse command line: %w", err)
	}
	if len(toks) == 0 || (toks[0] != "docker" && toks[0] != "podman") {
		return Spec{}, errors.New("expected docker or podman")
	}
	if len(toks) < 2 || toks[1] != "run" {
		return Spec{}, errors.New("expected run")
	}
	toks = toks[2:]

	var s Spec
	i := 0
	for i < len(toks) {
		tok := toks[i]
		if tok == "--" {
			i++
			break
		}
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			break
		}
		i++

		flag, val, hasVal := tok, "", false
		switch {
		case strings.HasPrefix(tok, "--"):
			if k, v, ok := strings.Cut(tok, "="); ok {
				flag, val, hasVal = k, v, true
			}
		case len(tok) > 2 && attachedShort[tok[:2]]:
			// Attached short-flag value such as -eK=V or -p80:80.
			flag, val, hasVal = tok[:2], tok[2:], true
		}

		switch flag {
		case "-i", "--interactive":
			s.Interactive = true
		case "--rm":
			s.Remove = true
		case "-it", "-ti", "-t", "--tty":
			return Spec{}, errors.New("tty is not supported")
		case "--env-file":
			return Spec{}, errors.New("unsupported flag --env-file")
		case "-e", "--env", "-v", "--volume", "-p", "--publish", "--name", "--network", "-w", "--workdir":
			if !hasVal {
				if i >= len(toks) {
					return Spec{}, fmt.Errorf("flag %s requires a value", flag)
				}
				val = toks[i]
				i++
			}
			if hasVal && val == "" {
				return Spec{}, fmt.Errorf("flag %s requires a value", flag)
			}
			if err := s.setValue(flag, val); err != nil {
				return Spec{}, err
			}
		default:
			return Spec{}, fmt.Errorf("unknown flag %s", flag)
		}
	}

	if i >= len(toks) {
		return Spec{}, errors.New("missing image")
	}
	s.Image = toks[i]
	if rest := toks[i+1:]; len(rest) > 0 {
		s.Args = append([]string(nil), rest...)
	}
	return s, nil
}

// setValue applies a value-taking flag.
func (s *Spec) setValue(flag, val string) error {
	switch flag {
	case "-e", "--env":
		k, v, ok := strings.Cut(val, "=")
		if k == "" {
			return fmt.Errorf("invalid env %q", val)
		}
		if !ok {
			s.PassEnv = append(s.PassEnv, k)
			return nil
		}
		if s.Env == nil {
			s.Env = map[string]string{}
		}
		s.Env[k] = v
	case "-v", "--volume":
		s.Volumes = append(s.Volumes, val)
	case "-p", "--publish":
		s.Ports = append(s.Ports, val)
	case "--name":
		s.Name = val
	case "--network":
		s.Network = val
	case "-w", "--workdir":
		s.WorkDir = val
	}
	return nil
}
