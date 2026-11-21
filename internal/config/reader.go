package config

import (
	"fmt"
	"sort"

	"github.com/iandeherdt/Symphony-beads/internal/workflow"
)

// section reads typed values from one YAML mapping and remembers which keys
// were consumed, so that unknown keys can be reported.
type section struct {
	path string
	m    map[string]any
	seen map[string]bool
	errs *[]error
}

func newSection(path string, v any, errs *[]error) *section {
	s := &section{path: path, m: map[string]any{}, seen: map[string]bool{}, errs: errs}
	switch t := v.(type) {
	case nil:
	case map[string]any:
		s.m = t
	default:
		s.fail("", "must be a mapping")
	}
	return s
}

func (s *section) key(k string) string {
	if s.path == "" {
		return k
	}
	if k == "" {
		return s.path
	}
	return s.path + "." + k
}

func (s *section) fail(k, format string, args ...any) {
	*s.errs = append(*s.errs, &workflow.Error{
		Class: workflow.ClassInvalidConfig, Key: s.key(k), Msg: fmt.Sprintf(format, args...),
	})
}

func (s *section) raw(k string) (any, bool) {
	s.seen[k] = true
	v, ok := s.m[k]
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}

func (s *section) sub(k string) *section {
	v, _ := s.raw(k)
	return newSection(s.key(k), v, s.errs)
}

func (s *section) str(k, def string) string {
	v, ok := s.raw(k)
	if !ok {
		return def
	}
	t, ok := v.(string)
	if !ok {
		s.fail(k, "must be a string")
		return def
	}
	return t
}

func (s *section) boolean(k string, def bool) bool {
	v, ok := s.raw(k)
	if !ok {
		return def
	}
	t, ok := v.(bool)
	if !ok {
		s.fail(k, "must be true or false")
		return def
	}
	return t
}

func (s *section) integer(k string, def int64) int64 {
	v, ok := s.raw(k)
	if !ok {
		return def
	}
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case uint64:
		if t <= 1<<62 {
			return int64(t)
		}
	}
	s.fail(k, "must be an integer")
	return def
}

// optInteger returns nil when the key is absent or null.
func (s *section) optInteger(k string, def *int64) *int64 {
	if _, ok := s.raw(k); !ok {
		if _, present := s.m[k]; present {
			return nil // explicit null
		}
		return def
	}
	v := s.integer(k, 0)
	return &v
}

func (s *section) strList(k string, def []string) []string {
	v, ok := s.raw(k)
	if !ok {
		return def
	}
	items, ok := v.([]any)
	if !ok {
		s.fail(k, "must be a list of strings")
		return def
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		t, ok := it.(string)
		if !ok {
			s.fail(k, "entry %d must be a string", i)
			return def
		}
		out = append(out, t)
	}
	return out
}

func (s *section) strMap(k string) map[string]string {
	v, ok := s.raw(k)
	if !ok {
		return map[string]string{}
	}
	m, ok := v.(map[string]any)
	if !ok {
		s.fail(k, "must be a mapping of strings")
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for mk, mv := range m {
		t, ok := mv.(string)
		if !ok {
			s.fail(k+"."+mk, "must be a string")
			continue
		}
		out[mk] = t
	}
	return out
}

// unknown returns the keys present in the mapping that were never read.
func (s *section) unknown() []string {
	var out []string
	for k := range s.m {
		if !s.seen[k] {
			out = append(out, s.key(k))
		}
	}
	sort.Strings(out)
	return out
}
