import { describe, expect, it } from 'vitest'
import { parseSiwcCallback, precheckSiwcJson, validateSiwcCallback } from '../siwcCallback'

const STATE = 'state_abc123'
const CLIENT_ID = 'oaiapp_test_client'
const base = `http://127.0.0.1:1455/auth/callback?code=ac_code&state=${STATE}&client_id=${CLIENT_ID}`

describe('parseSiwcCallback', () => {
  it('parses a full callback URL', () => {
    expect(parseSiwcCallback(base)).toEqual({
      code: 'ac_code',
      state: STATE,
      clientId: CLIENT_ID,
      error: '',
      errorDescription: ''
    })
  })

  it('parses a bare query with or without the leading question mark', () => {
    expect(parseSiwcCallback(`?code=x&state=${STATE}&client_id=${CLIENT_ID}`).code).toBe('x')
    expect(parseSiwcCallback(`code=x&state=${STATE}&client_id=${CLIENT_ID}`).clientId).toBe(CLIENT_ID)
  })

  it('returns empty fields for garbage input', () => {
    expect(parseSiwcCallback('   ')).toEqual({ code: '', state: '', clientId: '', error: '', errorDescription: '' })
    expect(parseSiwcCallback('not a url').code).toBe('')
  })

  it('extracts upstream error fields', () => {
    const parsed = parseSiwcCallback(`${base.split('?')[0]}?error=access_denied&error_description=user%20cancelled&state=${STATE}`)
    expect(parsed.error).toBe('access_denied')
    expect(parsed.errorDescription).toBe('user cancelled')
  })
})

describe('validateSiwcCallback', () => {
  it('accepts a well-formed callback matching the session state', () => {
    const result = validateSiwcCallback(base, STATE)
    expect(result.ok).toBe(true)
    expect(result.params?.code).toBe('ac_code')
  })

  it('accepts localhost host and surrounding whitespace', () => {
    const url = `  http://localhost:1455/auth/callback?code=c&state=${STATE}&client_id=${CLIENT_ID}  `
    expect(validateSiwcCallback(url, STATE).ok).toBe(true)
  })

  it('accepts a bare query string', () => {
    expect(validateSiwcCallback(`code=c&state=${STATE}&client_id=${CLIENT_ID}`, STATE).ok).toBe(true)
  })

  it('rejects empty and malformed input', () => {
    expect(validateSiwcCallback('', STATE).reason).toBe('empty')
    expect(validateSiwcCallback('hello world', STATE).reason).toBe('malformed')
  })

  it('rejects wrong host, port, path or hash', () => {
    expect(validateSiwcCallback(`http://127.0.0.1:1456/auth/callback?code=c&state=${STATE}&client_id=${CLIENT_ID}`, STATE).reason).toBe('wrong_redirect')
    expect(validateSiwcCallback(`http://example.com/auth/callback?code=c&state=${STATE}&client_id=${CLIENT_ID}`, STATE).reason).toBe('wrong_redirect')
    expect(validateSiwcCallback(`http://127.0.0.1:1455/callback?code=c&state=${STATE}&client_id=${CLIENT_ID}`, STATE).reason).toBe('wrong_redirect')
    expect(validateSiwcCallback(`${base}#frag`, STATE).reason).toBe('wrong_redirect')
  })

  it('rejects duplicated parameters', () => {
    expect(validateSiwcCallback(`${base}&code=other`, STATE).reason).toBe('duplicate_param')
    expect(validateSiwcCallback(`${base}&state=${STATE}`, STATE).reason).toBe('duplicate_param')
  })

  it('returns the upstream error when error= is present', () => {
    const result = validateSiwcCallback(
      `http://127.0.0.1:1455/auth/callback?error=access_denied&error_description=denied&state=${STATE}`,
      STATE
    )
    expect(result.ok).toBe(false)
    expect(result.reason).toBe('upstream_error')
    expect(result.upstreamError).toBe('access_denied: denied')
  })

  it('requires code and client_id with the oaiapp_ prefix', () => {
    expect(validateSiwcCallback(`http://127.0.0.1:1455/auth/callback?state=${STATE}&client_id=${CLIENT_ID}`, STATE).reason).toBe('missing_code')
    expect(validateSiwcCallback(`http://127.0.0.1:1455/auth/callback?code=c&state=${STATE}`, STATE).reason).toBe('missing_client_id')
    expect(validateSiwcCallback(`http://127.0.0.1:1455/auth/callback?code=c&state=${STATE}&client_id=app_xxx`, STATE).reason).toBe('invalid_client_id')
  })

  it('requires the state to match the session', () => {
    expect(validateSiwcCallback(`http://127.0.0.1:1455/auth/callback?code=c&client_id=${CLIENT_ID}`, STATE).reason).toBe('missing_state')
    expect(validateSiwcCallback(base, 'another').reason).toBe('state_mismatch')
    expect(validateSiwcCallback(base, '').reason).toBe('state_mismatch')
  })
})

describe('precheckSiwcJson', () => {
  const entry = { type: 'chatgpt_siwc', client_id: 'oaiapp_x', refresh_token: 'rt', email: 'a@b.c' }

  it('accepts a single object, an array and newline-delimited objects', () => {
    expect(precheckSiwcJson(JSON.stringify(entry))).toEqual({ ok: true, count: 1 })
    expect(precheckSiwcJson(JSON.stringify([entry, entry]))).toEqual({ ok: true, count: 2 })
    expect(precheckSiwcJson(`${JSON.stringify(entry)}\n${JSON.stringify(entry)}\n`)).toEqual({ ok: true, count: 2 })
  })

  it('recognises SIWC by client_id prefix alone', () => {
    expect(precheckSiwcJson(JSON.stringify({ client_id: 'oaiapp_y', refresh_token: 'rt', email: 'e' })).ok).toBe(true)
  })

  it('rejects empty, invalid JSON, non-SIWC and incomplete entries', () => {
    expect(precheckSiwcJson('').reason).toBe('empty')
    expect(precheckSiwcJson('{not json').reason).toBe('invalid_json')
    expect(precheckSiwcJson(JSON.stringify({ client_id: 'app_codex', refresh_token: 'rt', email: 'e' }))).toMatchObject({ ok: false, reason: 'not_siwc', index: 1 })
    expect(precheckSiwcJson(JSON.stringify([entry, { type: 'chatgpt_siwc', email: 'e' }]))).toMatchObject({ ok: false, reason: 'missing_fields', index: 2 })
  })
})
