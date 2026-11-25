/** thinLabels blanks every label except every Nth (N = ceil(len / max)), always keeping the last, so long axes stay legible. */
export function thinLabels(labels: string[], max = 6): string[] {
  const step = Math.max(1, Math.ceil(labels.length / Math.max(1, max)))
  const last = labels.length - 1
  return labels.map((label, i) => (i % step === 0 || i === last ? label : ''))
}
