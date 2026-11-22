import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ConfigResponse, ConfigRevision } from '../config-api'
import { configState, resetConfigState, setConfig } from '../config.svelte'
import { unitConfig } from '../../components/__tests__/configFixture'

function at(run: string, count: number, silence = '30s'): ConfigResponse {
  return { ...unitConfig, silence_timeout: silence, revision: { run, count } }
}

// A fake unit: each GET /api/config waits until the test answers it, with the
// state of the run that is live when it is answered.
function fakeReads() {
  const reads: {
    answer: (config: ConfigResponse) => void
    fail: (how: 'network' | 'http500') => void
  }[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(
      () =>
        // The app's TypeScript library, ES2022, has no Promise.withResolvers.
        new Promise<Response>((resolve, reject) =>
          reads.push({
            answer: (config) => resolve(new Response(JSON.stringify(config))),
            fail: (how) =>
              how === 'network'
                ? reject(new TypeError('Failed to fetch'))
                : resolve(new Response(JSON.stringify({ error: 'unit busy' }), { status: 500 })),
          }),
        ),
    ),
  )
  return reads
}

function shown(): ConfigRevision | undefined {
  return configState.config?.revision
}

// Lets a read's answer reach setConfig's follow-up.
async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await Promise.resolve()
  await new Promise((resolve) => setTimeout(resolve, 0))
}

describe('setConfig', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
    resetConfigState()
  })

  it('orders answers from one run by their count, and reads nothing', async () => {
    const reads = fakeReads()
    setConfig(at('run-A', 9, '45s'))
    setConfig(at('run-A', 7, '47s'))
    expect(configState.config?.silence_timeout).toBe('45s')
    setConfig(at('run-A', 10, '50s'))
    expect(configState.config?.silence_timeout).toBe('50s')
    await settle()
    expect(reads).toHaveLength(0)
  })

  it('takes an answer that has no revision, from an older unit', () => {
    setConfig(at('run-A', 9, '45s'))
    setConfig({ ...unitConfig, silence_timeout: '50s' })
    expect(configState.config?.silence_timeout).toBe('50s')
  })

  // A restart under an open page: run B's first answer has a lower count than
  // the page's run A. The page reads the config, and B answers it.
  it('takes a new run, as after a restart, from the read it asks for', async () => {
    const reads = fakeReads()
    setConfig(at('run-A', 9, '45s'))
    setConfig(at('run-B', 1, '50s'))
    expect(shown()).toEqual({ run: 'run-A', count: 9 })

    reads[0].answer(at('run-B', 1, '50s'))
    await settle()
    expect(shown()).toEqual({ run: 'run-B', count: 1 })
    expect(configState.config?.silence_timeout).toBe('50s')
  })

  // Sol's case: A -> B -> a late answer from A. B answers the read.
  it('keeps the live run when a late answer from an earlier run lands', async () => {
    const reads = fakeReads()
    setConfig(at('run-A', 9))
    setConfig(at('run-B', 1, '50s'))
    reads[0].answer(at('run-B', 1, '50s'))
    await settle()

    setConfig(at('run-A', 10, '45s'))
    await settle()
    expect(reads).toHaveLength(2)
    reads[1].answer(at('run-B', 1, '50s'))
    await settle()
    expect(shown()).toEqual({ run: 'run-B', count: 1 })
    expect(configState.config?.silence_timeout).toBe('50s')
  })

  // Two restarts with an answer on its way: the page shows A, never sees B,
  // takes C, then B's late answer lands. C answers the read, and the page
  // still takes C's later answers.
  it('keeps following the live run when a late answer from a run it never saw lands', async () => {
    const reads = fakeReads()
    setConfig(at('run-A', 9))
    setConfig(at('run-C', 2, '50s'))
    reads[0].answer(at('run-C', 2, '50s'))
    await settle()

    setConfig(at('run-B', 1, '45s'))
    await settle()
    reads[1].answer(at('run-C', 2, '50s'))
    await settle()
    expect(shown()).toEqual({ run: 'run-C', count: 2 })

    setConfig(at('run-C', 3, '55s'))
    expect(shown()).toEqual({ run: 'run-C', count: 3 })
    expect(configState.config?.silence_timeout).toBe('55s')
  })

  // A read answered by a run that dies right after: B answers, then C starts.
  // C's next answer is from another run again, so the page reads again, and
  // converges on C.
  it('converges when the run that answered a read is replaced', async () => {
    const reads = fakeReads()
    setConfig(at('run-A', 9))
    setConfig(at('run-B', 1))
    reads[0].answer(at('run-B', 1))
    await settle()
    expect(shown()).toEqual({ run: 'run-B', count: 1 })

    setConfig(at('run-C', 1, '55s'))
    await settle()
    expect(reads).toHaveLength(2)
    reads[1].answer(at('run-C', 1, '55s'))
    await settle()
    expect(shown()).toEqual({ run: 'run-C', count: 1 })
    expect(configState.config?.silence_timeout).toBe('55s')
  })

  // While a read is on its way, answers from other runs ask for one more read
  // between them, not one each; that read starts only once the first lands,
  // and covers a restart after the first read was answered.
  it('has one read on its way at a time, and one more at most after it', async () => {
    const reads = fakeReads()
    setConfig(at('run-A', 9))
    setConfig(at('run-B', 1))
    setConfig(at('run-A', 10))
    setConfig(at('run-C', 1))
    setConfig(at('run-B', 2))
    await settle()
    expect(reads).toHaveLength(1)

    reads[0].answer(at('run-B', 2))
    await settle()
    expect(shown()).toEqual({ run: 'run-B', count: 2 })
    expect(reads).toHaveLength(2)

    reads[1].answer(at('run-C', 1, '55s'))
    await settle()
    expect(shown()).toEqual({ run: 'run-C', count: 1 })
    expect(reads).toHaveLength(2)
  })

  // Late answers from an old run keep landing, one after another: each asks
  // for at most one read, and a read's answer starts no read.
  it('reads no more than once per late answer from an old run', async () => {
    const reads = fakeReads()
    setConfig(at('run-A', 9))
    setConfig(at('run-B', 1))
    reads[0].answer(at('run-B', 1))
    await settle()

    for (let count = 10; count < 13; count++) {
      setConfig(at('run-A', count, '45s'))
      await settle()
      reads.at(-1)?.answer(at('run-B', 1))
      await settle()
    }
    expect(reads).toHaveLength(4)
    await settle()
    expect(reads).toHaveLength(4)
    expect(shown()).toEqual({ run: 'run-B', count: 1 })
  })

  // A read that fails, on the network or with a 500, is tried again: 1 s
  // later, then after twice as long each time. While it waits, an answer from
  // another run asks for no read of its own: the next try covers it. Until a
  // read lands the page says the settings may be out of date.
  it.each(['network', 'http500'] as const)(
    'tries a read that fails (%s) again, waiting twice as long each time, until one lands',
    async (how) => {
      vi.useFakeTimers()
      const reads = fakeReads()
      setConfig(at('run-A', 9, '45s'))
      expect(configState.reloading).toBe(false)
      setConfig(at('run-B', 1, '50s'))
      expect(configState.reloading).toBe(true)
      reads[0].fail(how)
      await vi.advanceTimersByTimeAsync(0)
      expect(shown()).toEqual({ run: 'run-A', count: 9 })
      expect(reads).toHaveLength(1)

      setConfig(at('run-B', 2, '50s'))
      await vi.advanceTimersByTimeAsync(999)
      expect(reads).toHaveLength(1)
      await vi.advanceTimersByTimeAsync(1)
      expect(reads).toHaveLength(2)

      reads[1].fail(how)
      await vi.advanceTimersByTimeAsync(1999)
      expect(reads).toHaveLength(2)
      await vi.advanceTimersByTimeAsync(1)
      expect(reads).toHaveLength(3)
      expect(configState.reloading).toBe(true)

      reads[2].answer(at('run-B', 2, '50s'))
      await vi.advanceTimersByTimeAsync(0)
      expect(shown()).toEqual({ run: 'run-B', count: 2 })
      expect(configState.reloading).toBe(false)
      await vi.advanceTimersByTimeAsync(60_000)
      expect(reads).toHaveLength(3)
    },
  )

  it('waits no longer than 30 s between tries', async () => {
    vi.useFakeTimers()
    const reads = fakeReads()
    setConfig(at('run-A', 9))
    setConfig(at('run-B', 1))
    const waits: number[] = []
    for (let tries = 1; tries < 8; tries++) {
      reads.at(-1)?.fail('network')
      await vi.advanceTimersByTimeAsync(0)
      let waited = 0
      while (reads.length === tries) {
        await vi.advanceTimersByTimeAsync(1000)
        waited += 1000
      }
      waits.push(waited)
    }
    expect(waits).toEqual([1000, 2000, 4000, 8000, 16_000, 30_000, 30_000])
  })

  it('stops trying a failed read again when the state is reset', async () => {
    vi.useFakeTimers()
    const reads = fakeReads()
    setConfig(at('run-A', 9))
    setConfig(at('run-B', 1))
    reads[0].fail('network')
    await vi.advanceTimersByTimeAsync(0)
    resetConfigState()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(reads).toHaveLength(1)
    expect(configState.reloading).toBe(false)
  })

  it('shows no read that was on its way when the state was reset', async () => {
    const reads = fakeReads()
    setConfig(at('run-A', 9))
    setConfig(at('run-B', 1))
    resetConfigState()
    reads[0].answer(at('run-B', 1))
    await settle()
    expect(configState.config).toBeNull()
  })
})
