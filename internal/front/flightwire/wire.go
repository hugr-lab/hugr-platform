// Package flightwire reads the one thing the front needs from a Flight call's first message (spec 002): the label of
// the node that minted the handle the call names (duckdb-acl spec 134's addendum: a ticket's and a prepared
// statement's handle are `<label>.<random>`). Only for a call that carries no acl_node cookie. It reads a few
// protobuf fields and writes nothing:
//
//	DoGet                          Ticket{ticket} -> Any -> TicketStatementQuery{statement_handle} |
//	                               CommandPreparedStatementQuery{prepared_statement_handle}
//	GetFlightInfo / GetSchema /    FlightDescriptor{cmd} -> Any -> CommandPreparedStatementQuery |
//	PollFlightInfo                 CommandPreparedStatementUpdate {prepared_statement_handle}
//	DoPut / DoExchange             FlightData{flight_descriptor} -> as above
//	DoAction                       Action{type ClosePreparedStatement, body} -> Any ->
//	                               ActionClosePreparedStatementRequest{prepared_statement_handle}
package flightwire

import (
	"strings"

	"github.com/hugr-lab/hugr-platform/internal/front/route"
)

// Prefix is how much of a first message is read: the descriptor or the ticket comes first and is small, a DoPut's
// batch after it is never looked at.
const Prefix = 64 << 10

const typePrefix = "type.googleapis.com/arrow.flight.protocol.sql."

// field reads the length-delimited field `number` of a protobuf message (the first occurrence); false when it is
// absent or the message is not readable.
func field(msg []byte, number uint64) ([]byte, bool) {
	at := 0
	for at < len(msg) {
		key, n := varint(msg[at:])
		if n == 0 {
			return nil, false
		}
		at += n
		switch key & 7 {
		case 0: // varint
			_, n := varint(msg[at:])
			if n == 0 {
				return nil, false
			}
			at += n
		case 1: // 64-bit
			at += 8
		case 5: // 32-bit
			at += 4
		case 2: // length-delimited
			length, n := varint(msg[at:])
			if n == 0 || length > uint64(len(msg)-at-n) {
				return nil, false
			}
			at += n
			if key>>3 == number {
				return msg[at : at+int(length)], true
			}
			at += int(length)
		default: // groups: not in Flight
			return nil, false
		}
	}
	return nil, false
}

func varint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// anyOf reads an Any: its type's short name (after the Flight SQL prefix) and its value.
func anyOf(msg []byte) (string, []byte) {
	url, ok := field(msg, 1)
	if !ok || !strings.HasPrefix(string(url), typePrefix) {
		return "", nil
	}
	value, _ := field(msg, 2)
	return strings.TrimPrefix(string(url), typePrefix), value
}

// commandHandle: the prepared statement's handle a FlightDescriptor's command names.
func commandHandle(descriptor []byte) []byte {
	cmd, ok := field(descriptor, 2)
	if !ok {
		return nil
	}
	switch kind, value := anyOf(cmd); kind {
	case "CommandPreparedStatementQuery", "CommandPreparedStatementUpdate":
		handle, _ := field(value, 1)
		return handle
	}
	return nil
}

// Label of the node that minted the handle a call's first message names; empty when it names none, or one without a
// label (a valid node label only).
func Label(method string, first []byte) string {
	var handle []byte
	switch method {
	case "DoGet":
		// a statement's ticket is its handle; a prepared statement's is its command, as Flight SQL's server base
		// hands it out
		if ticket, ok := field(first, 1); ok {
			switch kind, value := anyOf(ticket); kind {
			case "TicketStatementQuery", "CommandPreparedStatementQuery":
				handle, _ = field(value, 1)
			}
		}
	case "GetFlightInfo", "GetSchema", "PollFlightInfo":
		handle = commandHandle(first)
	case "DoPut", "DoExchange":
		if descriptor, ok := field(first, 1); ok {
			handle = commandHandle(descriptor)
		}
	case "DoAction":
		if kind, ok := field(first, 1); ok && string(kind) == "ClosePreparedStatement" {
			if body, ok := field(first, 2); ok {
				if kind, value := anyOf(body); kind == "ActionClosePreparedStatementRequest" {
					handle, _ = field(value, 1)
				}
			}
		}
	}
	return route.ConnectionLabel(string(handle))
}
