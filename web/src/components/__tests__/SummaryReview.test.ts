import { cleanup, render } from '@testing-library/svelte'
import Markdown from '@humanspeak/svelte-markdown'
import { afterEach, describe, expect, it, vi } from 'vitest'
import SummaryMarkdown from '../SummaryMarkdown.svelte'
import SessionCard from '../SessionCard.svelte'
import { SummaryStatus, SessionStatus } from '../../lib/types'
import { htmlCases, deniedURLs, autoURLs } from './summaryReviewCases'

const views = [
  ['completed preview', SummaryStatus.Completed, false],
  ['failed preview', SummaryStatus.Failed, false],
  ['opened summary', SummaryStatus.Completed, true],
] as const

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

function summaries(source: string, status: SummaryStatus, expanded: boolean) {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(Response.json([]))),
  )
  const session = {
    id: 'review-synthetic',
    title: 'Synthetic review',
    started_at: '2026-01-01T10:00:00Z',
    ended_at: '2026-01-01T10:20:00Z',
    status: SessionStatus.Ended,
    summary: source,
    summary_status: status,
    summary_preset: 'default',
    audio_path: '',
  }
  const { container } = render(SessionCard, {
    session,
    detail: { session, segments: [] },
    expanded,
    presets: {},
    voiceTasks: undefined,
    onToggle: vi.fn(),
    onLoadDetail: vi.fn(),
    onResummarize: vi.fn(),
    onDelete: vi.fn(),
    onRefreshSession: vi.fn(),
    onSegmentDeleted: vi.fn(),
  })
  const nodes = [...container.querySelectorAll('.summary-preview-md, .summary-markdown')]
  expect(nodes.length).toBe(expanded ? 2 : 1)
  return nodes
}

describe('review controls', () => {
  it('detects main renderer creating a raw HTML element', () => {
    const { container } = render(Markdown, { source: '<img src="data:," alt="control">' })
    expect(container.querySelector('img')).not.toBeNull()
  })
  it('detects main renderer creating a forbidden link', () => {
    const { container } = render(Markdown, { source: '[control](javascript:review_marker)' })
    expect(container.querySelector('a')?.getAttribute('href')).toBe('javascript:review_marker')
  })
  it('detects main renderer creating a forbidden image', () => {
    const { container } = render(Markdown, { source: '![control](data:text/plain,review-marker)' })
    expect(container.querySelector('img')).not.toBeNull()
  })
})

for (const [view, status, expanded] of views) {
  describe(view, () => {
    for (const [name, source] of htmlCases) {
      it(`shows ${name} literally, without creating that element`, () => {
        for (const node of summaries(source, status, expanded)) {
          expect(node.querySelector('img,script,svg,iframe')).toBeNull()
          expect(node.textContent).toContain(source)
        }
      })
    }
    for (const [name, url] of deniedURLs) {
      for (const image of [false, true]) {
        it(`keeps ${image ? 'image' : 'link'} ${name} as text`, () => {
          const source = `${image ? '!' : ''}[review-label](<${url}>)`
          for (const node of summaries(source, status, expanded)) {
            expect(node.querySelector(image ? 'img' : 'a')).toBeNull()
            expect(node.textContent).toContain('review-label')
          }
        })
      }
    }
    for (const url of autoURLs) {
      it(`keeps autolink ${url} as text`, () => {
        for (const node of summaries(`<${url}>`, status, expanded)) {
          expect(node.querySelector('a')).toBeNull()
          expect(node.textContent).toContain(url)
        }
      })
    }
    it('keeps HTML in code spans literal', () => {
      const source = '<img src="data:," alt="code">'
      for (const node of summaries('`' + source + '`', status, expanded)) {
        expect(node.querySelector('img')).toBeNull()
        expect(node.querySelector('code')?.textContent).toBe(source)
      }
    })
    it('keeps allowed links and normal markdown formatting', () => {
      const source =
        '**bold** *italic* [http](http://example.invalid) [https](HTTPS://example.invalid) [mail](mailto:review@example.invalid)'
      for (const node of summaries(source, status, expanded)) {
        expect(node.querySelector('strong')?.textContent).toBe('bold')
        expect(node.querySelector('em')?.textContent).toBe('italic')
        expect([...node.querySelectorAll('a')].map((a) => a.getAttribute('href'))).toEqual([
          'http://example.invalid',
          'HTTPS://example.invalid',
          'mailto:review@example.invalid',
        ])
      }
    })
  })
}

it('rechecks URL policy after a summary changes', async () => {
  const { container, rerender } = render(SummaryMarkdown, {
    source: '[review](https://example.invalid)',
  })
  expect(container.querySelector('a')).not.toBeNull()
  await rerender({ source: '[review](javascript:review_marker)' })
  expect(container.querySelector('a')).toBeNull()
  expect(container.textContent).toContain('review')
})
