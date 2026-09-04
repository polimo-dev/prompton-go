package liquid

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type templateSuite struct {
	Cases []struct {
		Name      string                 `json:"name"`
		Template  string                 `json:"template"`
		Variables map[string]interface{} `json:"variables"`
		Engine    string                 `json:"engine"`
		Normative *bool                  `json:"normative"`
		Note      string                 `json:"note"`
		Expect    struct {
			Output   *string `json:"output"`
			Error    string  `json:"error"`
			Variable string  `json:"variable"`
		} `json:"expect"`
	} `json:"cases"`
	LintCases []struct {
		Name     string `json:"name"`
		Template string `json:"template"`
		Expect   struct {
			Lint    string       `json:"lint"`
			Reasons []LintReason `json:"reasons"`
		} `json:"expect"`
	} `json:"lint_cases"`
	VariablesCases []struct {
		Name     string `json:"name"`
		Template string `json:"template"`
		Expect   struct {
			Variables []string `json:"variables"`
		} `json:"expect"`
	} `json:"variables_cases"`
}

func loadTemplateSuite(t *testing.T) templateSuite {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "conformance", "template.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var suite templateSuite
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&suite); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return suite
}

func TestConformanceTemplateRender(t *testing.T) {
	suite := loadTemplateSuite(t)
	if len(suite.Cases) == 0 {
		t.Fatal("no render cases loaded")
	}
	normative := 0
	for _, c := range suite.Cases {
		c := c
		if c.Normative != nil && !*c.Normative {
			continue
		}
		normative++
		t.Run(c.Name, func(t *testing.T) {
			out, err := Render(c.Template, c.Variables, Engine(c.Engine))
			if c.Expect.Output != nil {
				if err != nil {
					t.Fatalf("unexpected error %v (want %q)", err, *c.Expect.Output)
				}
				if out != *c.Expect.Output {
					t.Fatalf("output mismatch\n got: %q\nwant: %q", out, *c.Expect.Output)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %s, got output %q", c.Expect.Error, out)
			}
			if err.Category != c.Expect.Error {
				t.Fatalf("category %q, want %q (%v)", err.Category, c.Expect.Error, err)
			}
			if c.Expect.Variable != "" && err.Variable != c.Expect.Variable {
				t.Fatalf("variable %q, want %q", err.Variable, c.Expect.Variable)
			}
		})
	}
	if normative < 60 {
		t.Fatalf("only %d normative render cases ran", normative)
	}
}

func TestConformanceTemplateLint(t *testing.T) {
	suite := loadTemplateSuite(t)
	if len(suite.LintCases) == 0 {
		t.Fatal("no lint cases loaded")
	}
	for _, c := range suite.LintCases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			got := Lint(c.Template)
			if c.Expect.Lint == "ok" {
				if len(got) != 0 {
					t.Fatalf("expected clean lint, got %+v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, c.Expect.Reasons) {
				t.Fatalf("lint mismatch\n got: %+v\nwant: %+v", got, c.Expect.Reasons)
			}
		})
	}
}

func TestConformanceTemplateVariables(t *testing.T) {
	suite := loadTemplateSuite(t)
	if len(suite.VariablesCases) == 0 {
		t.Fatal("no variables cases loaded")
	}
	for _, c := range suite.VariablesCases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			got := Variables(c.Template)
			if !reflect.DeepEqual(got, c.Expect.Variables) {
				t.Fatalf("variables mismatch\n got: %v\nwant: %v", got, c.Expect.Variables)
			}
		})
	}
}
