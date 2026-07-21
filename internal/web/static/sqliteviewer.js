/*
 * The snapshot viewer page.
 *
 * Owns the worker, the DOM and every piece of state. No framework, because
 * this is one page in a server-rendered app and pulling in a runtime to
 * re-render a table would be a strange thing for the gateway to do — see the
 * package comment in internal/web.
 *
 * The whole design rests on one fact the page has to keep saying out loud: the
 * database here is a *copy*, sitting in this tab's memory. Nothing typed into
 * the query box travels anywhere, and every edit is gone on reload. That is not
 * a caveat, it is the feature — someone asked for a snapshot precisely so that
 * a mistyped DELETE could not reach the running app.
 *
 * The static shell in templates/sqlite.html is the source of truth for the
 * markup; this file finds elements by id and fills them in. Anything it cannot
 * find is skipped rather than thrown on, so a template edit degrades to a
 * missing feature instead of a blank page.
 */

const $ = (id) => document.getElementById(id);

const el = {
	boot: $('sv-boot'),
	bootText: $('sv-boot-text'),
	bar: $('sv-bar'),
	barFill: $('sv-bar-fill'),
	error: $('sv-error'),
	appForm: $('sv-app-form'),
	appSelect: $('sv-app-select'),
	file: $('sv-file'),
	blank: $('sv-blank'),
	drop: $('sv-drop'),
	empty: $('sv-empty'),
	workspace: $('sv-workspace'),
	sourceLabel: $('sv-source-label'),
	railList: $('sv-rail-list'),
	railEmpty: $('sv-rail-empty'),
	tableName: $('sv-table-name'),
	tableGrid: $('sv-table-grid'),
	tableRange: $('sv-table-range'),
	prev: $('sv-prev'),
	next: $('sv-next'),
	browserEmpty: $('sv-browser-empty'),
	sql: $('sv-sql'),
	run: $('sv-run'),
	stop: $('sv-stop'),
	queryError: $('sv-query-error'),
	resultMeta: $('sv-result-meta'),
	resultGrid: $('sv-result-grid'),
	resultEmpty: $('sv-result-empty'),
	exportDb: $('sv-export-db'),
	exportSql: $('sv-export-sql'),
	info: $('sv-info'),
};

/* Page size comes off the script tag so the number lives in the markup next to
   the "showing X–Y of Z" it explains, rather than in two places. */
const PAGE_SIZE = Math.max(1, Number(($('sv-script') || { dataset: {} }).dataset.pageSize) || 50);

const state = {
	/** Human label for whatever is loaded, also the export filename stem. */
	label: '',
	tables: [],
	active: null,
	offset: 0,
	busy: false,
};

// -------------------------------------------------------------- helpers

function show(node, visible) {
	if (node) node.hidden = !visible;
}

function setText(node, text) {
	if (node) node.textContent = text;
}

function bytesLabel(n) {
	if (n === null || n === undefined) return '';
	if (n < 1024) return n + ' B';
	if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' kB';
	return (n / (1024 * 1024)).toFixed(2) + ' MB';
}

function numberLabel(n) {
	return typeof n === 'number' ? n.toLocaleString() : String(n);
}

/** A filename stem that cannot escape the download directory or surprise a shell. */
function safeStem(name) {
	const cleaned = String(name || 'snapshot').replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '');
	return cleaned || 'snapshot';
}

function showError(message) {
	if (!el.error) return;
	el.error.textContent = message;
	show(el.error, true);
	// The picker is at the top of the page and the error belongs to it; if the
	// user has scrolled down to the query box, an error they cannot see is the
	// same as no error at all.
	el.error.scrollIntoView({ block: 'nearest' });
}

function clearError() {
	if (el.error) {
		el.error.textContent = '';
		show(el.error, false);
	}
}

function setBusy(busy, message) {
	state.busy = busy;
	if (el.run) el.run.disabled = busy;
	if (el.stop) show(el.stop, busy);
	if (busy && message) setText(el.bootText, message);
	show(el.boot, busy || !worker.ready);
	if (el.bar) el.bar.classList.toggle('sv-bar--indeterminate', busy && !el.bar.dataset.determinate);
}

function download(filename, blob) {
	const url = URL.createObjectURL(blob);
	const a = document.createElement('a');
	a.href = url;
	a.download = filename;
	document.body.appendChild(a);
	a.click();
	a.remove();
	// Revoking immediately races the download in some browsers; a second is
	// long enough and the object is a few bytes of bookkeeping either way.
	setTimeout(() => URL.revokeObjectURL(url), 1000);
}

// --------------------------------------------------------- worker client

/**
 * Promise-wrapping client for sqliteworker.js.
 *
 * Rebuildable on purpose: terminate() is the only way to stop a runaway query,
 * and after it the page needs a fresh worker rather than a dead one. Everything
 * outstanding is rejected so no caller waits forever.
 */
const worker = {
	w: null,
	nextId: 1,
	pending: new Map(),
	ready: false,

	start() {
		this.w = new Worker(new URL('./sqliteworker.js', import.meta.url), { type: 'module' });
		this.w.onmessage = (event) => {
			const msg = event.data || {};
			if (msg.type === 'progress') {
				onProgress(msg);
				return;
			}
			const p = this.pending.get(msg.id);
			if (!p) return;
			this.pending.delete(msg.id);
			if (msg.ok) p.resolve(msg.result);
			else p.reject(new Error(msg.error));
		};
		// A worker that fails to load at all (blocked module, missing asset) is
		// otherwise a page that spins forever.
		this.w.onerror = () => {
			this.rejectAll(new Error('The SQLite worker could not start. Reload the page; if it keeps happening the browser may be blocking module workers.'));
		};
	},

	send(request, transfer) {
		const id = this.nextId++;
		return new Promise((resolve, reject) => {
			this.pending.set(id, { resolve, reject });
			this.w.postMessage(Object.assign({ id }, request), transfer || []);
		});
	},

	rejectAll(err) {
		for (const p of this.pending.values()) p.reject(err);
		this.pending.clear();
	},

	terminate() {
		if (this.w) this.w.terminate();
		this.rejectAll(new Error('Stopped.'));
		this.ready = false;
	},
};

function onProgress(msg) {
	if (msg.stage === 'ready') {
		worker.ready = true;
		setText(el.info, msg.detail + ' running in this tab');
	}
	setText(el.bootText, msg.stage === 'ready' ? 'SQLite ready' : msg.detail + '…');
}

// ------------------------------------------------------- cell rendering

const TRUNCATE_AT = 200;

/**
 * Render one value into a table cell.
 *
 * NULL has to be unmistakably not the four-character string "NULL", because in
 * a database people actually own, the difference is the difference between a
 * missing value and a bad import. It gets its own class, lowercase text, and a
 * title spelling it out; a text cell containing NULL renders as plain text.
 */
function renderCell(td, value) {
	if (value === null) {
		td.className = 'sv-cell sv-cell--null';
		td.textContent = 'null';
		td.title = 'SQL NULL (no value)';
		return;
	}
	if (value && typeof value === 'object') {
		if (value.t === 'blob') {
			td.className = 'sv-cell sv-cell--blob';
			td.textContent = 'BLOB · ' + bytesLabel(value.n);
			td.title = value.n
				? 'First bytes: ' + value.v + (value.n > 32 ? ' …' : '')
				: 'Empty BLOB';
			return;
		}
		if (value.t === 'int') {
			td.className = 'sv-cell sv-cell--num';
			td.textContent = value.v;
			td.title = 'Integer too large for a JavaScript number: ' + value.v;
			return;
		}
	}
	if (typeof value === 'number') {
		td.className = 'sv-cell sv-cell--num';
		td.textContent = String(value);
		return;
	}
	const text = String(value);
	td.className = 'sv-cell';
	if (text.length > TRUNCATE_AT) {
		td.textContent = text.slice(0, TRUNCATE_AT) + '…';
		// The full value stays reachable without a modal or a detail pane.
		td.title = text;
	} else {
		td.textContent = text;
		if (text.indexOf('\n') >= 0) td.title = text;
	}
}

/**
 * Build a result grid. `columns` may be plain strings or the richer
 * `{name, type, pk, notnull}` the table browser gets from PRAGMA table_info.
 */
function renderGrid(container, columns, rows) {
	if (!container) return;
	container.textContent = '';
	// No columns is not an empty grid, it is no grid: a statement like UPDATE
	// has no result set, and a bare header rule under the card would imply one
	// that came back empty.
	if (columns.length === 0) return;
	const table = document.createElement('table');
	table.className = 'sv-grid';

	const thead = document.createElement('thead');
	const hr = document.createElement('tr');
	for (const col of columns) {
		const th = document.createElement('th');
		const name = typeof col === 'string' ? col : col.name;
		const label = document.createElement('span');
		label.className = 'sv-col';
		label.textContent = name;
		th.appendChild(label);
		if (typeof col === 'object') {
			if (col.pk) {
				const pk = document.createElement('span');
				pk.className = 'sv-pk';
				pk.textContent = 'PK';
				pk.title = 'Primary key';
				th.appendChild(pk);
			}
			if (col.type) {
				const t = document.createElement('span');
				t.className = 'sv-coltype';
				t.textContent = col.type.toLowerCase();
				th.appendChild(t);
			}
			if (col.notnull) th.title = name + ' — declared NOT NULL';
		}
		hr.appendChild(th);
	}
	thead.appendChild(hr);
	table.appendChild(thead);

	const tbody = document.createElement('tbody');
	for (const row of rows) {
		const tr = document.createElement('tr');
		for (let i = 0; i < columns.length; i++) {
			const td = document.createElement('td');
			renderCell(td, row[i] === undefined ? null : row[i]);
			tr.appendChild(td);
		}
		tbody.appendChild(tr);
	}
	table.appendChild(tbody);
	container.appendChild(table);
}

// ----------------------------------------------------------- the rail

function renderRail() {
	if (!el.railList) return;
	el.railList.textContent = '';
	show(el.railEmpty, state.tables.length === 0);
	for (const t of state.tables) {
		const li = document.createElement('li');
		const btn = document.createElement('button');
		btn.type = 'button';
		btn.className = 'sv-rail__btn' + (t.name === state.active ? ' sv-rail__btn--active' : '');
		if (t.name === state.active) btn.setAttribute('aria-current', 'true');

		const name = document.createElement('span');
		name.className = 'sv-rail__name';
		name.textContent = t.name;
		btn.appendChild(name);

		if (t.type === 'view') {
			const tag = document.createElement('span');
			tag.className = 'sv-rail__kind';
			tag.textContent = 'view';
			btn.appendChild(tag);
		}

		const count = document.createElement('span');
		count.className = 'sv-rail__count';
		// A count that could not be taken shows as "?" rather than 0 — claiming
		// a broken view is empty would be a lie about someone's data.
		count.textContent = t.rows === null ? '?' : numberLabel(t.rows);
		if (t.rows === null) count.title = 'Row count unavailable — this object could not be read.';
		btn.appendChild(count);

		btn.addEventListener('click', () => void openTable(t.name, 0));
		li.appendChild(btn);
		el.railList.appendChild(li);
	}
}

// ------------------------------------------------------- table browser

async function openTable(name, offset) {
	clearError();
	state.active = name;
	state.offset = offset;
	renderRail();
	try {
		const data = await worker.send({ type: 'tableData', name, limit: PAGE_SIZE, offset });
		setText(el.tableName, name);
		show(el.browserEmpty, data.rows.length === 0);
		renderGrid(el.tableGrid, data.columns, data.rows);
		const first = data.total === 0 ? 0 : data.offset + 1;
		const lastRow = data.offset + data.rows.length;
		setText(
			el.tableRange,
			data.total === 0
				? 'No rows'
				: 'Showing ' + numberLabel(first) + '–' + numberLabel(lastRow) + ' of ' + numberLabel(data.total)
		);
		if (el.prev) el.prev.disabled = data.offset <= 0;
		if (el.next) el.next.disabled = lastRow >= data.total;
	} catch (err) {
		showError(err.message);
	}
}

// ------------------------------------------------------------ querying

async function runQuery() {
	const sql = el.sql ? el.sql.value.trim() : '';
	if (!sql || state.busy) return;
	show(el.queryError, false);
	setBusy(true, 'Running your query');
	const startedAt = Date.now();
	try {
		const res = await worker.send({ type: 'exec', sql });
		const bits = [];
		if (res.columns.length > 0) {
			bits.push(numberLabel(res.rows.length) + (res.rows.length === 1 ? ' row' : ' rows'));
		}
		if (res.rowsAffected > 0) {
			bits.push(numberLabel(res.rowsAffected) + ' row' + (res.rowsAffected === 1 ? '' : 's') + ' changed');
		}
		bits.push(res.statements + (res.statements === 1 ? ' statement' : ' statements'));
		bits.push(res.elapsedMs + ' ms');
		setText(el.resultMeta, bits.join(' · '));

		if (res.columns.length === 0) {
			renderGrid(el.resultGrid, [], []);
			setText(
				el.resultEmpty,
				res.rowsAffected > 0
					? 'That changed the copy in this tab. Nothing was sent to the server.'
					: 'That statement returned no rows.'
			);
			show(el.resultEmpty, true);
		} else if (res.rows.length === 0) {
			renderGrid(el.resultGrid, res.columns, []);
			setText(el.resultEmpty, 'The query ran and matched nothing.');
			show(el.resultEmpty, true);
		} else {
			show(el.resultEmpty, false);
			renderGrid(el.resultGrid, res.columns, res.rows);
		}
		if (res.truncated) {
			setText(
				el.resultEmpty,
				'Showing the first ' + numberLabel(res.maxRows) + ' rows. Add a LIMIT to see a specific slice.'
			);
			show(el.resultEmpty, true);
		}
		// A statement may have created or dropped something; the rail is cheap
		// to rebuild and a stale one is confusing.
		await refreshTables(state.active);
	} catch (err) {
		if (el.queryError) {
			el.queryError.textContent = err.message;
			show(el.queryError, true);
		}
		setText(el.resultMeta, 'Failed after ' + (Date.now() - startedAt) + ' ms');
	} finally {
		setBusy(false);
	}
}

// --------------------------------------------------------- loading a db

async function refreshTables(preferred) {
	state.tables = await worker.send({ type: 'listTables' });
	renderRail();
	const wanted = state.tables.some((t) => t.name === preferred)
		? preferred
		: state.tables.length > 0
			? state.tables[0].name
			: null;
	if (wanted) {
		await openTable(wanted, wanted === preferred ? state.offset : 0);
	} else {
		state.active = null;
		setText(el.tableName, '');
		setText(el.tableRange, '');
		renderGrid(el.tableGrid, [], []);
		show(el.browserEmpty, true);
	}
}

function describe(info) {
	const parts = [];
	if (info.pageCount && info.pageSize) {
		parts.push(numberLabel(info.pageCount) + ' pages · ' + bytesLabel(info.pageCount * info.pageSize));
	}
	if (info.encoding) parts.push(info.encoding);
	if (info.sqliteVersion) parts.push('SQLite ' + info.sqliteVersion);
	return parts.join(' · ');
}

async function afterOpen(label, info) {
	state.label = label;
	setText(el.sourceLabel, label);
	setText(el.info, describe(info));
	show(el.empty, false);
	show(el.workspace, true);
	if (el.exportDb) el.exportDb.disabled = false;
	if (el.exportSql) el.exportSql.disabled = false;
	await refreshTables(null);
}

async function loadBytes(label, bytes) {
	clearError();
	setBusy(true, 'Opening ' + label);
	try {
		// Transferred, not copied: a 100 MB snapshot has no business existing
		// twice while it crosses into the worker.
		const info = await worker.send({ type: 'open', bytes }, [bytes.buffer]);
		await afterOpen(label, info);
	} catch (err) {
		showError(err.message);
	} finally {
		setBusy(false);
	}
}

/**
 * Pull one app's snapshot off the server.
 *
 * Read with a stream so the progress bar reflects real bytes: these files can
 * be tens of megabytes and a spinner with no end in sight is the state people
 * reload out of. When the server does not send a Content-Length (it does today,
 * but a proxy in front could strip it) the bar falls back to indeterminate
 * rather than inventing a percentage.
 */
async function loadFromServer(title, url) {
	clearError();
	setBusy(true, 'Downloading ' + title);
	try {
		const res = await fetch(url, { credentials: 'same-origin', headers: { Accept: 'application/vnd.sqlite3' } });
		if (!res.ok) {
			// The gateway answers a script-initiated request with a JSON body
			// saying what actually went wrong — most usefully "that instance
			// has no database yet", which happens whenever an app has been
			// added but never opened. Guessing "your session expired" from the
			// status code sent people to re-authenticate over a database that
			// simply did not exist yet, so prefer the server's own words and
			// keep the guess for the case where there are none.
			let detail = '';
			try {
				const body = await res.json();
				if (body && typeof body.error === 'string') detail = body.error;
			} catch (e) {
				/* not JSON: fall through to the generic message */
			}
			if (!detail) {
				detail =
					res.status === 401 || res.status === 403
						? 'Your session may have expired — reload the page and sign in again.'
						: 'The server would not hand over that snapshot (HTTP ' + res.status + ').';
			}
			throw new Error(detail);
		}
		const total = Number(res.headers.get('Content-Length') || 0);
		let bytes;
		if (res.body && total > 0) {
			const reader = res.body.getReader();
			const chunks = [];
			let read = 0;
			for (;;) {
				const step = await reader.read();
				if (step.done) break;
				chunks.push(step.value);
				read += step.value.length;
				setProgress(read / total, 'Downloading ' + title + ' — ' + bytesLabel(read) + ' of ' + bytesLabel(total));
			}
			bytes = new Uint8Array(read);
			let at = 0;
			for (const c of chunks) {
				bytes.set(c, at);
				at += c.length;
			}
		} else {
			bytes = new Uint8Array(await res.arrayBuffer());
		}
		clearProgress();
		setBusy(true, 'Opening ' + title);
		const info = await worker.send({ type: 'open', bytes }, [bytes.buffer]);
		await afterOpen(title + ' snapshot', info);
	} catch (err) {
		showError(err.message);
	} finally {
		clearProgress();
		setBusy(false);
	}
}

function setProgress(fraction, message) {
	if (el.bar) {
		el.bar.dataset.determinate = '1';
		el.bar.classList.remove('sv-bar--indeterminate');
		el.bar.setAttribute('aria-valuenow', String(Math.round(fraction * 100)));
	}
	if (el.barFill) el.barFill.style.width = Math.max(0, Math.min(1, fraction)) * 100 + '%';
	if (message) setText(el.bootText, message);
}

function clearProgress() {
	if (el.bar) {
		delete el.bar.dataset.determinate;
		el.bar.removeAttribute('aria-valuenow');
	}
	if (el.barFill) el.barFill.style.width = '';
}

async function loadFile(file) {
	const bytes = new Uint8Array(await file.arrayBuffer());
	await loadBytes(file.name, bytes);
}

async function startBlank() {
	clearError();
	setBusy(true, 'Creating an empty database');
	try {
		const info = await worker.send({ type: 'openBlank' });
		await afterOpen('Empty database', info);
	} catch (err) {
		showError(err.message);
	} finally {
		setBusy(false);
	}
}

// ------------------------------------------------------------ exporting

async function exportDb() {
	clearError();
	setBusy(true, 'Serialising the database');
	try {
		const bytes = await worker.send({ type: 'serialize' });
		download(safeStem(state.label) + '.db', new Blob([bytes], { type: 'application/vnd.sqlite3' }));
	} catch (err) {
		showError(err.message);
	} finally {
		setBusy(false);
	}
}

async function exportSql() {
	clearError();
	setBusy(true, 'Writing the SQL dump');
	try {
		const sql = await worker.send({ type: 'dumpSQL' });
		download(safeStem(state.label) + '.sql', new Blob([sql], { type: 'application/sql' }));
	} catch (err) {
		showError(err.message);
	} finally {
		setBusy(false);
	}
}

// --------------------------------------------------------------- wiring

function readConfig() {
	const node = $('sv-config');
	if (!node) return { apps: [], preselect: '' };
	try {
		const parsed = JSON.parse(node.textContent) || {};
		return { apps: parsed.apps || [], preselect: parsed.preselect || '' };
	} catch (e) {
		return { apps: [], preselect: '' };
	}
}

function appFor(config, name) {
	return config.apps.find((a) => a.Name === name) || null;
}

function wireDragAndDrop() {
	const zone = el.drop;
	if (!zone) return;
	let depth = 0;
	// dragenter/dragleave fire for every child element the pointer crosses, so
	// a plain toggle flickers; counting the enters is the standard fix.
	zone.addEventListener('dragenter', (ev) => {
		ev.preventDefault();
		depth++;
		zone.classList.add('sv-drop--over');
	});
	zone.addEventListener('dragover', (ev) => {
		ev.preventDefault();
		if (ev.dataTransfer) ev.dataTransfer.dropEffect = 'copy';
	});
	zone.addEventListener('dragleave', () => {
		depth = Math.max(0, depth - 1);
		if (depth === 0) zone.classList.remove('sv-drop--over');
	});
	zone.addEventListener('drop', (ev) => {
		ev.preventDefault();
		depth = 0;
		zone.classList.remove('sv-drop--over');
		const file = ev.dataTransfer && ev.dataTransfer.files && ev.dataTransfer.files[0];
		if (file) void loadFile(file);
	});
}

function init() {
	const config = readConfig();

	worker.start();
	setBusy(false, 'Starting SQLite');
	show(el.boot, true);
	setText(el.bootText, 'Starting SQLite…');

	if (el.appForm) {
		el.appForm.addEventListener('submit', (ev) => {
			ev.preventDefault();
			const app = appFor(config, el.appSelect ? el.appSelect.value : '');
			if (app) void loadFromServer(app.Title, app.URL);
		});
	}
	if (el.file) {
		el.file.addEventListener('change', () => {
			const f = el.file.files && el.file.files[0];
			if (f) void loadFile(f);
			// Reset so re-picking the same file after a failed load still fires.
			el.file.value = '';
		});
	}
	if (el.blank) el.blank.addEventListener('click', () => void startBlank());
	if (el.run) el.run.addEventListener('click', () => void runQuery());
	if (el.stop) {
		el.stop.addEventListener('click', () => {
			// The only reliable way to stop a query mid-flight is to kill the
			// worker, and that takes the in-memory copy with it. Saying so is
			// better than a "cancel" that quietly does nothing.
			worker.terminate();
			worker.start();
			state.tables = [];
			state.active = null;
			renderRail();
			show(el.workspace, false);
			show(el.empty, true);
			setBusy(false);
			showError('Query stopped. The copy in this tab was discarded with it — load the snapshot again to carry on.');
		});
	}
	if (el.prev) el.prev.addEventListener('click', () => {
		if (state.active) void openTable(state.active, Math.max(0, state.offset - PAGE_SIZE));
	});
	if (el.next) el.next.addEventListener('click', () => {
		if (state.active) void openTable(state.active, state.offset + PAGE_SIZE);
	});
	if (el.exportDb) el.exportDb.addEventListener('click', () => void exportDb());
	if (el.exportSql) el.exportSql.addEventListener('click', () => void exportSql());

	if (el.sql) {
		el.sql.addEventListener('keydown', (ev) => {
			// Ctrl/Cmd+Enter, the one shortcut every SQL box has had since psql.
			if ((ev.ctrlKey || ev.metaKey) && ev.key === 'Enter') {
				ev.preventDefault();
				void runQuery();
			}
		});
	}

	wireDragAndDrop();

	// Boot the runtime immediately rather than on first use: it is ~1.4 MB and
	// the wait is much easier to accept while reading the page than after
	// clicking something.
	worker
		.send({ type: 'info' })
		.then(() => {
			show(el.boot, false);
			if (config.preselect) {
				const app = appFor(config, config.preselect);
				if (app) {
					if (el.appSelect) el.appSelect.value = app.Name;
					void loadFromServer(app.Title, app.URL);
				}
			}
		})
		.catch((err) => {
			setText(el.bootText, 'SQLite could not start');
			showError(err.message);
		});
}

init();
