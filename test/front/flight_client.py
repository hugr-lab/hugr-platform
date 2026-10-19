"""Flight SQL through the front (spec 002): the real ADBC driver, its cookie middleware on, as DBeaver runs it.

Each node's table `t` holds the node's own label, so a read says which node answered: every call of one connection
must reach the node that seated it.
"""
import sys
import adbc_driver_flightsql.dbapi as dbapi
from adbc_driver_flightsql import DatabaseOptions

uri, token = sys.argv[1], sys.argv[2]


def check(what, ok, seen=None):
    if not ok:
        print(f"FAIL: {what}: {seen!r}")
        sys.exit(1)
    print(f"ok - {what}")


def connect():
    conn = dbapi.connect(uri, db_kwargs={DatabaseOptions.AUTHORIZATION_HEADER.value: f"Bearer {token}",
                                         DatabaseOptions.WITH_COOKIE_MIDDLEWARE.value: "true"})
    conn.adbc_connection.set_autocommit(True)
    return conn


# twelve connections: the front seats each by two random choices, so all of them on one node of two would be a
# chance of 1 in 2048 - the check that both were used is what makes the per-connection checks mean something
seen = set()
for c in range(12):
    with connect() as conn, conn.cursor() as cur:
        answers = set()
        for _ in range(8):
            cur.execute("SELECT v FROM t")
            answers.add(cur.fetchone()[0])
        check(f"connection {c}: every call on the node that seated it", len(answers) == 1, answers)
        seen |= answers
        # a statement with a parameter: a prepared statement's calls (CreatePreparedStatement, DoPut, DoGet)
        cur.execute("SELECT v, ? AS p FROM t", (c,))
        row = cur.fetchone()
        check(f"connection {c}: a prepared statement on the same node", row[0] in answers and row[1] == c, row)
        if c >= 2:
            continue
        # a large result streams through: many batches of one DoGet
        cur.execute("SELECT count(*), sum(id) FROM (SELECT id FROM big)")
        n, s = cur.fetchone()
        check(f"connection {c}: an aggregate", n == 2000000 and s == 1999999000000, (n, s))
        cur.execute("SELECT id FROM big")
        table = cur.fetch_arrow_table()
        check(f"connection {c}: 2M rows streamed", table.num_rows == 2000000, table.num_rows)
        # a session's own setting stays on its node (spec 068: the session's connection)
        cur.execute("SET TimeZone = 'Asia/Tokyo'")
        cur.execute("SELECT current_setting('TimeZone')")
        tz = cur.fetchone()[0]
        check(f"connection {c}: the session keeps its setting", tz == "Asia/Tokyo", tz)
check("connections were seated on both nodes", seen == {"node-1", "node-2"}, seen)

# a client that keeps no cookie (ADBC's default): every call is seated afresh, and the DoGet of a statement or the
# calls of a prepared one reach the node that minted the ticket / handle by the label in it (duckdb-acl spec 134's
# addendum)
served = set()
# (autocommit from the start: without a cookie there is no connection-long session to hold a transaction, spec 050)
with dbapi.connect(uri, db_kwargs={DatabaseOptions.AUTHORIZATION_HEADER.value: f"Bearer {token}"},
                   autocommit=True) as conn, conn.cursor() as cur:
    for i in range(16):
        cur.execute("SELECT v FROM t")
        served.add(cur.fetchone()[0])
        cur.execute("SELECT v, ? AS p FROM t", (i,))
        row = cur.fetchone()
        check(f"without cookies, statement {i}: its ticket and its prepared handle reach their node", row[1] == i, row)
check("without cookies, the statements were seated on both nodes", served == {"node-1", "node-2"}, served)
