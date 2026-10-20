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

	// round 2: what the engine holds - duckdb's own functions, in the policy's meta and environment categories
	// (the owner's Q7); a sum is the console's, an aggregate would need another category
	stmtMemory = "SELECT tag, memory_usage_bytes, temporary_storage_bytes FROM duckdb_memory()"
	stmtSpill  = "SELECT size FROM duckdb_temporary_files()"
	stmtLimits = "SELECT current_setting('memory_limit') AS memory_limit, " +
		"current_setting('max_temp_directory_size') AS max_temp_directory_size"
	// what the node came up with. Its sources are drift's database rows: under a principal duckdb_databases() is the
	// ACL's metadata surface (the virtual catalogs), so the profile's own views name the physical ones - never a
	// database's path (an ATTACH string may carry a credential)
	stmtExtensions = "SELECT extension_name, loaded, installed, extension_version, install_mode, installed_from " +
		"FROM duckdb_extensions() WHERE loaded OR installed"
	stmtDrift     = "SELECT kind, name, state, node_value, profile_value, scope FROM platform.drift"
	stmtEffective = "SELECT scope, kind, name, class, version FROM platform.cluster_effective"
	stmtItems     = "SELECT scope, kind, name, spec, class, version, depends_on, comment FROM platform.cluster_items"
	stmtLineage   = "SELECT status, sending, level, namespace FROM platform.lineage_status"
	// the node's ring, newest first - never a statement's text, a claim's value or a handle (duckdb-acl spec 069)
	stmtEvents = "SELECT ts, seq, kind, level, door, session, subject, roles, statement, verdict, reason_code, " +
		"rows, duration_us, correlation_id, traceparent, detail FROM platform.audit_events ORDER BY seq DESC LIMIT 200"
)

// The levels a session's tracing takes (duckdb-acl spec 069, 074, 118): "" inherits - DEFAULT clears the audit's
// override, OFF the profile's.
var (
	auditLevels   = map[string]string{"": "DEFAULT", "off": "OFF", "denied": "DENIED", "decisions": "DECISIONS", "all": "ALL"}
	profileLevels = map[string]string{"": "OFF", "sampled": "SAMPLED", "all": "ALL"}
)

// errBadLevel is a level no tracing statement takes.
var errBadLevel = errors.New("not a level")

// auditLevel is `SET SESSION '<id>' AUDIT LEVEL <level>`.
func auditLevel(id, level string) (string, error) {
	if !opsID.MatchString(id) {
		return "", errBadSessionID
	}
	word, ok := auditLevels[level]
	if !ok {
		return "", errBadLevel
	}
	return "SET SESSION '" + strings.ToUpper(id) + "' AUDIT LEVEL " + word, nil
}

// profileLevel is `PROFILE SESSION '<id>' <level>`.
func profileLevel(id, level string) (string, error) {
	if !opsID.MatchString(id) {
		return "", errBadSessionID
	}
	word, ok := profileLevels[level]
	if !ok {
		return "", errBadLevel
	}
	return "PROFILE SESSION '" + strings.ToUpper(id) + "' " + word, nil
}

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
