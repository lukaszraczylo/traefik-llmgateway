import { describe, expect, it } from 'vitest'

import { toCsv } from './csv'

describe('toCsv', () => {
  it('joins a header row and data rows with CRLF, including a trailing CRLF', () => {
    expect(toCsv(['a', 'b'], [['1', '2']])).toBe('a,b\r\n1,2\r\n')
  })

  it('renders multiple data rows in order', () => {
    expect(toCsv(['id'], [['x'], ['y'], ['z']])).toBe('id\r\nx\r\ny\r\nz\r\n')
  })

  it('leaves a plain field bare (no quotes) when it has no special characters', () => {
    expect(toCsv(['name'], [['alice']])).toBe('name\r\nalice\r\n')
  })

  it('quotes a field containing a comma', () => {
    expect(toCsv(['name'], [['alice, bob']])).toBe('name\r\n"alice, bob"\r\n')
  })

  it('quotes a field containing a double quote and doubles the embedded quote', () => {
    expect(toCsv(['name'], [['say "hi"']])).toBe('name\r\n"say ""hi"""\r\n')
  })

  it('quotes a field containing an embedded newline', () => {
    expect(toCsv(['note'], [['line1\nline2']])).toBe('note\r\n"line1\nline2"\r\n')
  })

  it('quotes a field containing an embedded carriage return', () => {
    expect(toCsv(['note'], [['line1\rline2']])).toBe('note\r\n"line1\rline2"\r\n')
  })

  it('stringifies numbers and booleans without caller-side conversion', () => {
    expect(toCsv(['n', 'b'], [[42, true]])).toBe('n,b\r\n42,true\r\n')
  })

  it('renders null/undefined fields as empty, never the literal text "null"/"undefined"', () => {
    expect(toCsv(['a', 'b'], [[null, undefined]])).toBe('a,b\r\n,\r\n')
  })

  it('quotes a header cell too, not just data cells', () => {
    expect(toCsv(['a, b'], [['x']])).toBe('"a, b"\r\nx\r\n')
  })

  it('renders just the header row (CRLF-terminated) when there are no data rows', () => {
    expect(toCsv(['only'], [])).toBe('only\r\n')
  })
})

// P4 (security): OWASP CSV Injection — a cell whose text begins with a
// formula-trigger character must be neutralized with a leading single
// quote before any other quoting, so a malicious/upstream-controlled id
// never executes as a formula when the export is opened in a spreadsheet.
describe('toCsv: formula injection (P4)', () => {
  it.each([
    ['=cmd', `id\r\n'=cmd\r\n`],
    ['+1+1', `id\r\n'+1+1\r\n`],
    ['-1+1', `id\r\n'-1+1\r\n`],
    ['@SUM(A1:A9)', `id\r\n'@SUM(A1:A9)\r\n`],
    ['\tcmd', `id\r\n'\tcmd\r\n`],
    // A leading \r is ALSO an RFC 4180 special character, so this one gets
    // both defenses: prefixed, then quoted (no embedded quote to double).
    ['\rcmd', `id\r\n"'\rcmd"\r\n`],
  ])('prefixes a cell starting with a formula-trigger character: %j', (raw, expected) => {
    expect(toCsv(['id'], [[raw]])).toBe(expected)
  })

  it('leaves an ordinary id (no leading formula-trigger character) unprefixed', () => {
    const csv = toCsv(['id'], [['openai/gpt-4']])
    expect(csv).toBe('id\r\nopenai/gpt-4\r\n')
  })

  it('a formula-triggering value that ALSO needs comma/quote-quoting gets both defenses (prefixed, then quoted)', () => {
    const csv = toCsv(['id'], [['=A1, "x"']])
    expect(csv).toBe(`id\r\n"'=A1, ""x"""\r\n`)
  })

  it('does not treat a bare "=" alone as anything other than a formula trigger (still prefixed)', () => {
    expect(toCsv(['v'], [['=']])).toBe("v\r\n'=\r\n")
  })

  it('a value merely CONTAINING (not starting with) a trigger character is left unprefixed', () => {
    expect(toCsv(['note'], [['cost=5']])).toBe('note\r\ncost=5\r\n')
  })
})
