// Package quackwire reads the little of quack's wire the front needs (spec 002): a message's header - its type and
// its connection id - and the text of an error answer. The front writes no message of its own and forwards every
// body untouched.
//
// A message is two objects of duckdb's BinarySerializer, the header and the body. A field is its id (a u16,
// little-endian) and its value: an unsigned integer is a LEB128 varint, a string its length (a varint) and its bytes.
// A field holding its default is left out, and an object ends with the id 0xFFFF. The header's fields are `type` (1),
// `connection_id` (2, absent on a CONNECT) and `client_query_id` (3).
package quackwire

import (
	"bufio"
	"errors"
	"io"
)

// Type of a message.
type Type uint8

// The types the front tells apart.
const (
	Connect       Type = 1
	ConnectAnswer Type = 2
	Prepare       Type = 3
	Fetch         Type = 7
	SendData      Type = 9
	Success       Type = 10
	Disconnect    Type = 11
	Cancel        Type = 12
	Acknowledge   Type = 15
	Heartbeat     Type = 16
	Error         Type = 100
)

// MaxConnectionID is the longest connection id the front reads: a node's are `<label>.<32 hex>`, a label at most 63.
const MaxConnectionID = 256

// HeaderLimit is how many bytes of a body the header may take (type, connection id, query id, the end).
const HeaderLimit = 2 + 10 + 2 + 10 + MaxConnectionID + 2 + 10 + 2

const (
	fieldType         = 1
	fieldConnectionID = 2
	fieldQueryID      = 3
	fieldEnd          = 0xFFFF
)

// ErrMalformed is a body that does not start with a header the front can read.
var ErrMalformed = errors.New("quack: the body does not start with a message header")

// Header is what the front reads of a message.
type Header struct {
	Type         Type
	ConnectionID string // empty on a CONNECT
}

type reader struct {
	b   []byte
	pos int
}

func (r *reader) field() (uint16, bool) {
	if r.pos+2 > len(r.b) {
		return 0, false
	}
	id := uint16(r.b[r.pos]) | uint16(r.b[r.pos+1])<<8
	r.pos += 2
	return id, true
}

func (r *reader) peekField() (uint16, bool) {
	at := r.pos
	id, ok := r.field()
	r.pos = at
	return id, ok
}

func (r *reader) varint() (uint64, bool) {
	var v uint64
	for shift := uint(0); shift < 70; shift += 7 {
		if r.pos >= len(r.b) {
			return 0, false
		}
		c := r.b[r.pos]
		r.pos++
		if shift == 63 && c > 1 {
			return 0, false // past 64 bits
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, true
		}
	}
	return 0, false
}

func (r *reader) str(limit int) (string, bool) {
	n, ok := r.varint()
	if !ok || n > uint64(limit) || r.pos+int(n) > len(r.b) {
		return "", false
	}
	s := string(r.b[r.pos : r.pos+int(n)])
	r.pos += int(n)
	return s, true
}

// header reads the header's type and connection id. It stops after the connection id: a field the front does not
// know may follow, and it needs none of them.
func (r *reader) header() (Header, bool) {
	if id, ok := r.field(); !ok || id != fieldType {
		return Header{}, false
	}
	t, ok := r.varint()
	if !ok || t > 0xff {
		return Header{}, false
	}
	h := Header{Type: Type(t)}
	if id, ok := r.peekField(); !ok {
		return Header{}, false
	} else if id == fieldConnectionID {
		r.field()
		if h.ConnectionID, ok = r.str(MaxConnectionID); !ok {
			return Header{}, false
		}
	}
	return h, true
}

// ParseHeader reads the header from the start of a body.
func ParseHeader(body []byte) (Header, error) {
	r := reader{b: body}
	h, ok := r.header()
	if !ok {
		return Header{}, ErrMalformed
	}
	return h, nil
}

// PeekHeader reads the header from a body's first bytes without consuming them: the body is forwarded whole.
func PeekHeader(body *bufio.Reader) (Header, error) {
	b, err := body.Peek(HeaderLimit)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return Header{}, err
	}
	return ParseHeader(b)
}

// ErrorText is the message of an error answer, and false for a body that is no error answer the front can read.
func ErrorText(body []byte) (string, bool) {
	r := reader{b: body}
	h, ok := r.header()
	if !ok || h.Type != Error {
		return "", false
	}
	// the rest of the header: the query id, then its end - anything else is a header of another revision
	if id, ok := r.peekField(); ok && id == fieldQueryID {
		r.field()
		if _, ok := r.varint(); !ok {
			return "", false
		}
	}
	if id, ok := r.field(); !ok || id != fieldEnd {
		return "", false
	}
	id, ok := r.peekField()
	if !ok || id == fieldEnd {
		return "", ok // an error answer without a message
	}
	if id != 1 {
		return "", false
	}
	r.field()
	return r.str(64 << 10)
}
