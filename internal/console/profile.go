package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// spec 010 stage 2: the cluster's profile changed from the console's forms - one hugr-node CLUSTER statement per form,
// built here from the form's fields (names plain or quoted, values literals: nothing a person types is SQL), run once on
// a node under the person's token. The node judges it (the cluster bundle) and answers what it did.

// ProfileChange is `POST cluster/profile`: one statement's fields. Group, when set, scopes it to the nodes of one
// resource group (IN GROUP).
type ProfileChange struct {
	// Action: install, update, remove (an extension), attach, detach (a source), set, reset (a setting), sources,
	// reset-sources (a group's source set, hugr-node spec 035), add-script, alter-script, drop-script (a boot script,
	// hugr-node spec 047 - spec 015).
	Action string `json:"action"`
	// an extension's
	Extension  string `json:"extension,omitempty"`
	Version    string `json:"version,omitempty"`
	Repository string `json:"repository,omitempty"`
	// a source's
	Path      string   `json:"path,omitempty"`
	Alias     string   `json:"alias,omitempty"`
	Type      string   `json:"type,omitempty"`
	Secret    string   `json:"secret,omitempty"`
	ReadOnly  bool     `json:"readOnly,omitempty"`
	Lineage   string   `json:"lineage,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
	Cascade   bool     `json:"cascade,omitempty"`
	Force     bool     `json:"force,omitempty"`
	// Lazy: a source attached on its first read, not at bootstrap; Idle, the interval after which an unused one is
	// detached (duckdb reads it: '30 min'). An attach's marks, or a group's set's (they replace its sources' own).
	Lazy bool   `json:"lazy,omitempty"`
	Idle string `json:"idle,omitempty"`
	// a group's source set: the aliases its nodes attach, or All (every source, which only LAZY makes a set)
	Sources []string `json:"sources,omitempty"`
	All     bool     `json:"all,omitempty"`
	// a setting's
	Setting string `json:"setting,omitempty"`
	Value   string `json:"value,omitempty"`
	// a boot script's: its name (it orders the script among its place's), its place (before_sources, after_source,
	// after_sources), the source an after_source script follows, its SQL - a literal the console never reads - and
	// its policy on error (fail_node, continue, detach_source, retry)
	Script  string `json:"script,omitempty"`
	Place   string `json:"place,omitempty"`
	Source  string `json:"source,omitempty"`
	SQL     string `json:"sql,omitempty"`
	OnError string `json:"onError,omitempty"`
	// the scope, and a note for the item
	Group   string `json:"group,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// ProfileWritten is the node's answer: the profile's version after the write, how the change reaches the nodes (hot,
// drain, restart), whether the answering node applied it, and what it said (duckdb-acl spec 179: ok and message).
type ProfileWritten struct {
	Version     int64  `json:"version"`
	Class       string `json:"class"`
	AppliedHere bool   `json:"appliedHere"`
	OK          bool   `json:"ok"`
	Message     string `json:"message"`
	// Node is the node that wrote it
	Node string `json:"node"`
}

var (
	// a name the CLUSTER grammar takes bare: an extension, a repository, a type, a secret, a setting
	bareName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
	// a script's name: it may start with a digit (10_vars), as hugr-node takes it
	scriptName = regexp.MustCompile(`^[A-Za-z0-9_]{1,63}$`)
)

// The places of a script and their words, the policies on error and theirs (hugr-node spec 047).
var (
	scriptPlaces = map[string]string{"before_sources": "BEFORE SOURCES", "after_source": "AFTER SOURCE",
		"after_sources": "AFTER SOURCES"}
	scriptPolicies = map[string]string{"fail_node": "FAIL NODE", "continue": "CONTINUE",
		"detach_source": "DETACH SOURCE", "retry": "RETRY"}
)

// MaxScriptBytes is the longest script the form writes.
const MaxScriptBytes = 64 << 10

// scriptLiteral is a script's SQL as one string literal: a quote doubled, nothing else changed. It may span lines; a
// control character other than a line break or a tab, text that is not UTF-8, or nothing but spaces is not written.
func scriptLiteral(sql string) (string, bool) {
	if strings.TrimSpace(sql) == "" || len(sql) > MaxScriptBytes || !utf8.ValidString(sql) {
		return "", false
	}
	if strings.ContainsFunc(sql, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) {
		return "", false
	}
	return "'" + strings.ReplaceAll(sql, "'", "''") + "'", true
}

// statement is the CLUSTER statement the change writes, or why it cannot be written.
func (c ProfileChange) statement() (string, error) {
	var b strings.Builder
	name := func(field, value string) error {
		if !bareName.MatchString(value) {
			return fmt.Errorf("%s is a plain name ([A-Za-z_][A-Za-z0-9_]*)", field)
		}
		b.WriteString(value)
		return nil
	}
	text := func(field, value string) error {
		lit, ok := literal(value)
		if !ok {
			return fmt.Errorf("%s is required, one line of at most 1024 characters", field)
		}
		b.WriteString(lit)
		return nil
	}
	// a source's name: quoted, but never with a comma or edge spaces - hugr-node keeps a dependency list joined by
	// commas and trims each name, so such an alias could never be named as one
	alias := func(field, value string) error {
		q, ok := quoted(value)
		if !ok || strings.Contains(value, ",") || strings.TrimSpace(value) != value {
			return fmt.Errorf("%s is required: one line of at most 256 bytes, no comma, no space at its ends", field)
		}
		b.WriteString(q)
		return nil
	}
	var err error
	switch c.Action {
	case "install", "update":
		b.WriteString("CLUSTER " + strings.ToUpper(c.Action) + " EXTENSION ")
		if err = name("the extension", c.Extension); err == nil {
			b.WriteString(" VERSION ")
			err = text("the version", c.Version)
		}
		if err == nil && c.Action == "install" && c.Repository != "" {
			b.WriteString(" FROM ")
			err = name("the repository", c.Repository)
		}
	case "remove":
		b.WriteString("CLUSTER REMOVE EXTENSION ")
		err = name("the extension", c.Extension)
	case "attach":
		b.WriteString("CLUSTER ATTACH ")
		switch {
		case c.Path == "" && c.Secret != "":
			// the secret names the source (a postgres, mysql or mssql source is written so: ATTACH '' … SECRET s)
			b.WriteString("''")
		case c.Path == "":
			err = errors.New("the path is required, unless a secret names the source")
		default:
			err = text("the path", c.Path)
		}
		if err == nil {
			b.WriteString(" AS ")
			err = alias("the alias", c.Alias)
		}
		if err == nil {
			b.WriteString(" (TYPE ")
			err = name("the type", c.Type)
		}
		if err == nil && c.Secret != "" {
			b.WriteString(", SECRET ")
			err = name("the secret", c.Secret)
		}
		if err == nil && c.ReadOnly {
			b.WriteString(", READ_ONLY")
		}
		if err == nil {
			b.WriteString(")")
		}
		if err == nil {
			err = c.lazyMarks(&b)
		}
		if err == nil && c.Lineage != "" {
			b.WriteString(" LINEAGE ")
			err = text("the lineage's name", c.Lineage)
		}
		if err == nil && len(c.DependsOn) > 0 {
			b.WriteString(" DEPENDS ON (")
			for i, a := range c.DependsOn {
				if i > 0 {
					b.WriteString(", ")
				}
				if err = alias("a dependency", a); err != nil {
					break
				}
			}
			b.WriteString(")")
		}
	case "detach":
		b.WriteString("CLUSTER DETACH ")
		err = alias("the alias", c.Alias)
		if err == nil && c.Cascade {
			b.WriteString(" CASCADE")
		}
		if err == nil && c.Force {
			b.WriteString(" FORCE")
		}
	case "set":
		b.WriteString("CLUSTER SET ")
		if err = name("the setting", c.Setting); err == nil {
			// the node keeps a setting's value as text whatever it is written as: a literal always
			b.WriteString(" = ")
			err = text("the value", c.Value)
		}
	case "reset":
		b.WriteString("CLUSTER RESET ")
		err = name("the setting", c.Setting)
	case "sources":
		// SET SOURCES (a, b) | ALL [LAZY [IDLE '…']] IN GROUP g: a group's, never the cluster's
		b.WriteString("CLUSTER SET SOURCES ")
		switch {
		case c.Group == "":
			err = errors.New("a source set is a group's: name the group")
		case c.All && len(c.Sources) > 0:
			err = errors.New("a set is either the sources it lists or all")
		case c.All && !c.Lazy:
			err = errors.New("every source attached at once is no set: reset the group's set instead")
		case c.All:
			b.WriteString("ALL")
		case len(c.Sources) == 0:
			err = errors.New("name the sources the group's nodes attach")
		default:
			b.WriteString("(")
			for i, a := range c.Sources {
				if i > 0 {
					b.WriteString(", ")
				}
				if err = alias("a source", a); err != nil {
					break
				}
			}
			b.WriteString(")")
		}
		if err == nil {
			err = c.lazyMarks(&b)
		}
	case "reset-sources":
		b.WriteString("CLUSTER RESET SOURCES")
		if c.Group == "" {
			err = errors.New("a source set is a group's: name the group")
		}
	case "add-script", "alter-script", "drop-script":
		err = c.script(&b, alias)
	default:
		return "", errors.New("the action is one of install, update, remove, attach, detach, set, reset, sources, " +
			"reset-sources, add-script, alter-script, drop-script")
	}
	if err != nil {
		return "", err
	}
	if c.Group != "" {
		// a resource group's name as the first policy takes it, quoted (the grammar reads a quoted name as one)
		if !nodeproto.ValidPlainName(c.Group) {
			return "", errors.New("the group is a resource group's name ([A-Za-z0-9_-], at most 63)")
		}
		q, _ := quoted(c.Group)
		b.WriteString(" IN GROUP " + q)
	}
	// a comment is an install's, an attach's or a script's (the grammar's)
	if c.Comment != "" && (c.Action == "install" || c.Action == "attach" || c.Action == "add-script" ||
		c.Action == "alter-script") {
		b.WriteString(" COMMENT ")
		if err := text("the comment", c.Comment); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

// script writes `CLUSTER ADD | ALTER | DROP SCRIPT <name> [<place>] [AS '<sql>'] [ON ERROR <policy>]`: an add takes its
// place and its SQL, an alter what it changes (the node keeps the rest), a drop the name alone. Which policy a place
// takes is the node's to judge.
func (c ProfileChange) script(b *strings.Builder, alias func(field, value string) error) error {
	verb := map[string]string{"add-script": "ADD", "alter-script": "ALTER", "drop-script": "DROP"}[c.Action]
	b.WriteString("CLUSTER " + verb + " SCRIPT ")
	if !scriptName.MatchString(c.Script) {
		return errors.New("the script's name is letters, digits and _, at most 63 - it orders the script among its place's")
	}
	b.WriteString(`"` + c.Script + `"`)
	if verb == "DROP" {
		if c.Place != "" || c.Source != "" || c.SQL != "" || c.OnError != "" {
			return errors.New("a script is dropped by its name alone")
		}
		return nil
	}
	if c.Place == "" && verb == "ADD" {
		return errors.New("a script's place is before_sources, after_source or after_sources")
	}
	if c.Place != "" {
		word, ok := scriptPlaces[c.Place]
		if !ok {
			return errors.New("a script's place is before_sources, after_source or after_sources")
		}
		b.WriteString(" " + word)
		if c.Place == "after_source" {
			b.WriteString(" ")
			if err := alias("the source it follows", c.Source); err != nil {
				return err
			}
		} else if c.Source != "" {
			return errors.New("a source is an after_source script's")
		}
	} else if c.Source != "" {
		return errors.New("a source is an after_source script's: give the place too")
	}
	if c.SQL == "" && verb == "ADD" {
		return errors.New("a script's SQL is required")
	}
	if c.SQL != "" {
		lit, ok := scriptLiteral(c.SQL)
		if !ok {
			return errors.New("a script's SQL is at most 64 KiB of text, with no control character but a line break " +
				"and a tab")
		}
		b.WriteString(" AS " + lit)
	}
	if c.OnError != "" {
		word, ok := scriptPolicies[c.OnError]
		if !ok {
			return errors.New("a script's policy on error is fail_node, continue, detach_source or retry")
		}
		b.WriteString(" ON ERROR " + word)
	}
	if verb == "ALTER" && c.Place == "" && c.SQL == "" && c.OnError == "" && c.Comment == "" {
		return errors.New("an alter changes the place, the SQL, the policy or the comment: give one")
	}
	return nil
}

// lazyMarks are `LAZY [IDLE '<interval>']`, when the change is lazy; an idle time alone is refused (the grammar's: a
// source attached at bootstrap is never detached for idleness).
func (c ProfileChange) lazyMarks(b *strings.Builder) error {
	if !c.Lazy {
		if c.Idle != "" {
			return errors.New("an idle time is a lazy source's: mark it lazy")
		}
		return nil
	}
	b.WriteString(" LAZY")
	if c.Idle != "" {
		lit, ok := literal(c.Idle)
		if !ok {
			return errors.New("the idle time is one line of at most 1024 characters ('30 min')")
		}
		b.WriteString(" IDLE " + lit)
	}
	return nil
}

// changeProfile writes one CLUSTER statement on a node that seats the person (a read picks it; the write goes there,
// once - never retried on another node, which a timed-out write may have reached) and answers what the node did.
func (s *Server) changeProfile(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	var in ProfileChange
	// a script's SQL is up to 64 KiB, which JSON may write longer (a quote, a backslash, a line break escaped)
	if err := decodeStrictUpTo(r, &in, 3*MaxScriptBytes); err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "send one change of the profile: {\"action\": ..., ...}")
		return
	}
	statement, err := in.statement()
	if err != nil {
		problem(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	s.writeProfile(w, r.Context(), token, statement)
}

// writeProfile runs one CLUSTER statement on a node that writes (write), once, and answers what the node did.
func (s *Server) writeProfile(w http.ResponseWriter, ctx context.Context, token, statement string) {
	results, node, err := s.write(ctx, token, statement)
	if err != nil {
		policyProblem(w, err)
		return
	}
	// the CLUSTER answer: (ok, message) and its own fields after them as columns (duckdb-acl spec 179)
	said, err := AnswerOf(results[0])
	if err != nil {
		problem(w, http.StatusBadGateway, "bad_answer", err.Error())
		return
	}
	out := ProfileWritten{Node: node.Label, OK: said.OK, Message: said.Message}
	out.Class, _ = said.Fields["class"].(string)
	out.AppliedHere, _ = said.Fields["applied_here"].(bool)
	// a column keeps its type (Arrow's GetOneForMarshal): the BIGINT is an int64
	switch v := said.Fields["version"].(type) {
	case int64:
		out.Version = v
	case float64:
		out.Version = int64(v)
	}
	answer(w, out)
}
