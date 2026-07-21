/*
 * SQLite, in a worker, holding one in-memory database.
 *
 * A worker rather than the page thread because the whole point of this feature
 * is that someone can type arbitrary SQL at it. `WITH RECURSIVE` four
 * characters wrong is an infinite loop, and on the main thread that is a locked
 * tab with no way back except closing it. Here the worst case is a busy worker
 * the page can terminate, which is what the "Stop" affordance in the viewer
 * does.
 *
 * In memory rather than OPFS, deliberately. Persistent storage in the browser
 * needs SharedArrayBuffer, which needs COOP/COEP response headers, which the
 * gateway would then be setting on every page it serves — including the proxied
 * apps, which are not ours to break. Losing the database on reload is also the
 * honest behaviour: this is a scratch copy of a snapshot, and nothing anyone
 * does to it was ever going to reach the server.
 *
 * Protocol: the page posts `{ id, type, ...payload }`, this replies
 * `{ id, ok: true, result }` or `{ id, ok: false, error }`. Unsolicited
 * `{ type: "progress", stage, detail }` messages (no id) report boot state.
 *
 * Values crossing back to the page are encoded rather than raw — see
 * encodeCell. Rows are arrays with a separate column list, never objects: two
 * columns in one result can share a name (`SELECT a.id, b.id FROM ...`) and an
 * object would silently drop one of them.
 */

// The loader resolves sqlite3.wasm against its own import.meta.url, so this
// path has to keep pointing at the directory the pair actually lives in.
import sqlite3InitModule from './sqlite/sqlite3.mjs';

/** @type {any} */ let sqlite3 = null;
/** @type {any} */ let db = null;

// A ceiling on rows carried back to the page. `SELECT * FROM events` against a
// real database is a plausible first thing to type, and a million rows would
// die in the structured clone rather than in the grid. The viewer says so when
// this trips, which is better than a page that quietly never renders.
const MAX_ROWS = 5000;

// Enough of the file header to answer "is this even a database" before handing
// a .zip to sqlite3_deserialize. Every SQLite file starts with this exact
// 16 bytes including the terminating NUL.
const HEADER = 'SQLite format 3\u0000';

// ------------------------------------------------------------------ boot

function post(msg, transfer) {
	self.postMessage(msg, transfer || []);
}

async function boot() {
	if (sqlite3) return sqlite3;
	post({ type: 'progress', stage: 'fetch', detail: 'Fetching the SQLite runtime' });
	// print/printErr are silenced because the Emscripten loader narrates its
	// own startup to the console, and a console full of "OPFS not available"
	// on a page that deliberately does not use OPFS teaches the wrong thing to
	// whoever opens devtools next.
	sqlite3 = await sqlite3InitModule({ print: () => {}, printErr: () => {} });
	post({ type: 'progress', stage: 'ready', detail: 'SQLite ' + sqlite3.capi.sqlite3_libversion() });
	return sqlite3;
}

function requireDb() {
	if (!db) throw new Error('No database is open yet.');
	return db;
}

function closeDb() {
	if (db) {
		try {
			db.close();
		} catch (e) {
			/* closing a database we are discarding anyway is not worth a failure */
		}
		db = null;
	}
}

// ------------------------------------------------------- value encoding

const HEX = [];
for (let i = 0; i < 256; i++) HEX.push(i.toString(16).padStart(2, '0'));

function toHex(bytes, limit) {
	const n = limit === undefined ? bytes.length : Math.min(limit, bytes.length);
	let out = '';
	for (let i = 0; i < n; i++) out += HEX[bytes[i]];
	return out;
}

/**
 * Make one cell safe to clone and possible to display.
 *
 * Two of SQLite's five types have no good JS equivalent for this trip. BigInt
 * survives a structured clone but stringifies with an `n` suffix and cannot be
 * mixed with numbers, and a BLOB is bytes that mean nothing rendered as text.
 * Both become tagged objects so the grid can style them as what they are
 * instead of guessing from a string.
 */
function encodeCell(v) {
	if (v === null || v === undefined) return null;
	if (typeof v === 'bigint') return { t: 'int', v: v.toString() };
	if (v instanceof Uint8Array) {
		return { t: 'blob', n: v.byteLength, v: toHex(v, 32) };
	}
	return v;
}

/** Double-quote an identifier, doubling any embedded quote. */
function quoteId(name) {
	return '"' + String(name).replaceAll('"', '""') + '"';
}

/** SQL literal for a value read back out of the database. */
function quoteValue(v) {
	if (v === null || v === undefined) return 'NULL';
	if (typeof v === 'bigint') return v.toString();
	if (typeof v === 'number') return Number.isFinite(v) ? String(v) : 'NULL';
	if (v instanceof Uint8Array) return "X'" + toHex(v) + "'";
	return "'" + String(v).replaceAll("'", "''") + "'";
}

// ------------------------------------------------------- query plumbing

/** Rows of one statement as objects. Internal use only — column names are ours. */
function selectObjects(sql) {
	const out = [];
	requireDb().exec({ sql, rowMode: 'object', callback: (row) => void out.push(row) });
	return out;
}

/** Rows of one statement as arrays, with its column names. Internal use only. */
function selectArrays(sql) {
	const rows = [];
	const columns = [];
	requireDb().exec({ sql, rowMode: 'array', columnNames: columns, resultRows: rows });
	return { columns, rows };
}

function selectOne(sql) {
	const v = requireDb().selectValue(sql);
	return typeof v === 'bigint' ? Number(v) : v;
}

/**
 * Run every statement in `sql`, returning the result of the *last* one that
 * produced rows.
 *
 * This walks sqlite3_prepare_v3 by hand rather than using oo1's exec(), which
 * looks like more work than it is. oo1 collects rows from the *first*
 * row-producing statement and ignores the rest, which is backwards for a query
 * box: someone pastes `DELETE FROM ...; SELECT * FROM ...` and wants to see
 * what is left, not what the first half returned. Preparing one statement at a
 * time (rather than compiling them all up front) is also the only order that
 * works, since `CREATE TABLE t(...); INSERT INTO t ...` cannot compile its
 * second statement until the first has run.
 */
function runStatements(sql) {
	const database = requireDb();
	const { capi, wasm } = sqlite3;
	const pDb = database.pointer;
	const started = performance.now();
	const changesBefore = database.changes(true);

	let statements = 0;
	let last = null;
	let truncated = false;

	// The pointer bookkeeping below is lifted from oo1's own exec loop rather
	// than reinvented: it is the shape that is known to be correct for this
	// build's pointer type, which is not necessarily a plain JS number.
	const stack = wasm.scopedAllocPush();
	try {
		let sqlByteLen = wasm.jstrlen(sql);
		const ppStmt = wasm.scopedAlloc(2 * wasm.ptr.size + (sqlByteLen + 1));
		const pzTail = wasm.ptr.add(ppStmt, wasm.ptr.size);
		let pSql = wasm.ptr.add(pzTail, wasm.ptr.size);
		const pSqlEnd = wasm.ptr.add(pSql, sqlByteLen);
		wasm.jstrcpy(sql, wasm.heap8(), pSql, sqlByteLen, false);
		wasm.poke8(wasm.ptr.add(pSql, sqlByteLen), 0);

		while (pSql && wasm.peek8(pSql)) {
			wasm.pokePtr([ppStmt, pzTail], 0);
			const rc = capi.sqlite3_prepare_v3(pDb, pSql, sqlByteLen, 0, ppStmt, pzTail);
			if (rc) throw new Error(capi.sqlite3_errmsg(pDb));
			const pStmt = wasm.peekPtr(ppStmt);
			pSql = wasm.peekPtr(pzTail);
			sqlByteLen = Number(wasm.ptr.add(pSqlEnd, -pSql));
			// Trailing whitespace or a comment compiles to no statement at all.
			if (!pStmt) continue;
			statements++;
			try {
				const nCol = capi.sqlite3_column_count(pStmt);
				if (nCol > 0) {
					const columns = [];
					for (let i = 0; i < nCol; i++) columns.push(capi.sqlite3_column_name(pStmt, i));
					const rows = [];
					let stepRc = capi.sqlite3_step(pStmt);
					while (stepRc === capi.SQLITE_ROW) {
						if (rows.length >= MAX_ROWS) {
							truncated = true;
							break;
						}
						const row = new Array(nCol);
						for (let i = 0; i < nCol; i++) row[i] = encodeCell(capi.sqlite3_column_js(pStmt, i));
						rows.push(row);
						stepRc = capi.sqlite3_step(pStmt);
					}
					if (stepRc !== capi.SQLITE_ROW && stepRc !== capi.SQLITE_DONE) {
						throw new Error(capi.sqlite3_errmsg(pDb));
					}
					last = { columns, rows };
				} else {
					const stepRc = capi.sqlite3_step(pStmt);
					if (stepRc !== capi.SQLITE_DONE && stepRc !== capi.SQLITE_ROW) {
						throw new Error(capi.sqlite3_errmsg(pDb));
					}
				}
			} finally {
				capi.sqlite3_finalize(pStmt);
			}
		}
	} finally {
		wasm.scopedAllocPop(stack);
	}

	return {
		columns: last ? last.columns : [],
		rows: last ? last.rows : [],
		// Total changes across the whole batch, which is what someone running a
		// migration script wants; per-statement counts would need a UI nobody
		// asked for.
		rowsAffected: database.changes(true) - changesBefore,
		elapsedMs: Math.round((performance.now() - started) * 100) / 100,
		statements,
		truncated,
		maxRows: MAX_ROWS,
	};
}

// -------------------------------------------------------------- opening

function looksLikeSqlite(bytes) {
	if (bytes.length < HEADER.length) return false;
	for (let i = 0; i < HEADER.length; i++) {
		if (bytes[i] !== HEADER.charCodeAt(i)) return false;
	}
	return true;
}

function openBlank() {
	closeDb();
	db = new sqlite3.oo1.DB(':memory:');
}

/**
 * Load a .db file image into a fresh in-memory database.
 *
 * The header is checked in JS first because somebody will eventually hand this
 * a .zip, and "file is not a database" from three layers down is a worse answer
 * than one sentence naming the file. sqlite3_deserialize is then still allowed
 * to have its own opinion, and even a clean return code is not trusted — a
 * truncated or corrupt image deserialises fine and only fails on the first
 * page read, so we do one read here rather than letting it surface later as a
 * mysterious failure in the table list.
 */
function openBytes(bytes) {
	if (!looksLikeSqlite(bytes)) {
		throw new Error(
			'That file is not a SQLite database — it does not start with the SQLite file header. ' +
				'Snapshots downloaded from your dashboard are plain .db files; a .zip, .sqlite.gz or ' +
				'text dump has to be unpacked first.'
		);
	}
	const { capi, wasm } = sqlite3;
	closeDb();
	const next = new sqlite3.oo1.DB(':memory:');
	// wasm.alloc is sqlite3_malloc in this build, which is what FREEONCLOSE and
	// RESIZEABLE require: SQLite will realloc and eventually free this pointer
	// itself. The constants are read from capi with the C values as a fallback,
	// so a build that stopped exporting the enum degrades to still working.
	const p = wasm.allocFromTypedArray(bytes);
	const flags =
		(capi.SQLITE_DESERIALIZE_FREEONCLOSE ?? 1) | (capi.SQLITE_DESERIALIZE_RESIZEABLE ?? 2);
	let rc;
	try {
		rc = capi.sqlite3_deserialize(next.pointer, 'main', p, bytes.byteLength, bytes.byteLength, flags);
	} catch (err) {
		// Not freeing p here is intentional. SQLite frees the buffer itself on
		// the error paths that can reach this point, and a double free inside
		// WASM is an unrecoverable crash of the whole worker, where the worst
		// case of leaking it is one file image in a worker we are about to
		// stop using.
		next.close();
		throw err;
	}
	if (rc) {
		next.close();
		throw new Error('SQLite refused that file (code ' + rc + '). It may be truncated or corrupt.');
	}
	try {
		// Forces an actual page read.
		next.exec('SELECT count(*) FROM sqlite_schema;');
	} catch (err) {
		next.close();
		throw new Error(
			'That file has a SQLite header but could not be read: ' +
				(err && err.message ? err.message : String(err)) +
				'. It is probably truncated or encrypted.'
		);
	}
	db = next;
}

// -------------------------------------------------------------- queries

/**
 * Tables and views with their row counts, alphabetically.
 *
 * A view's count means running the view, which can be slow or can fail outright
 * (a view over a table that was dropped still sits in the schema). A count that
 * throws becomes null rather than taking the whole listing down with it — the
 * rail can render "?" and the object is still there to click on.
 */
function listTables() {
	const rows = selectObjects(
		"SELECT name, type FROM sqlite_schema WHERE type IN ('table','view') " +
			"AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY name COLLATE NOCASE;"
	);
	return rows.map((r) => {
		let count = null;
		try {
			count = selectOne('SELECT count(*) FROM ' + quoteId(r.name) + ';');
		} catch (e) {
			/* see above: an unreadable object is still worth listing */
		}
		return { name: String(r.name), type: String(r.type), rows: count };
	});
}

/**
 * One page of one table or view.
 *
 * LIMIT/OFFSET with no ORDER BY is not a stable paging scheme in general, and
 * that is accepted here on purpose: the alternative is inventing an ordering
 * the user did not ask for, which misrepresents what is in the file. Anyone who
 * needs a guaranteed order has a query box directly below this grid.
 */
function tableData(name, limit, offset) {
	const lim = Math.max(1, Math.min(1000, Number(limit) || 50));
	const off = Math.max(0, Number(offset) || 0);
	const cols = selectObjects(
		'SELECT name, type, "notnull", pk FROM pragma_table_info(' + quoteValue(name) + ') ORDER BY cid;'
	);
	if (cols.length === 0) {
		throw new Error('No such table or view: ' + name);
	}
	const total = selectOne('SELECT count(*) FROM ' + quoteId(name) + ';');
	const page = selectArrays(
		'SELECT * FROM ' + quoteId(name) + ' LIMIT ' + lim + ' OFFSET ' + off + ';'
	);
	return {
		name,
		columns: cols.map((c) => ({
			name: String(c.name),
			type: String(c.type || ''),
			pk: Number(c.pk) > 0,
			notnull: Number(c.notnull) > 0,
		})),
		// selectArrays gives back the *result* column names, which for SELECT *
		// match the pragma, but the pragma is the one carrying types and keys.
		rows: page.rows.map((r) => r.map(encodeCell)),
		total: total === null ? page.rows.length : total,
		limit: lim,
		offset: off,
	};
}

/** Every CREATE statement in the file, in creation order, as one block of text. */
function schema() {
	const rows = selectObjects(
		'SELECT sql FROM sqlite_schema WHERE sql IS NOT NULL ORDER BY rowid;'
	);
	return rows.map((r) => String(r.sql).trim() + ';').join('\n\n');
}

/**
 * Our own `.dump`, because the WASM build has no shell to borrow one from.
 *
 * Tables and their rows first, then indexes, triggers and views, so replaying
 * the file does not fire a trigger against a half-populated table or build an
 * index row by row. Generated columns are left out of the INSERT column list
 * (they cannot be written), and a virtual table's shadow tables are skipped
 * because recreating the virtual table recreates them.
 */
function dumpSQL() {
	const objects = selectObjects(
		"SELECT type, name, sql FROM sqlite_schema WHERE sql IS NOT NULL " +
			"AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY rowid;"
	);
	const tables = objects.filter((o) => o.type === 'table');
	const virtualNames = tables
		.filter((t) => /^\s*CREATE\s+VIRTUAL\s+TABLE/i.test(String(t.sql)))
		.map((t) => String(t.name));
	const isShadow = (name) => virtualNames.some((vt) => name !== vt && name.startsWith(vt + '_'));

	const lines = ['PRAGMA foreign_keys=OFF;', 'BEGIN TRANSACTION;'];

	for (const t of tables) {
		const name = String(t.name);
		if (isShadow(name)) continue;
		lines.push(String(t.sql).trim() + ';');
		// hidden = 0 drops generated columns and a virtual table's hidden ones.
		const cols = selectObjects(
			'SELECT name FROM pragma_table_xinfo(' + quoteValue(name) + ') WHERE hidden = 0 ORDER BY cid;'
		).map((c) => String(c.name));
		if (cols.length === 0) continue;
		const colList = cols.map(quoteId).join(', ');
		const data = selectArrays('SELECT ' + colList + ' FROM ' + quoteId(name) + ';');
		for (const row of data.rows) {
			lines.push(
				'INSERT INTO ' + quoteId(name) + ' (' + colList + ') VALUES (' +
					row.map(quoteValue).join(', ') + ');'
			);
		}
	}

	for (const o of objects) {
		if (o.type === 'table' || isShadow(String(o.name))) continue;
		lines.push(String(o.sql).trim() + ';');
	}

	lines.push('COMMIT;');
	return lines.join('\n');
}

/** The current database as a .db file image. */
function serialize() {
	return sqlite3.capi.sqlite3_js_db_export(requireDb().pointer);
}

function info() {
	const out = {
		sqliteVersion: sqlite3 ? sqlite3.capi.sqlite3_libversion() : null,
		pageSize: null,
		pageCount: null,
		encoding: null,
	};
	if (!db) return out;
	out.pageSize = selectOne('PRAGMA page_size;');
	out.pageCount = selectOne('PRAGMA page_count;');
	out.encoding = selectOne('PRAGMA encoding;');
	return out;
}

// ------------------------------------------------------------ dispatch

self.onmessage = async (event) => {
	const msg = event.data || {};
	try {
		await boot();
		let result;
		switch (msg.type) {
			case 'open':
				openBytes(msg.bytes instanceof Uint8Array ? msg.bytes : new Uint8Array(msg.bytes));
				result = info();
				break;
			case 'openBlank':
				openBlank();
				result = info();
				break;
			case 'exec':
				result = runStatements(String(msg.sql || ''));
				break;
			case 'listTables':
				result = listTables();
				break;
			case 'tableData':
				result = tableData(String(msg.name), msg.limit, msg.offset);
				break;
			case 'schema':
				result = schema();
				break;
			case 'dumpSQL':
				result = dumpSQL();
				break;
			case 'serialize':
				result = serialize();
				break;
			case 'info':
				result = info();
				break;
			default:
				throw new Error('Unknown request type: ' + msg.type);
		}
		// Hand the file image over rather than copying it; a 200 MB database
		// should not exist twice just to cross one postMessage.
		const transfer = result instanceof Uint8Array ? [result.buffer] : [];
		post({ id: msg.id, ok: true, result }, transfer);
	} catch (err) {
		post({ id: msg.id, ok: false, error: err && err.message ? err.message : String(err) });
	}
};
