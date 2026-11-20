package memory

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// ErrLosesSpecifics refuses a rewrite that leaves out something exact from
// the entries it replaces.
var ErrLosesSpecifics = fmt.Errorf("the new text leaves out specifics of the entries it replaces")

var codeSpan = regexp.MustCompile("`([^`\n]{1,200})`")

// specifics returns the parts of a text that a rewrite of it must not lose
// by accident: what is written as code, and the words that carry a number or
// are plainly an identifier (a path, a name with an underscore, a camel-case
// name). They are what a merged entry most often drops while reading well,
// and they can be found without understanding the text.
func specifics(text string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.Join(strings.Fields(s), " ")
		if key := strings.ToLower(s); len(s) >= 2 && !seen[key] {
			seen[key] = true
			out = append(out, s)
		}
	}
	for _, m := range codeSpan.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, word := range strings.Fields(codeSpan.ReplaceAllString(text, " ")) {
		word = strings.Trim(word, ".,;:!?()[]{}<>\"'*_~|→—–-")
		var digit, inner, camel bool
		for i, r := range word {
			switch {
			case unicode.IsDigit(r):
				digit = true
			case (r == '/' || r == '_') && i > 0:
				inner = true
			case unicode.IsUpper(r) && i > 0 && unicode.IsLower(rune(word[i-1])):
				camel = true
			}
		}
		if digit || inner || camel {
			add(word)
		}
	}
	return out
}

// lostSpecifics lists the specifics of the old texts that the new text does
// not have and that no drop accounts for. A drop is "SPECIFIC: why".
func lostSpecifics(text string, olds, drops []string) []string {
	have := strings.ToLower(strings.Join(strings.Fields(text), " "))
	var dropped []string
	for _, d := range drops {
		what, _, _ := strings.Cut(d, ": ")
		if what = strings.ToLower(strings.Join(strings.Fields(strings.Trim(what, "`")), " ")); len(what) >= 2 {
			dropped = append(dropped, what)
		}
	}
	var lost []string
	seen := map[string]bool{}
	for _, old := range olds {
		for _, s := range specifics(old) {
			key := strings.ToLower(s)
			if seen[key] || strings.Contains(have, key) {
				continue
			}
			seen[key] = true
			excused := false
			for _, d := range dropped {
				excused = excused || strings.Contains(d, key) || strings.Contains(key, d)
			}
			if !excused {
				lost = append(lost, s)
			}
		}
	}
	return lost
}

// lostMessage is the refusal an agent reads: what is missing, and the two
// ways on.
func lostMessage(lost []string) string {
	shown := lost
	more := ""
	if len(shown) > 12 {
		shown, more = shown[:12], fmt.Sprintf(" and %d more", len(lost)-12)
	}
	quoted := make([]string, len(shown))
	for i, s := range shown {
		quoted[i] = "`" + s + "`"
	}
	return fmt.Sprintf("%s%s. Put them in the new text; or, for each one that should go (a value a correction replaced, say), "+
		"add --drops 'SPECIFIC: why', which is kept in the run's log", strings.Join(quoted, ", "), more)
}
