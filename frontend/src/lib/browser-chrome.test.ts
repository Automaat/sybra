import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'
import { JSDOM } from 'jsdom'
import { describe, expect, it, vi } from 'vitest'

const browserChromePath = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', 'browser_chrome.js')
const browserChromeSource = readFileSync(browserChromePath, 'utf8')

function runBrowserChrome(url: string, source = browserChromeSource) {
  const dom = new JSDOM('<!doctype html><html><body></body></html>', {
    runScripts: 'dangerously',
    url,
  })
  dom.window.eval(source)
  return dom.window
}

describe('browser chrome url normalization', () => {
  it('accepts bare host:port inputs and bare IPv6 loopback', () => {
    const instrumented = browserChromeSource.replace(
      'function normalizeURL(raw) {',
      'window.__normalizeURL = function normalizeURL(raw) {',
    )
    const window = runBrowserChrome('https://github.com/Automaat/sybra', instrumented) as unknown as Window & {
      __normalizeURL: (value: string) => string | null
    }

    expect(window.__normalizeURL('localhost:8080')).toBe('https://localhost:8080/')
    expect(window.__normalizeURL('example.com:8080')).toBe('https://example.com:8080/')
    expect(window.__normalizeURL('::1')).toBe('https://[::1]/')
    expect(window.__normalizeURL('javascript:alert(1)')).toBeNull()
    expect(window.__normalizeURL('#fragment')).toBeNull()
  })
})

describe('browser chrome page scoping', () => {
  it('reserves space and patches SPA history on GitHub pages', () => {
    const window = runBrowserChrome('https://github.com/Automaat/sybra/issues')

    expect(window.document.documentElement.style.getPropertyValue('padding-top')).toBe('40px')
    expect((window.history as History & { __sybraPatched?: boolean }).__sybraPatched).toBe(true)
  })

  it('keeps third-party pages unpatched', () => {
    const window = runBrowserChrome('https://example.com/docs')

    expect(window.document.documentElement.style.getPropertyValue('padding-top')).toBe('')
    expect((window.history as History & { __sybraPatched?: boolean }).__sybraPatched).toBeUndefined()
  })
})

describe('browser chrome failures', () => {
  it('logs a warning when injection fails', () => {
    const dom = new JSDOM('<!doctype html><html><body></body></html>', {
      runScripts: 'dangerously',
      url: 'https://github.com/Automaat/sybra',
    })
    const warn = vi.fn()

    Object.defineProperty(dom.window.console, 'warn', {
      configurable: true,
      value: warn,
    })
    dom.window.document.documentElement.appendChild = () => {
      throw new Error('boom')
    }

    dom.window.eval(browserChromeSource)

    expect(warn).toHaveBeenCalledOnce()
    expect(warn.mock.calls[0]?.[0]).toBe('[Sybra Browser] toolbar injection failed')
  })
})
