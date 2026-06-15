package mailbox

import (
	"context"
	"strings"
)

// MatchFrom resolves an incoming From: address to at most one filter row,
// applying the precedence "exact address beats wildcard domain". The
// boolean second return indicates whether any filter matched at all.
//
// addr is lowercased and trimmed before lookup. Empty or malformed
// addresses (no '@') get no match.
//
// All currently-active filters are scanned in-memory — the cache lives
// on the Repo and is invalidated by the filter CRUD handlers in Phase 8.
// Two reads (one for the cache populate path, zero on hot path) are
// cheaper than a SQL round-trip per polled message.
func MatchFrom(ctx context.Context, repo *Repo, addr string) (Filter, bool, error) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" || !strings.Contains(addr, "@") {
		return Filter{}, false, nil
	}
	domain := "@" + addr[strings.Index(addr, "@")+1:]

	filters, err := repo.cachedFilters(ctx)
	if err != nil {
		return Filter{}, false, err
	}

	// Exact-address pass first — wins regardless of any domain match.
	for _, f := range filters {
		if f.Kind == FilterKindAddress && f.Pattern == addr {
			return f, true, nil
		}
	}
	// Domain pass.
	for _, f := range filters {
		if f.Kind == FilterKindDomain && f.Pattern == domain {
			return f, true, nil
		}
	}
	return Filter{}, false, nil
}

// normaliseFilterPattern lowercases the input and (for domain kind)
// ensures the leading '@'. Returns the canonical form to persist.
// Returns empty string when the input is not a usable pattern for the
// given kind — the caller is expected to reject the request.
func normaliseFilterPattern(kind FilterKind, raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch kind {
	case FilterKindAddress:
		if !strings.Contains(s, "@") || strings.HasPrefix(s, "@") || strings.HasSuffix(s, "@") {
			return ""
		}
		return s
	case FilterKindDomain:
		s = strings.TrimPrefix(s, "*")
		if !strings.HasPrefix(s, "@") {
			s = "@" + s
		}
		if s == "@" || strings.Count(s, "@") != 1 {
			return ""
		}
		return s
	}
	return ""
}
