package server

import (
	"fmt"
	"strings"

	"github.com/nektos/act/pkg/exprparser"
	"github.com/nektos/act/pkg/model"
	"gopkg.in/yaml.v3"
)

// evaluator evaluates the workflow expressions the server needs before a job
// runs: job `if`, `strategy.matrix`, `runs-on` and `name`. It has the github,
// needs, matrix and (empty) vars and inputs contexts; the worker evaluates
// everything else when the job runs.
type evaluator struct {
	github *model.GithubContext
	wf     *model.Workflow // jobs that finished carry their Result, as act expects for success() and failure()
	key    string
	needs  map[string]exprparser.Needs
	matrix map[string]any
}

func (e *evaluator) interpreter() exprparser.Interpreter {
	matrix := e.matrix
	if matrix == nil {
		matrix = map[string]any{}
	}
	jobs := map[string]*model.WorkflowCallResult{}
	return exprparser.NewInterpeter(&exprparser.EvaluationEnvironment{
		Github:   e.github,
		Env:      map[string]string{},
		Job:      &model.JobContext{Status: "success"},
		Jobs:     &jobs,
		Steps:    map[string]*model.StepResult{},
		Runner:   map[string]any{},
		Secrets:  map[string]string{},
		Vars:     map[string]string{},
		Strategy: map[string]any{},
		Matrix:   matrix,
		Needs:    e.needs,
		Inputs:   map[string]any{},
	}, exprparser.Config{Run: &model.Run{Workflow: e.wf, JobID: e.key}, Context: "job"})
}

// enabled evaluates a job's `if`, which defaults to success().
func (e *evaluator) enabled(cond string) (bool, error) {
	expr, err := toExpression(cond, false)
	if err != nil {
		return false, err
	}
	v, err := e.interpreter().Evaluate(expr, exprparser.DefaultStatusCheckSuccess)
	if err != nil {
		return false, err
	}
	return exprparser.IsTruthy(v), nil
}

// value evaluates a string that may hold ${{ }} expressions. A string that is
// one expression keeps the expression's type (fromJSON can return a list).
func (e *evaluator) value(in string) (any, error) {
	if !strings.Contains(in, "${{") {
		return in, nil
	}
	expr, err := toExpression(in, false)
	if err != nil {
		return nil, err
	}
	return e.interpreter().Evaluate(expr, exprparser.DefaultStatusCheckNone)
}

// interpolate evaluates in to a string.
func (e *evaluator) interpolate(in string) (string, error) {
	if !strings.Contains(in, "${{") {
		return in, nil
	}
	expr, err := toExpression(in, true)
	if err != nil {
		return "", err
	}
	v, err := e.interpreter().Evaluate(expr, exprparser.DefaultStatusCheckNone)
	if err != nil {
		return "", err
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return fmt.Sprint(v), nil
}

// node evaluates every expression in a YAML value in place, as act does for
// strategy.matrix.
func (e *evaluator) node(n *yaml.Node) error {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if err := e.node(c); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			if err := e.node(n.Content[i]); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if n.Tag != "!!str" && n.Tag != "" || !strings.Contains(n.Value, "${{") {
			return nil
		}
		v, err := e.value(n.Value)
		if err != nil {
			return err
		}
		var out yaml.Node
		if err := out.Encode(v); err != nil {
			return err
		}
		*n = out
	}
	return nil
}

// toExpression turns a string with ${{ }} parts into one expression: a lone
// ${{ x }} becomes x, anything else a format() call. A string without ${{ is
// already an expression (as in `if: github.ref == 'refs/heads/main'`), unless
// asString is set.
func toExpression(in string, asString bool) (string, error) {
	var format strings.Builder
	var args []string
	rest := in
	for {
		start := strings.Index(rest, "${{")
		if start < 0 {
			format.WriteString(escapeFormat(rest))
			break
		}
		format.WriteString(escapeFormat(rest[:start]))
		rest = rest[start+3:]
		end := exprEnd(rest)
		if end < 0 {
			return "", fmt.Errorf("unclosed expression in %q", in)
		}
		fmt.Fprintf(&format, "{%d}", len(args))
		args = append(args, strings.TrimSpace(rest[:end]))
		rest = rest[end+2:]
	}
	switch {
	case len(args) == 0 && !asString:
		return in, nil
	case len(args) == 1 && format.String() == "{0}" && !asString:
		return args[0], nil
	}
	if len(args) == 0 {
		return "'" + strings.ReplaceAll(in, "'", "''") + "'", nil
	}
	return fmt.Sprintf("format('%s', %s)", strings.ReplaceAll(format.String(), "'", "''"), strings.Join(args, ", ")), nil
}

// exprEnd finds the }} that closes an expression, skipping quoted strings.
func exprEnd(s string) int {
	inString := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			if inString && i+1 < len(s) && s[i+1] == '\'' {
				i++ // an escaped quote
				continue
			}
			inString = !inString
		case !inString && strings.HasPrefix(s[i:], "}}"):
			return i
		}
	}
	return -1
}

// escapeFormat escapes literal text for a format() string.
func escapeFormat(s string) string {
	return strings.NewReplacer("{", "{{", "}", "}}").Replace(s)
}
