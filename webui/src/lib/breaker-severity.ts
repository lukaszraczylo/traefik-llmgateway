const SEVERITY: Record<string, number> = { open: 3, 'half-open': 2, degraded: 1, closed: 0 }

/** Sort rank of a breaker status, most severe highest; unknown states rank below closed. */
export function statusRank(status: string): number {
  return SEVERITY[status] ?? -1
}
