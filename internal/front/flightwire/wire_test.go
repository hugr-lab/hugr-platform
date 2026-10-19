package flightwire

import "testing"

func varintOf(v uint64) []byte {
	var b []byte
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func bytesField(number uint64, payload []byte) []byte {
	return append(append(varintOf(number<<3|2), varintOf(uint64(len(payload)))...), payload...)
}

func anyMsg(kind string, value []byte) []byte {
	return append(bytesField(1, []byte("type.googleapis.com/"+typePackage+kind)), bytesField(2, value)...)
}

func descriptor(kind, handle string) []byte {
	// FlightDescriptor{type = CMD (2), cmd}
	return append([]byte{0x08, 0x02}, bytesField(2, anyMsg(kind, bytesField(1, []byte(handle))))...)
}

func TestLabels(t *testing.T) {
	handle := "node-b.65BC150114D603160B8B3BDEF240A4F6"
	ticket := bytesField(1, anyMsg("TicketStatementQuery", bytesField(1, []byte(handle))))
	put := append(bytesField(1, descriptor("CommandPreparedStatementQuery", handle)), bytesField(1000, make([]byte, 100))...)
	closeAction := append(bytesField(1, []byte("ClosePreparedStatement")),
		bytesField(2, anyMsg("ActionClosePreparedStatementRequest", bytesField(1, []byte(handle))))...)
	for _, c := range []struct {
		method string
		first  []byte
		want   string
	}{
		{"DoGet", ticket, "node-b"},
		{"DoGet", bytesField(1, anyMsg("CommandPreparedStatementQuery", bytesField(1, []byte(handle)))), "node-b"},
		{"GetFlightInfo", descriptor("CommandPreparedStatementQuery", handle), "node-b"},
		{"GetSchema", descriptor("CommandPreparedStatementUpdate", handle), "node-b"},
		{"DoPut", put, "node-b"},
		{"DoAction", closeAction, "node-b"},
		// what names no node: an ad-hoc statement, a catalog command, a handle without a label, another action
		{"GetFlightInfo", descriptor("CommandStatementQuery", handle), ""},
		{"DoGet", bytesField(1, anyMsg("CommandGetTables", nil)), ""},
		{"DoGet", bytesField(1, anyMsg("TicketStatementQuery", bytesField(1, []byte("65BC150114D603160B8B3BDEF240A4F6")))), ""},
		{"DoAction", append(bytesField(1, []byte("CancelQuery")), bytesField(2, anyMsg("ActionClosePreparedStatementRequest", bytesField(1, []byte(handle))))...), ""},
		{"Handshake", ticket, ""},
		// protobuf's reading: another URL host, the last of a repeated singular field
		{"DoGet", bytesField(1, append(bytesField(1, []byte("example.com/x/"+typePackage+"TicketStatementQuery")),
			bytesField(2, bytesField(1, []byte(handle)))...)), "node-b"},
		{"DoGet", bytesField(1, anyMsg("TicketStatementQuery", append(bytesField(1, []byte("node-x.AA")),
			bytesField(1, []byte(handle))...))), "node-b"},
		// unreadable: truncated, a length past the end, garbage
		{"DoGet", ticket[:len(ticket)-5], ""},
		{"DoGet", []byte{0x0a, 0xff, 0xff, 0x03}, ""},
		{"DoGet", []byte{0xff, 0xff, 0xff}, ""},
		{"DoGet", nil, ""},
	} {
		if got := Label(c.method, c.first); got != c.want {
			t.Errorf("%s %x: %q, want %q", c.method, c.first, got, c.want)
		}
	}
}
