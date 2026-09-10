package template

import (
	"fmt"
	"sort"
	"text/template/parse"

	"github.com/Masterminds/sprig/v3"
)

// Problem is one thing wrong with a template, located in the source.
type Problem struct {
	// Location is "line:column", as text/template reports positions.
	Location string
	// Message describes the problem in the renderer's own vocabulary.
	Message string
}

func (p Problem) String() string {
	if p.Location == "" {
		return p.Message
	}
	return fmt.Sprintf("%s: %s", p.Location, p.Message)
}

// Located reports whether Validate could place this problem in the source. It is false only when
// the parser itself could not continue, in which case the single problem IS the parser's error
// and the caller is better off keeping the renderer's own message.
func (p Problem) Located() bool { return p.Location != "" }

// builtins are the functions text/template defines itself; they are not in sprig's map but are
// always callable, so treating them as undefined would produce false reports.
var builtins = map[string]bool{
	"and": true, "call": true, "html": true, "index": true, "slice": true, "js": true,
	"len": true, "not": true, "or": true, "print": true, "printf": true, "println": true,
	"urlquery": true, "eq": true, "ge": true, "gt": true, "le": true, "lt": true, "ne": true,
	"break": true, "continue": true,
}

// Validate reports EVERY problem it can find in one pass, which Render cannot do.
//
// text/template stops at the first error — a file with three undefined functions reports one, and
// the operator then fixes, re-syncs, and meets the next. Worse, a reference to a value that was
// never declared is not an error at all: it renders as the literal "<no value>" and is committed.
//
// Validate side-steps both by parsing with parse.SkipFuncCheck, which yields a tree even when the
// functions do not exist, then walking it:
//
//   - every function identifier absent from sprig and from text/template's builtins
//   - every top-level field reference absent from the supplied values
//
// It is a REPORTING aid, not the gate: Render remains the source of truth for whether a file
// renders. A syntax error still stops the parser, so for that class Validate returns the single
// error the parser gives, exactly as before.
func Validate(body, left, right string, values map[string]any) []Problem {
	if left == "" || right == "" {
		left, right = DefaultLeftDelim, DefaultRightDelim
	}

	tree := parse.New("template")
	tree.Mode = parse.SkipFuncCheck

	parsed, err := tree.Parse(body, left, right, map[string]*parse.Tree{})
	if err != nil {
		// The parser cannot continue past a syntax error, so this really is all there is.
		return []Problem{{Location: "", Message: err.Error()}}
	}

	funcs := sprig.FuncMap()
	seen := map[string]bool{}
	var out []Problem

	add := func(node parse.Node, msg string) {
		loc := ""
		if ctx, _ := parsed.ErrorContext(node); ctx != "" {
			loc = ctx
		}
		key := loc + "\x00" + msg
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Problem{Location: loc, Message: msg})
	}

	var walk func(parse.Node)
	walkPipe := func(p *parse.PipeNode) {
		if p != nil {
			walk(p)
		}
	}

	walk = func(n parse.Node) {
		switch t := n.(type) {
		case *parse.ListNode:
			if t == nil {
				return
			}
			for _, c := range t.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			walkPipe(t.Pipe)
		case *parse.IfNode:
			walkPipe(t.Pipe)
			walk(t.List)
			walk(t.ElseList)
		case *parse.RangeNode:
			walkPipe(t.Pipe)
			walk(t.List)
			walk(t.ElseList)
		case *parse.WithNode:
			walkPipe(t.Pipe)
			walk(t.List)
			walk(t.ElseList)
		case *parse.TemplateNode:
			walkPipe(t.Pipe)
		case *parse.PipeNode:
			for _, c := range t.Cmds {
				walk(c)
			}
		case *parse.CommandNode:
			for i, a := range t.Args {
				switch arg := a.(type) {
				case *parse.IdentifierNode:
					// Only the FIRST argument of a command is the function being called;
					// later identifiers are arguments to it.
					if i == 0 && !builtins[arg.Ident] {
						if _, ok := funcs[arg.Ident]; !ok {
							add(arg, fmt.Sprintf("function %q not defined", arg.Ident))
						}
					}
				case *parse.FieldNode:
					if len(arg.Ident) > 0 && values != nil {
						if _, ok := values[arg.Ident[0]]; !ok {
							add(arg, fmt.Sprintf("no value declared for %q", "."+arg.Ident[0]))
						}
					}
				case *parse.PipeNode:
					walk(arg)
				}
			}
		}
	}

	walk(parsed.Root)

	sort.SliceStable(out, func(i, j int) bool { return out[i].Location < out[j].Location })
	return out
}
