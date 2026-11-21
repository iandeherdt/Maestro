// Package logging sets up the service log (SPEC section 16.1): one event
// per line, key=value pairs, UTC timestamps.
//
// Issue titles, agent messages and error text end up in log values. They
// are untrusted, so every string value is length-capped, and the handler
// quotes any value that contains a space, a quote, an equals sign or a
// control character. A value can therefore never start a new log line or
// add a key of its own.
package logging

import (
	"io"
	"log/slog"
	"time"
	"unicode/utf8"
)

// MaxValueBytes caps a single string value.
const MaxValueBytes = 2000

// New returns a logger that writes to w.
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: replace}))
}

func replace(_ []string, a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindTime:
		a.Value = slog.TimeValue(a.Value.Time().UTC().Round(0))
	case slog.KindString:
		a.Value = slog.StringValue(capString(a.Value.String()))
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case error:
			a.Value = slog.StringValue(capString(v.Error()))
		case time.Time:
			a.Value = slog.TimeValue(v.UTC())
		}
	}
	return a
}

func capString(s string) string {
	if len(s) <= MaxValueBytes {
		return s
	}
	s = s[:MaxValueBytes]
	for len(s) > 0 && !utf8.ValidString(s) && len(s) > MaxValueBytes-utf8.UTFMax {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// Issue returns the context every issue-related line must carry.
func Issue(id, identifier string) slog.Attr {
	return slog.Group("", slog.String("issue_id", id), slog.String("issue_identifier", identifier))
}
