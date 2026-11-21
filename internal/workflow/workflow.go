// Package workflow loads WORKFLOW.md: optional YAML front matter followed by
// a Markdown prompt body (SPEC section 5).
package workflow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Error classes from SPEC section 5.5.
const (
	ClassMissingFile       = "missing_workflow_file"
	ClassParseError        = "workflow_parse_error"
	ClassFrontMatterNotMap = "workflow_front_matter_not_a_map"
	ClassPathUntrusted     = "workflow_path_untrusted"
	ClassInvalidConfig     = "workflow_invalid_config"
)

// Error is a typed workflow error.
type Error struct {
	Class string
	// Key is the offending configuration key, when one applies.
	Key string
	Msg string
}

func (e *Error) Error() string {
	if e.Key != "" {
		return fmt.Sprintf("%s: %s: %s", e.Class, e.Key, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Class, e.Msg)
}

// ClassOf returns the error class of err, or "" if err is not a workflow error.
func ClassOf(err error) string {
	var we *Error
	if errors.As(err, &we) {
		return we.Class
	}
	return ""
}

// Definition is the parsed workflow file.
type Definition struct {
	// Path is the path the definition was loaded from.
	Path string
	// Config is the front matter root object. Empty when there is no front matter.
	Config map[string]any
	// PromptTemplate is the trimmed Markdown body.
	PromptTemplate string
}

// DefaultFileName is used when no explicit path is given.
const DefaultFileName = "WORKFLOW.md"

// ResolvePath applies the path precedence of SPEC section 5.1.
func ResolvePath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return DefaultFileName
}

// Load reads and parses the workflow file at path.
func Load(path string) (*Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Class: ClassMissingFile, Msg: err.Error()}
	}
	def, err := Parse(data)
	if err != nil {
		return nil, err
	}
	def.Path = path
	return def, nil
}

// Parse parses workflow file content.
func Parse(data []byte) (*Definition, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	front, body, hasFront, err := split(text)
	if err != nil {
		return nil, err
	}
	def := &Definition{Config: map[string]any{}, PromptTemplate: strings.TrimSpace(body)}
	if !hasFront {
		return def, nil
	}

	var root yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader([]byte(front)))
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return def, nil // empty front matter is an empty map
		}
		return nil, &Error{Class: ClassParseError, Msg: err.Error()}
	}
	if len(root.Content) == 0 {
		return def, nil
	}
	doc := root.Content[0]
	if err := rejectUnsafe(doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.MappingNode {
		return nil, &Error{Class: ClassFrontMatterNotMap, Msg: "front matter must be a YAML mapping"}
	}
	var cfg map[string]any
	if err := doc.Decode(&cfg); err != nil {
		return nil, &Error{Class: ClassParseError, Msg: err.Error()}
	}
	if cfg != nil {
		def.Config = cfg
	}
	return def, nil
}

// split separates front matter from the body. Front matter exists only when
// the first line is exactly "---".
func split(text string) (front, body string, hasFront bool, err error) {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return "", text, false, nil
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), true, nil
		}
	}
	return "", "", false, &Error{Class: ClassParseError, Msg: "front matter is not closed by a '---' line"}
}

// standardTags are the only YAML tags accepted. Anything else is a custom tag.
var standardTags = map[string]bool{
	"": true, "!!map": true, "!!seq": true, "!!str": true, "!!int": true,
	"!!float": true, "!!bool": true, "!!null": true,
}

// rejectUnsafe refuses anchors, aliases, merge keys and custom tags
// (SPEC section 5.2).
func rejectUnsafe(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode {
		return &Error{Class: ClassParseError, Msg: fmt.Sprintf("line %d: YAML aliases are not allowed", n.Line)}
	}
	if n.Anchor != "" {
		return &Error{Class: ClassParseError, Msg: fmt.Sprintf("line %d: YAML anchors are not allowed", n.Line)}
	}
	if !standardTags[n.Tag] {
		return &Error{Class: ClassParseError, Msg: fmt.Sprintf("line %d: YAML tag %q is not allowed", n.Line, n.Tag)}
	}
	for _, c := range n.Content {
		if err := rejectUnsafe(c); err != nil {
			return err
		}
	}
	return nil
}
