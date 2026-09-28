package sshserver

import (
	"bytes"
	"io"
)

// crlf is the line ending an interactive terminal expects.
var crlf = []byte("\r\n")

// crlfWriter writes the bytes it receives with every bare line feed replaced
// by CRLF.
//
// A control session has no pseudo terminal, so nothing applies the ONLCR
// translation a terminal usually performs. A client that asks for a PTY puts
// its own terminal into raw mode, and a bare LF then only moves the cursor one
// line down while keeping the column: every output line would start where the
// previous one ended and the display would drift to the right. Container
// logins do not need this because their PTY performs the translation.
type crlfWriter struct {
	w io.Writer
	// lastCR records whether the last byte sent was a carriage return, so
	// that a CRLF pair split over two Write calls is not turned into CR CR
	// LF.
	lastCR bool
}

func newCRLFWriter(w io.Writer) *crlfWriter { return &crlfWriter{w: w} }

// Write implements io.Writer. The return value counts the input bytes that
// were converted and sent, as the contract requires.
func (c *crlfWriter) Write(p []byte) (int, error) {
	consumed := 0
	for consumed < len(p) {
		i := bytes.IndexByte(p[consumed:], '\n')
		if i < 0 {
			// No line feed left: the rest goes out unchanged.
			n, err := c.w.Write(p[consumed:])
			if n > 0 {
				c.lastCR = p[consumed+n-1] == '\r'
			}
			return consumed + n, err
		}
		at := consumed + i
		bare := !c.lastCR && (at == 0 || p[at-1] != '\r')
		if at > consumed {
			n, err := c.w.Write(p[consumed:at])
			consumed += n
			if err != nil {
				return consumed, err
			}
			if consumed < at {
				return consumed, io.ErrShortWrite
			}
		}
		out := p[at : at+1]
		if bare {
			out = crlf
		}
		n, err := c.w.Write(out)
		c.lastCR = n > 0 && out[n-1] == '\r'
		if n == len(out) {
			consumed = at + 1
		}
		if err != nil {
			return consumed, err
		}
		if n < len(out) {
			return consumed, io.ErrShortWrite
		}
	}
	return consumed, nil
}
