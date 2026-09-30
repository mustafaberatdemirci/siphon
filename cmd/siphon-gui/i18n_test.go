package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// usedTexts reads the package's sources and returns every text given to T,
// Tf or msg as a literal, with where it is.
func usedTexts(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "translations_") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || (fn.Name != "T" && fn.Name != "Tf" && fn.Name != "msg") {
				return true
			}
			s, ok := literal(call.Args[0])
			if !ok {
				// A variable is fine: it passes on a text marked where it
				// was written (msg, or Tf's own format). Anything else, a
				// text built at run time, has no catalog entry to find.
				switch call.Args[0].(type) {
				case *ast.Ident, *ast.SelectorExpr:
				default:
					t.Errorf("%s: %s is given a text built at run time; the catalogs can't have it",
						fset.Position(call.Pos()), fn.Name)
				}
				return true
			}
			out[s] = fset.Position(call.Args[0].Pos()).String()
			return true
		})
	}
	return out
}

// literal is the value of a string literal, or of literals joined with +.
func literal(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		a, ok1 := literal(e.X)
		b, ok2 := literal(e.Y)
		return a + b, ok1 && ok2
	case *ast.ParenExpr:
		return literal(e.X)
	}
	return "", false
}

var verbRe = regexp.MustCompile(`%(?:\[\d+\])?[-+# 0]*\d*(?:\.\d+)?[a-zA-Z%]`)

// verbs are the formatting verbs of a text, sorted, without %%; explicit
// argument indexes are dropped so a translation may reorder them.
func verbs(s string) string {
	var v []string
	for _, m := range verbRe.FindAllString(s, -1) {
		if m == "%%" {
			continue
		}
		v = append(v, m[len(m)-1:])
	}
	sort.Strings(v)
	return strings.Join(v, "")
}

// Every text the window asks for has a Turkish and a German translation,
// with the same formatting verbs, and no translation is left over.
func TestCatalogsAreComplete(t *testing.T) {
	used := usedTexts(t)
	if len(used) < 100 {
		t.Fatalf("only %d texts found; is the scan still finding T calls?", len(used))
	}
	for code, cat := range catalogs {
		for s, where := range used {
			tr, ok := cat[s]
			if !ok || strings.TrimSpace(tr) == "" {
				t.Errorf("%s: no %s translation for %q", where, code, s)
				continue
			}
			if verbs(tr) != verbs(s) {
				t.Errorf("%s: %s translation of %q has verbs %q, want %q", where, code, s, verbs(tr), verbs(s))
			}
		}
		for s := range cat {
			if _, ok := used[s]; !ok {
				t.Errorf("%s catalog has %q, which the code no longer asks for", code, s)
			}
		}
	}
}

// TestPrintMissingTranslations lists what the catalogs lack, as Go map
// lines, when SIPHON_I18N_MISSING is set; a help for translating.
func TestPrintMissingTranslations(t *testing.T) {
	if os.Getenv("SIPHON_I18N_MISSING") == "" {
		t.Skip("set SIPHON_I18N_MISSING=1 to list the texts without a translation")
	}
	used := usedTexts(t)
	keys := make([]string, 0, len(used))
	for s := range used {
		keys = append(keys, s)
	}
	sort.Strings(keys)
	for code, cat := range catalogs {
		for _, s := range keys {
			if _, ok := cat[s]; !ok {
				fmt.Printf("%s\t%q: \"\",\n", code, s)
			}
		}
	}
}

func TestResolveLanguage(t *testing.T) {
	cases := []struct {
		choice, system, want language
	}{
		{langAuto, langTurkish, langTurkish},
		{langAuto, langGerman, langGerman},
		{langAuto, "fr", langEnglish},
		{langAuto, "", langEnglish},
		{langGerman, langTurkish, langGerman},
		{langEnglish, langTurkish, langEnglish},
		{"xx", langTurkish, langEnglish},
	}
	for _, c := range cases {
		if got := resolveLanguage(c.choice, c.system); got != c.want {
			t.Errorf("resolveLanguage(%q, %q) = %q, want %q", c.choice, c.system, got, c.want)
		}
	}
}

func TestTFallsBackToEnglish(t *testing.T) {
	defer func(l language) { current = l }(current)
	current = langTurkish
	if got := T("a text no catalog has"); got != "a text no catalog has" {
		t.Errorf("T = %q", got)
	}
	current = langEnglish
	if got := Tf("%d files added to the queue.", 3); got != "3 files added to the queue." {
		t.Errorf("Tf = %q", got)
	}
}
