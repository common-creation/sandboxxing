package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// errInterrupt is returned when the user presses Ctrl-C.
var errInterrupt = errors.New("interrupt")

// errEOF is returned when the user presses Ctrl-D on an empty line.
var errEOF = errors.New("eof")

// lineReader reads one line from an interactive terminal. When echo is
// enabled it performs the usual line editing: the typed characters are
// echoed, backspace removes them and Ctrl-C abandons the line.
type lineReader struct {
	in   io.Reader
	out  io.Writer
	echo bool
}

func (l *lineReader) readLine() (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := l.in.Read(buf)
		if n > 0 {
			c := buf[0]
			switch c {
			case '\n', '\r':
				if l.echo {
					fmt.Fprint(l.out, "\r\n")
				}
				return b.String(), nil
			case 0x03: // Ctrl-C
				if l.echo {
					fmt.Fprint(l.out, "^C\r\n")
				}
				return "", errInterrupt
			case 0x04: // Ctrl-D
				if b.Len() == 0 {
					if l.echo {
						fmt.Fprint(l.out, "\r\n")
					}
					return "", errEOF
				}
			case 0x7f, 0x08: // Backspace
				if b.Len() > 0 {
					s := b.String()
					b.Reset()
					b.WriteString(s[:len(s)-1])
					if l.echo {
						fmt.Fprint(l.out, "\b \b")
					}
				}
			case 0x15: // Ctrl-U: clear the line
				b.Reset()
				if l.echo {
					fmt.Fprint(l.out, "\r\x1b[K")
				}
			default:
				if c >= 0x20 && c != 0x7f {
					b.WriteByte(c)
					if l.echo {
						l.out.Write([]byte{c})
					}
				}
			}
		}
		if err != nil {
			if b.Len() > 0 {
				return b.String(), nil
			}
			return "", err
		}
	}
}
