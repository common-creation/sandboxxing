package sshserver

import (
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
	"unicode"
)

func pemEncode(block *pem.Block) []byte {
	return pem.EncodeToMemory(block)
}

func base64URL(b []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
}

// shlex splits a command line the way a POSIX shell would, without performing
// any expansion. It is applied to the command string sent with an SSH "exec"
// request so that both `ssh host ls -l` and `ssh host 'ls -l'` work.
func shlex(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	quote := rune(0)
	escaped := false
	started := false
	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
			started = true
		case r == '\\' && quote != '\'':
			escaped = true
			started = true
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			started = true
		case unicode.IsSpace(r):
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if escaped {
		cur.WriteRune('\\')
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in command")
	}
	flush()
	return args, nil
}
