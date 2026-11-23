import { ADMIN_UI_REQUEST_HEADER } from '@/api/adminUIRequest'
import type { AccountTestMetadata, AccountTestStreamEvent } from '@/types/accountTestStream'

/**
 * Server-Sent Events consumer for the account connectivity test endpoint.
 *
 * `EventSource` cannot POST, so the modal streams the response body itself.
 * Decoding lives here so both test modals render exactly the same events, and
 * so a final event without a trailing newline is not silently dropped — the
 * streamed text must keep growing while the response arrives.
 */
export interface AccountTestStreamOptions {
  url: string
  body: unknown
  signal: AbortSignal
  /** Caller-specific headers, e.g. the admin UI request marker. */
  headers?: Record<string, string>
  /** Message used when the response carries no readable body. */
  noResponseBodyMessage?: string
  onEvent: (event: AccountTestStreamEvent) => void
}

const SSE_DATA_PREFIX = 'data:'

/** Text fields the test stream reports; blank values never overwrite known ones. */
const METADATA_TEXT_KEYS = [
  'requested_model',
  'upstream_sent_model',
  'upstream_response_model',
  'effective_prompt',
  'effective_effort',
  'stream_mode',
  'model'
] as const

/** Normalizes an optional metadata string; blank and non-strings read as absent. */
export function metadataText(value: unknown): string {
  return typeof value === 'string' ? value.trim() : ''
}

/**
 * Non-blank text kept exactly as reported. Whitespace inside a prompt is part
 * of the request, so it is never trimmed away; a whitespace-only value still
 * reads as absent.
 */
export function metadataVerbatim(value: unknown): string {
  if (typeof value !== 'string') return ''
  return value.trim() ? value : ''
}

/** Folds one event's metadata into the metadata already reported for the run. */
export function mergeAccountTestMetadata(
  current: AccountTestMetadata,
  event: AccountTestMetadata
): AccountTestMetadata {
  const next: AccountTestMetadata = { ...current }

  for (const key of METADATA_TEXT_KEYS) {
    if (key === 'effective_prompt') continue
    const value = metadataText(event[key])
    if (value) next[key] = value
  }
  // The prompt is displayed verbatim, so its own whitespace survives the merge.
  const prompt = metadataVerbatim(event.effective_prompt)
  if (prompt) next.effective_prompt = prompt
  if (typeof event.upstream_response_model_conflict === 'boolean') {
    next.upstream_response_model_conflict = event.upstream_response_model_conflict
  }

  return next
}

/** i18n key for a known response mode; unknown modes stay verbatim. */
export function accountTestStreamModeLabelKey(mode: string): string {
  if (mode === 'stream') return 'admin.accounts.testMetadata.streamMode.stream'
  if (mode === 'native_non_stream') {
    return 'admin.accounts.testMetadata.streamMode.native_non_stream'
  }
  return ''
}

/** A plain text segment or a validated `http(s)://` link for safe template rendering. */
export type TextLinkToken =
  | { kind: 'text'; text: string }
  | { kind: 'link'; text: string; href: string }

const HTTP_URL_PATTERN = /https?:\/\/[^\s<>"'`]+/gi
const TRAILING_LINK_PUNCTUATION = new Set(['.', ',', ';', ':', '!', '?', ']', '}'])

function trimTrailingUrlPunctuation(raw: string): string {
  let end = raw.length
  while (end > 0) {
    const ch = raw[end - 1]
    if (TRAILING_LINK_PUNCTUATION.has(ch)) {
      end -= 1
      continue
    }
    if (ch === ')') {
      const slice = raw.slice(0, end)
      const opens = (slice.match(/\(/g) || []).length
      const closes = (slice.match(/\)/g) || []).length
      if (closes > opens) {
        end -= 1
        continue
      }
    }
    break
  }
  return raw.slice(0, end)
}

function isExactUrlMatch(raw: string, exact: string): boolean {
  if (raw === exact) return true
  if (!raw.startsWith(exact)) return false
  const suffix = raw.slice(exact.length)
  const openParens = (exact.match(/\(/g) || []).length - (exact.match(/\)/g) || []).length
  for (const ch of suffix) {
    if (TRAILING_LINK_PUNCTUATION.has(ch)) continue
    if (ch === ')' && openParens <= 0) continue
    return false
  }
  return true
}

/**
 * Splits plain text into inert text segments and validated `http(s)://` links
 * so Vue templates can render clickable verification URLs without `v-html`.
 *
 * Explicit backend links passed in `exactUrls` are preserved verbatim (never
 * passed through prose trailing-punctuation trimming) so verification tokens
 * ending in punctuation such as `.` keep exact href fidelity.
 */
export function splitTextWithLinks(text: string, exactUrls?: readonly string[]): TextLinkToken[] {
  if (!text) return []

  const normalizedExactUrls = (exactUrls ?? [])
    .map((u) => u.trim())
    .filter((u) => {
      if (!u) return false
      try {
        const parsed = new URL(u)
        return parsed.protocol === 'http:' || parsed.protocol === 'https:'
      } catch {
        return false
      }
    })
    .sort((a, b) => b.length - a.length)

  const tokens: TextLinkToken[] = []
  let lastIndex = 0

  for (const match of text.matchAll(HTTP_URL_PATTERN)) {
    const raw = match[0]
    const index = match.index ?? 0
    if (index > lastIndex) {
      tokens.push({ kind: 'text', text: text.slice(lastIndex, index) })
    }

    const matchedExact = normalizedExactUrls.find((exact) => isExactUrlMatch(raw, exact))
    const candidate = matchedExact ?? trimTrailingUrlPunctuation(raw)
    let safeHref = ''
    try {
      const parsed = new URL(candidate)
      if (parsed.protocol === 'http:' || parsed.protocol === 'https:') {
        safeHref = matchedExact ? candidate : parsed.href
      }
    } catch {
      safeHref = ''
    }

    if (safeHref) {
      tokens.push({ kind: 'link', text: candidate, href: safeHref })
      if (candidate.length < raw.length) {
        tokens.push({ kind: 'text', text: raw.slice(candidate.length) })
      }
    } else {
      tokens.push({ kind: 'text', text: raw })
    }

    lastIndex = index + raw.length
  }

  if (lastIndex < text.length) {
    tokens.push({ kind: 'text', text: text.slice(lastIndex) })
  }

  return tokens
}

/** Decodes one SSE line into an event, or `null` for comments/blank/malformed lines. */
function decodeEventLine(rawLine: string): AccountTestStreamEvent | null {
  const line = rawLine.endsWith('\r') ? rawLine.slice(0, -1) : rawLine
  if (!line.startsWith(SSE_DATA_PREFIX)) return null

  const payload = line.slice(SSE_DATA_PREFIX.length).trim()
  if (!payload) return null

  try {
    const parsed: unknown = JSON.parse(payload)
    if (typeof parsed !== 'object' || parsed === null) return null
    const event = parsed as AccountTestStreamEvent
    return typeof event.type === 'string' && event.type !== '' ? event : null
  } catch (error) {
    console.error('Failed to parse SSE event:', error)
    return null
  }
}

export async function consumeAccountTestStream(options: AccountTestStreamOptions): Promise<void> {
  const response = await fetch(options.url, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${localStorage.getItem('auth_token')}`,
      'Content-Type': 'application/json',
      [ADMIN_UI_REQUEST_HEADER]: '1',
      ...options.headers
    },
    body: JSON.stringify(options.body),
    signal: options.signal
  })

  if (!response.ok) {
    let detail = ''
    try {
      const raw = (await response.text()).trim()
      if (raw) {
        try {
          const parsed: unknown = JSON.parse(raw)
          if (typeof parsed === 'object' && parsed !== null) {
            const record = parsed as Record<string, unknown>
            const nestedError =
              typeof record.error === 'object' && record.error !== null
                ? (record.error as Record<string, unknown>)
                : null
            const candidate =
              (typeof nestedError?.message === 'string' && nestedError.message.trim()) ||
              (typeof record.message === 'string' && record.message.trim()) ||
              (typeof record.error === 'string' && record.error.trim()) ||
              ''
            detail = candidate || raw
          } else {
            detail = raw
          }
        } catch {
          detail = raw
        }
      }
    } catch (error: unknown) {
      if (
        options.signal.aborted ||
        (error instanceof DOMException && error.name === 'AbortError') ||
        (error instanceof Error && error.name === 'AbortError')
      ) {
        throw error
      }
      detail = ''
    }
    throw new Error(detail ? `HTTP error! status: ${response.status}: ${detail}` : `HTTP error! status: ${response.status}`)
  }

  const reader = response.body?.getReader()
  if (!reader) {
    throw new Error(options.noResponseBodyMessage || 'No response body')
  }

  const decoder = new TextDecoder()
  let buffer = ''

  const dispatch = (line: string) => {
    const event = decodeEventLine(line)
    if (event) options.onEvent(event)
  }

  for (;;) {
    const { done, value } = await reader.read()
    if (done) break

    buffer += decoder.decode(value, { stream: true })
    const lines = buffer.split('\n')
    buffer = lines.pop() ?? ''
    for (const line of lines) dispatch(line)
  }

  // A provider may close the stream right after its last event; keep it.
  buffer += decoder.decode()
  if (buffer) dispatch(buffer)
}
