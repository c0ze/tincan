package host

import (
	"errors"
	"fmt"
	"strings"
)

// Vars holds the per-message values substituted into an exec template.
type Vars struct {
	Body string // message body (the prompt)
	Out  string // per-message reply file, when the preset replies via file
	Room string // room directory; also the agent's working directory
	Name string // hosted listener name
	ID   string // message id
}

// Render substitutes {body}, {out}, {room}, {name} and {id} in every argv
// token of exec and returns a new slice. Substitution is per token: a token
// that is exactly "{body}" becomes one argv element holding the whole body —
// spaces, newlines and quotes included. Nothing is re-split or passed
// through a shell, and a Replacer makes a single pass, so text substituted
// in (a body that itself contains "{out}") is never rescanned.
func Render(exec []string, v Vars) []string {
	r := strings.NewReplacer(
		"{body}", v.Body,
		"{out}", v.Out,
		"{room}", v.Room,
		"{name}", v.Name,
		"{id}", v.ID,
	)
	out := make([]string, len(exec))
	for i, tok := range exec {
		out[i] = r.Replace(tok)
	}
	return out
}

// SplitTemplate splits a --exec template string into argv tokens on
// unquoted whitespace. Single and double quotes group a token (the quotes
// themselves are dropped, so "{body}" is the bare token {body}); a backslash
// outside quotes escapes the next character. This is the only shell-like
// parsing tincan does, and it happens before any placeholder is substituted,
// so a message body can never be re-tokenized.
func SplitTemplate(s string) ([]string, error) {
	var (
		args  []string
		cur   strings.Builder
		inTok bool
		quote rune // 0, '\'' or '"'
	)
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inTok = true
		case c == '\\':
			if i+1 >= len(runes) {
				return nil, errors.New("exec template: trailing backslash")
			}
			i++
			cur.WriteRune(runes[i])
			inTok = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inTok {
				args = append(args, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteRune(c)
			inTok = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("exec template: unterminated %c quote", quote)
	}
	if inTok {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, errors.New("exec template: empty")
	}
	return args, nil
}
