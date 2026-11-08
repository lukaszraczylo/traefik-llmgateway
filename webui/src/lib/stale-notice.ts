/** staleNotice returns the text for a "showing old data" banner, or null. Shown only when data is on screen and the latest refresh failed. */
export function staleNotice(hasData: boolean, error: string, lastUpdated: Date | null): string | null {
  if (!hasData || !error) return null
  const at = lastUpdated ? ` Data from ${lastUpdated.toLocaleTimeString()}.` : ''
  return `Refresh failing: ${error}.${at}`
}
