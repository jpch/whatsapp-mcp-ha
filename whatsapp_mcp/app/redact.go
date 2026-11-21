package main

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
)

// Log redaction. The add-on never logs message contents, but whatsmeow's warnings and errors, and
// errors passed through from it, can name other people's JIDs and phone numbers. Every log line goes
// through redactingHandler, which masks them in the message and in all string, error and Stringer
// attributes (also inside groups). Numbers that are not strings (counts, sizes) are left alone.
var (
	// JIDs: user, group (digits-digits), LID; with an optional device part (":12" or ".0:12").
	jidRe = regexp.MustCompile(`[0-9][0-9-]{4,}(?:[.:][0-9]+)*@[a-z.]+`)
	// Bare phone numbers: 8 or more digits, optionally with a leading +.
	phoneLikeRe = regexp.MustCompile(`\+?[0-9]{8,}`)
	// Grouped phone numbers: digit groups separated by spaces or dashes (not dots, so IP addresses stay), e.g. "+351 900 000 000",
	// "351-900-000-000", "(351) 900 000 000". Masked when they hold 9 or more digits, so ISO dates
	// (8 digits) and times (colons) stay readable.
	groupedPhoneRe = regexp.MustCompile(`\+?\(?[0-9]{1,4}\)?(?:[ \-]\(?[0-9]{1,5}\)?){2,6}`)
)

func redactText(s string) string {
	s = jidRe.ReplaceAllString(s, "[jid]")
	s = groupedPhoneRe.ReplaceAllStringFunc(s, func(m string) string {
		digits := 0
		for _, r := range m {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		if digits >= 9 {
			return "[number]"
		}
		return m
	})
	return phoneLikeRe.ReplaceAllString(s, "[number]")
}

type redactingHandler struct{ next slog.Handler }

func newRedactingHandler(next slog.Handler) slog.Handler { return &redactingHandler{next: next} }

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, redactText(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redactAttr(a)
	}
	return &redactingHandler{next: h.next.WithAttrs(red)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, redactText(v.String()))
	case slog.KindGroup:
		g := v.Group()
		red := make([]any, len(g))
		for i, ga := range g {
			red[i] = redactAttr(ga)
		}
		return slog.Group(a.Key, red...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, redactText(x.Error()))
		case fmt.Stringer:
			return slog.String(a.Key, redactText(x.String()))
		case []byte:
			return slog.String(a.Key, redactText(string(x)))
		default:
			return slog.String(a.Key, redactText(fmt.Sprint(x)))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}
