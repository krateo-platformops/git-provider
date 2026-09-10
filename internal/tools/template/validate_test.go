package template

import (
	"strings"
	"testing"
)

// The point of Validate: Render reports ONE problem per file; this reports all of them.
func TestValidateReportsEveryUndefinedFunction(t *testing.T) {
	body := "a: {% toYaml .x %}\nb: {% include \"y\" . %}\nc: {% tpl .z %}\n"

	if _, err := Template(body).Render(map[string]any{"x": 1, "z": 2}); err == nil {
		t.Fatal("setup: expected Render to fail")
	} else if strings.Count(err.Error(), "not defined") != 1 {
		t.Fatalf("setup: Render should report exactly one, got %v", err)
	}

	got := Validate(body, "", "", map[string]any{"x": 1, "z": 2})
	if len(got) != 3 {
		t.Fatalf("expected 3 problems, got %d: %v", len(got), got)
	}
	for _, want := range []string{"toYaml", "include", "tpl"} {
		found := false
		for _, p := range got {
			if strings.Contains(p.Message, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing a report for %q: %v", want, got)
		}
	}
	for _, p := range got {
		if p.Location == "" {
			t.Errorf("problem without a location: %v", p)
		}
	}
}

// The silent half: a reference to a value nobody declared is NOT an error to Render — it renders
// "<no value>" and gets committed. Validate catches it.
func TestValidateCatchesUndeclaredValues(t *testing.T) {
	body := "tenant: {% .tenant %}\nenv: {% .environment %}\ncluster: {% .cluster %}\n"
	values := map[string]any{"tenant": "kiratech"}

	out, err := Template(body).Render(values)
	if err != nil {
		t.Fatalf("setup: Render should NOT fail here, got %v", err)
	}
	if !strings.Contains(string(out), "<no value>") {
		t.Fatalf("setup: expected silent <no value>, got %q", string(out))
	}

	got := Validate(body, "", "", values)
	if len(got) != 2 {
		t.Fatalf("expected 2 undeclared values, got %d: %v", len(got), got)
	}
	for _, want := range []string{".environment", ".cluster"} {
		found := false
		for _, p := range got {
			if strings.Contains(p.Message, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing a report for %q: %v", want, got)
		}
	}
}

func TestValidateMixesBothClasses(t *testing.T) {
	body := "a: {% toYaml .x %}\nb: {% .undeclared %}\n"
	got := Validate(body, "", "", map[string]any{"x": 1})
	if len(got) != 2 {
		t.Fatalf("expected 2 problems, got %d: %v", len(got), got)
	}
}

func TestValidateCleanTemplateHasNoProblems(t *testing.T) {
	body := "project: {% .name | upper %}\nstamp: {% now | date \"2006\" %}\n"
	if got := Validate(body, "", "", map[string]any{"name": "sock-shop"}); len(got) != 0 {
		t.Errorf("expected no problems, got %v", got)
	}
}

// Helm syntax must not be reported under the default delimiters — it is not ours to validate.
func TestValidateIgnoresHelmSyntaxUnderDefaultDelims(t *testing.T) {
	body := "name: {{ include \"x\" . }}\nreplicas: {{ .Values.replicas }}\ntenant: {% .tenant %}\n"
	if got := Validate(body, "", "", map[string]any{"tenant": "kiratech"}); len(got) != 0 {
		t.Errorf("Helm syntax should be invisible under {%% %%}, got %v", got)
	}
}

// A syntax error genuinely stops the parser, so one report is all there is — say so honestly
// rather than pretending to be exhaustive.
func TestValidateReturnsSingleProblemForSyntaxError(t *testing.T) {
	got := Validate("a: {% .x\n", "", "", nil)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 problem for a syntax error, got %v", got)
	}
}

// Builtins and pipeline arguments must not produce false reports.
func TestValidateNoFalsePositives(t *testing.T) {
	body := "a: {% printf \"%s\" .name %}\nb: {% if not .flag %}x{% end %}\nc: {% len .items %}\n"
	values := map[string]any{"name": "x", "flag": true, "items": []int{1}}
	if got := Validate(body, "", "", values); len(got) != 0 {
		t.Errorf("expected no problems, got %v", got)
	}
}
