export const LIMIT: number
export const DIST: string
export function hasDist(): boolean
export function measure(dir?: string): { file: string; gzip: number }[]
export function totalGzip(dir?: string): number
export function readCss(dir?: string): string
