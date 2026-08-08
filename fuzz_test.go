package migrate

import (
	"strings"
	"testing"
)

// FuzzLiteral checks that arbitrary default values remain quoted SQL literals.
func FuzzLiteral(f *testing.F) {
	f.Add("plain")
	f.Add("it's")
	f.Add(`back\slash`)
	f.Add(`mix'\''ed`)
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		for _, backslash := range []bool{false, true} {
			lit, err := literal(s, backslash)
			if err != nil {
				t.Fatalf("literal(%q) failed: %v", s, err)
			}
			if len(lit) < 2 || !strings.HasPrefix(lit, "'") || !strings.HasSuffix(lit, "'") {
				t.Fatalf("literal(%q) = %s is not quoted", s, lit)
			}
			inner := lit[1 : len(lit)-1]
			if backslash {
				inner = strings.ReplaceAll(inner, `\\`, "")
				if strings.Contains(inner, `\`) {
					t.Fatalf("literal(%q) leaves an unescaped backslash: %s", s, lit)
				}
			}
			if strings.Contains(strings.ReplaceAll(inner, "''", ""), "'") {
				t.Fatalf("literal(%q) leaves an unescaped quote: %s", s, lit)
			}
		}
	})
}

// FuzzQuoterIdent checks that arbitrary identifiers stay within their quotes.
func FuzzQuoterIdent(f *testing.F) {
	f.Add("users")
	f.Add(`we"ird`)
	f.Add("back`tick")
	f.Fuzz(func(t *testing.T, s string) {
		for _, q := range []quoter{pgQ, myQ} {
			id := q.ident(s)
			c := string(q)
			if !strings.HasPrefix(id, c) || !strings.HasSuffix(id, c) {
				t.Fatalf("ident(%q) = %s is not wrapped in %s", s, id, c)
			}
			inner := id[1 : len(id)-1]
			if strings.Contains(strings.ReplaceAll(inner, c+c, ""), c) {
				t.Fatalf("ident(%q) leaves an unescaped quote: %s", s, id)
			}
		}
	})
}

// FuzzGuessParentTable rejects malformed inferred table names.
func FuzzGuessParentTable(f *testing.F) {
	f.Add("user_id")
	f.Add("_id")
	f.Add("y_id")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		got := guessParentTable(s)
		if got != "" && !strings.HasSuffix(s, "_id") {
			t.Fatalf("guessParentTable(%q) = %q for a column without _id", s, got)
		}
	})
}
