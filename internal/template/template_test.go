package template

import (
	"strings"
	"testing"
)

func vars() map[string]any {
	return map[string]any{
		"issue": map[string]any{
			"identifier": "GH-7", "title": "Fix login", "description": nil,
			"priority": int64(2), "labels": []any{"bug", "run:linux"},
		},
		"attempt":    nil,
		"retry_kind": nil,
		"feedback": []any{
			map[string]any{"source": "ci", "body": "lint failed"},
			map[string]any{"source": "review", "body": "rename it"},
		},
		"empty_list": []any{},
		"n":          3,
	}
}

func render(t *testing.T, src string) string {
	t.Helper()
	tmpl, err := Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	out, err := tmpl.Render(vars())
	if err != nil {
		t.Fatalf("render %q: %v", src, err)
	}
	return out
}

func TestRender(t *testing.T) {
	for src, want := range map[string]string{
		"plain text { not a tag }":                                                                "plain text { not a tag }",
		"{{ issue.identifier }}: {{ issue.title | upcase }}":                                      "GH-7: FIX LOGIN",
		"[{{ issue.description }}]":                                                               "[]",
		"{{ issue.description | default: 'none' }}":                                               "none",
		"{{ issue.labels | join: ', ' }}":                                                         "bug, run:linux",
		"{{ issue.labels.size }} {{ issue.labels[1] }}":                                           "2 run:linux",
		"{{ issue.labels.first }} {{ issue.labels | last }}":                                      "bug run:linux",
		"{{ issue.title | truncate: 6 }}":                                                         "Fix...",
		"{{ issue.title | replace: 'login', 'logout' | downcase }}":                               "fix logout",
		"{{ n | plus: 1 }} {{ n | minus: 5 }}":                                                    "4 -2",
		"{{ 'a' | append: 'b' | prepend: 'c' | capitalize }}":                                     "Cab",
		"{% if attempt %}retry{% else %}first{% endif %}":                                         "first",
		"{% unless attempt %}first{% endunless %}":                                                "first",
		"{% if issue.priority <= 2 and issue.labels contains 'bug' %}urgent bug{% endif %}":       "urgent bug",
		"{% if issue.priority == 1 %}p1{% elsif issue.priority == 2 %}p2{% else %}low{% endif %}": "p2",
		"{% if issue.description == blank %}no description{% endif %}":                            "no description",
		"{% if issue.title != empty %}titled{% endif %}":                                          "titled",
		"{% if retry_kind == nil %}fresh{% endif %}":                                              "fresh",
		"{% if issue.title contains 'login' %}yes{% endif %}":                                     "yes",
		"{% for f in feedback %}{{ forloop.index }}/{{ forloop.length }} {{ f.source }}: {{ f.body }}{% unless forloop.last %}; {% endunless %}{% endfor %}": "1/2 ci: lint failed; 2/2 review: rename it",
		"{% for x in empty_list %}x{% else %}nothing{% endfor %}":                "nothing",
		"{% for l in issue.labels %}{% if l == 'bug' %}B{% endif %}{% endfor %}": "B",
		"a {% comment %} {{ nope }} {% bogus %} {% endcomment %}b":               "a b",
		"{% raw %}{{ issue.title }} {% if %}{% endraw %}":                        "{{ issue.title }} {% if %}",
		"a  {{- issue.identifier -}}  b":                                         "aGH-7b",
		"a\n  {%- if true -%}\n  x\n  {%- endif -%}\n b":                         "axb",
		// Right to left, no precedence: false and (false or true) is false.
		"{% if false and false or true %}t{% else %}f{% endif %}": "f",
		"{% if true or false and false %}t{% else %}f{% endif %}": "t",
	} {
		if got := render(t, src); got != want {
			t.Errorf("%q\n got %q\nwant %q", src, got, want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for src, want := range map[string]string{
		"{{ issue.title | shell }}":                   `unknown filter "shell"`,
		"{{ issue.title | upcase: 1 }}":               "arguments",
		"{{ issue.title | truncate }}":                "arguments",
		"{% include 'secrets' %}":                     `unsupported tag "include"`,
		"{% render 'x' %}":                            `unsupported tag "render"`,
		"{% assign x = 1 %}":                          `unsupported tag "assign"`,
		"{% capture x %}{% endcapture %}":             `unsupported tag "capture"`,
		"{% if issue.title %}":                        "if without endif",
		"{% endif %}":                                 "unexpected tag",
		"{% for f in feedback %}":                     "for without endfor",
		"{% for f in feedback limit:1 %}{% endfor %}": "not supported",
		"{{ issue.title":                              "unclosed",
		"{{ issue.title issue.title }}":               "unexpected",
		"{{ 'open }}":                                 "unclosed string",
		"{% if a == %}{% endif %}":                    "value expected",
		"{% raw %}never closed":                       "raw without endraw",
		"line one\n{{ x | nope }}":                    "line 2",
	} {
		_, err := Parse(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want it to contain %q", src, err, want)
		}
	}
}

func TestRenderErrors(t *testing.T) {
	for src, want := range map[string]string{
		"{{ nope }}":                                      `unknown variable "nope"`,
		"{{ issue.titel }}":                               `unknown variable "issue.titel"`,
		"{{ issue.title.length }}":                        `unknown variable "issue.title.length"`,
		"{{ issue.description.size }}":                    "unknown variable",
		"{% if nope %}x{% endif %}":                       "unknown variable",
		"{% for x in nope %}{% endfor %}":                 "unknown variable",
		"{% for x in issue.title %}{% endfor %}":          "for needs a list",
		"{{ issue }}":                                     "cannot be printed",
		"{{ issue.labels }}":                              "cannot be printed",
		"{{ issue.title | plus: 1 }}":                     "whole numbers",
		"{% if issue.title > 1 %}x{% endif %}":            "numbers only",
		"{% for f in feedback %}{% endfor %}{{ f.body }}": `unknown variable "f"`,
	} {
		tmpl, err := Parse(src)
		if err != nil {
			t.Errorf("%q: parse error %v", src, err)
			continue
		}
		_, err = tmpl.Render(vars())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want it to contain %q", src, err, want)
		}
	}
}

func TestLimits(t *testing.T) {
	deep := strings.Repeat("{% if true %}", 100) + strings.Repeat("{% endif %}", 100)
	if _, err := Parse(deep); err == nil || !strings.Contains(err.Error(), "nested too deeply") {
		t.Errorf("deep nesting: %v", err)
	}
	big := make([]any, 1000)
	for i := range big {
		big[i] = strings.Repeat("x", 2000)
	}
	tmpl, err := Parse("{% for a in big %}{{ a }}{% endfor %}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmpl.Render(map[string]any{"big": big}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("output limit: %v", err)
	}
}

// A value is inserted as text. Template syntax inside it is never run.
func TestValuesAreNotReparsed(t *testing.T) {
	tmpl, err := Parse("{{ t }}")
	if err != nil {
		t.Fatal(err)
	}
	evil := "{% include 'x' %}{{ secret }}"
	out, err := tmpl.Render(map[string]any{"t": evil, "secret": "s3cret"})
	if err != nil || out != evil {
		t.Errorf("out %q err %v", out, err)
	}
}
