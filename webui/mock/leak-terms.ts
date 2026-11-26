export type LeakTerm = string | RegExp

// Site-specific names come from LEAK_SCAN_EXTRA_TERMS so this list never publishes them.
const LITERALS: string[] = ['raczylo', 'aliyuncs', 'cluster.local']

const PATTERNS: RegExp[] = [
  /\bsk-[A-Za-z0-9]/,
  /\b10\.\d+\.\d+\.\d+\b/,
  /\b172\.(1[6-9]|2\d|3[01])\./,
  /\b192\.168\./,
  /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/,
]

export function leakTerms(extra?: string): LeakTerm[] {
  const names = (extra ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  return [...LITERALS, ...PATTERNS, ...names]
}
