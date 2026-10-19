package console

import (
	"errors"
	"regexp"
	"strings"
)

// The statements the console runs on a node: fixed texts (spec 003). Nothing from the browser becomes SQL but a
// session's ops id, and only once it has the ops id's form.
const (
	stmtInfo     = "SELECT * FROM platform.console_info()"
	stmtLoad     = "SELECT * FROM platform.node_load"
	stmtSessions = "SELECT id, subject, roles, door, idle_seconds, expires_at, audit_level, audit_level_source, " +
		"profile_level, profile_source, groups, charged_group FROM platform.sessions"
	stmtDrain  = "DRAIN NODE"
	stmtResume = "RESUME NODE"
)

// opsID is a session's id for operations as duckdb-acl mints it: 12 hex digits (spec 050), never its handle.
var opsID = regexp.MustCompile(`^[0-9A-Fa-f]{12}$`)

// errBadSessionID is an id that is not a session's ops id.
var errBadSessionID = errors.New("not a session id")

// killSession is `KILL SESSION '<id>'` (duckdb-acl spec 118) for an id of the ops id's form.
func killSession(id string) (string, error) {
	if !opsID.MatchString(id) {
		return "", errBadSessionID
	}
	return "KILL SESSION '" + strings.ToUpper(id) + "'", nil
}
