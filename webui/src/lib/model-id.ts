export interface SplitModelId {
  provider: string | null
  model: string
}

/** Splits `provider/model` at the first slash; later slashes stay in the model part. A bare id has no provider. */
export function splitModelId(id: string): SplitModelId {
  const slash = id.indexOf('/')
  if (slash <= 0 || slash === id.length - 1) return { provider: null, model: id }
  return { provider: id.slice(0, slash), model: id.slice(slash + 1) }
}
