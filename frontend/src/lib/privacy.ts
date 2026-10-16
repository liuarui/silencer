import { maskWord } from './sensitive-word.ts'

export const HIDDEN_TEXT = '••••••'

export function displayOriginal(value: string, showPlaintext: boolean): string {
  return showPlaintext ? value : maskWord(value)
}

type Mapping = { original?: string; preview?: string; tok?: string }
type BodyFields = {
  items?: Mapping[]
  raw?: string | null
  masked?: string | null
  upstream_response?: string | null
  restored_response?: string | null
  dialog_req?: string
  dialog?: string
  dialog_resp?: string
  req_preview?: string
  resp_preview?: string
}

/** Replace originals in text as well as nested JSON/tool arguments and SSE.
 * Replacements are simultaneous and longest-first, so overlapping mappings
 * cannot disclose a suffix or reprocess another mapping's masked display.
 */
export function redactOriginals(text: string, items: readonly Mapping[]): string {
  const replacements = new Map<string, string>()
  for (const { original } of items) {
    if (!original) continue
    const masked = maskWord(original)
    replacements.set(original, masked)
    let encoded = original
    for (let depth = 0; depth < 3; depth++) {
      encoded = JSON.stringify(encoded).slice(1, -1)
      replacements.set(encoded, masked)
      replacements.set(encoded.replaceAll('/', '\\/'), masked)
    }
    const unicode = original.split('').map((c) => '\\u' + c.charCodeAt(0).toString(16).padStart(4, '0')).join('')
    replacements.set(unicode, masked)
    replacements.set(unicode.replace(/[a-f]/g, (c) => c.toUpperCase()), masked)
  }
  if (!replacements.size) return text
  const terms = [...replacements.keys()].sort((a, b) => b.length - a.length)
  const pattern = new RegExp(terms.map((term) => term.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|'), 'g')
  const replacePlain = (value: string) => value.replace(pattern, (match) => replacements.get(match)!)
  // Decode actual JSON string literals before matching. JSON permits mixing
  // literal characters with arbitrary Unicode escapes (including nested tool
  // arguments); enumerating whole-string encodings alone cannot cover that.
  const redactJSONStrings = (value: string, depth: number): string => value.replace(/"(?:[^"\\\r\n]|\\[^\r\n])*"/g, (encoded) => {
    try {
      const decoded: string = JSON.parse(encoded)
      const nested = depth < 8 ? redactJSONStrings(decoded, depth + 1) : decoded
      const safe = replacePlain(nested)
      return safe === decoded ? encoded : JSON.stringify(safe)
    } catch {
      return encoded
    }
  })
  return replacePlain(redactJSONStrings(text, 0))
}

export function displayPreview(value: string, showPlaintext: boolean): string {
  // Gateway placeholders are safe to keep recognizable. All other previews
  // are treated as potentially sensitive, including custom words shaped <...>.
  return showPlaintext || /^\{\{[A-Z][A-Z0-9_]*_[a-zA-Z0-9]+\}\}$/.test(value) ? value : maskWord(value)
}

export function projectPrivateBodies<T extends BodyFields>(value: T, showPlaintext: boolean): T {
  if (showPlaintext) return value
  const items = value.items ?? []
  const known = items.length > 0 && items.every((item) => Boolean(item.original))
  const result = { ...value }
  for (const key of ['raw', 'masked', 'upstream_response', 'restored_response', 'dialog_req', 'dialog', 'dialog_resp', 'req_preview', 'resp_preview'] as const) {
    const text = value[key]
    if (text == null) continue
    // Legacy bodies without usable mappings cannot be reliably redacted.
    const untrusted = !known && key !== 'masked' && key !== 'upstream_response' && key !== 'req_preview'
    Object.assign(result, { [key]: untrusted && text ? HIDDEN_TEXT : redactOriginals(text, items) })
  }
  result.items = items.map((item) => ({
    ...item,
    original: item.original == null ? undefined : maskWord(item.original),
    preview: item.preview == null ? undefined : displayPreview(redactOriginals(item.preview, items), false),
  }))
  return result
}
