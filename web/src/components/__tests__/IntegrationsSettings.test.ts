import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import IntegrationsSettings from '../IntegrationsSettings.svelte'
import SettingsPage from '../SettingsPage.svelte'
import { configState, resetConfigState, setConfig } from '../../lib/config.svelte'
import { deferredFetch, sent, stubFetch, unitConfig as config } from './configFixture'

describe('IntegrationsSettings', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('shows the model providers, not per-vendor key slots', async () => {
    stubFetch()
    render(IntegrationsSettings, { config })

    expect(screen.getByRole('heading', { name: 'Model providers' })).toBeTruthy()
    expect(screen.queryByRole('heading', { name: 'API Keys' })).toBeNull()
    const vertex = await screen.findByRole('listitem', { name: 'vertex' })
    expect(await within(vertex).findByText('Signed in')).toBeTruthy()
  })

  it('says 0 means no limit for each GC limit, and allows it', () => {
    stubFetch()
    render(IntegrationsSettings, { config })

    const maxAge = screen.getByLabelText('Delete sessions older than (days)')
    const budget = screen.getByLabelText('Audio budget (MB)')
    expect(maxAge.getAttribute('min')).toBe('0')
    expect(budget.getAttribute('min')).toBe('0')
    expect(screen.getByText(/^0 = never\./)).toBeTruthy()
    expect(screen.getByText(/^0 = no budget: GC removes no audio to free space\./)).toBeTruthy()
  })

  it('saves 0 as the limit', async () => {
    const fetch = stubFetch()
    const { container } = render(IntegrationsSettings, { config })

    await fireEvent.input(screen.getByLabelText('Delete sessions older than (days)'), {
      target: { value: '0' },
    })
    await fireEvent.input(screen.getByLabelText('Audio budget (MB)'), { target: { value: '0' } })
    const gcSection = container.querySelector('.gc-section') as HTMLElement
    await fireEvent.click(within(gcSection).getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([
        {
          url: '/api/config',
          method: 'PATCH',
          body: { gc: { max_age_days: 0, max_audio_size_mb: 0 } },
        },
      ]),
    )
  })

  it('shows the sync and GC settings the unit holds', () => {
    stubFetch()
    render(IntegrationsSettings, {
      config: {
        ...config,
        gdrive: { ...config.gdrive, folder_id: 'folder-1', sync_enabled: false },
      },
    })

    expect((screen.getByLabelText('Folder ID') as HTMLInputElement).value).toBe('folder-1')
    expect((screen.getByLabelText(/Enable automatic sync/) as HTMLInputElement).checked).toBe(false)
    expect((screen.getByLabelText('Enable garbage collection') as HTMLInputElement).checked).toBe(
      true,
    )
    expect(
      (screen.getByLabelText('Delete sessions older than (days)') as HTMLInputElement).value,
    ).toBe('30')
    expect((screen.getByLabelText('Audio budget (MB)') as HTMLInputElement).value).toBe('1024')
  })

  it('shows a setting the unit changed, and keeps what was typed in another', async () => {
    stubFetch()
    const view = render(IntegrationsSettings, { config })
    await fireEvent.input(screen.getByLabelText('Folder ID'), { target: { value: 'typed' } })

    await view.rerender({ config: { ...config, gc: { ...config.gc, max_age_days: 60 } } })

    expect((screen.getByLabelText('Folder ID') as HTMLInputElement).value).toBe('typed')
    expect(
      (screen.getByLabelText('Delete sessions older than (days)') as HTMLInputElement).value,
    ).toBe('60')
    const note = 'Changed on the unit or in another tab; your edit was replaced.'
    expect(screen.queryByText(note)).toBeNull()

    await view.rerender({ config: { ...config, gdrive: { ...config.gdrive, folder_id: 'other' } } })

    expect((screen.getByLabelText('Folder ID') as HTMLInputElement).value).toBe('other')
    expect(screen.getByLabelText('Folder ID').closest('.field')?.textContent).toContain(note)
  })

  it('sends no folder ID that was not changed', async () => {
    const fetch = stubFetch()
    const { container } = render(IntegrationsSettings, { config })

    const gdrive = container.querySelector('.gdrive-section') as HTMLElement
    await fireEvent.click(within(gdrive).getByRole('button', { name: 'Save' }))

    const shown = await within(gdrive).findByText('No changes to save.')
    expect(shown.classList.contains('error')).toBe(false)
    expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([])
  })

  it('sends no GC limit that was not changed', async () => {
    const fetch = stubFetch()
    const { container } = render(IntegrationsSettings, { config })

    const gcSection = container.querySelector('.gc-section') as HTMLElement
    await fireEvent.click(within(gcSection).getByRole('button', { name: 'Save' }))

    const shown = await within(gcSection).findByText('No changes to save.')
    expect(shown.classList.contains('error')).toBe(false)
    expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([])
  })

  // The unit reads a cleared field's null as no value (a save of it would
  // change nothing while the page said "Saved!"), refuses a negative limit,
  // and refuses a fraction or a number past its integers with a parser error.
  it.each([
    ['Delete sessions older than (days)', ''],
    ['Delete sessions older than (days)', '-5'],
    ['Delete sessions older than (days)', '2.5'],
    ['Delete sessions older than (days)', '1e21'],
    ['Audio budget (MB)', ''],
    ['Audio budget (MB)', '-1'],
    ['Audio budget (MB)', '3.5'],
  ])('saves no %s of %j, and says what it needs', async (label, entered) => {
    const fetch = stubFetch()
    const { container } = render(IntegrationsSettings, { config })

    await fireEvent.input(screen.getByLabelText(label), { target: { value: entered } })
    const gcSection = container.querySelector('.gc-section') as HTMLElement
    await fireEvent.click(within(gcSection).getByRole('button', { name: 'Save' }))

    const shown = await within(gcSection).findByText(
      'Each GC limit needs a whole number, 0 or more; 0 means no limit.',
    )
    expect(shown.classList.contains('error')).toBe(true)
    expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([])
  })

  it('sends no GC limit left empty while GC is off and the limits are hidden', async () => {
    const fetch = stubFetch()
    const { container } = render(IntegrationsSettings, { config })

    await fireEvent.input(screen.getByLabelText('Delete sessions older than (days)'), {
      target: { value: '' },
    })
    await fireEvent.click(screen.getByLabelText('Enable garbage collection'))
    const gcSection = container.querySelector('.gc-section') as HTMLElement
    await fireEvent.click(within(gcSection).getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(
        sent(fetch)
          .filter((r) => r.method === 'PATCH')
          .map((r) => r.body),
      ).toEqual([{ gc: { enabled: false } }]),
    )
  })

  // Text typed while a save is on its way would be replaced by the save's
  // answer, so the fields take none until it lands.
  it('takes no input in a section while its save is on its way', async () => {
    const answer = deferredFetch()
    const { container } = render(IntegrationsSettings, { config })
    const gcSection = container.querySelector('.gc-section') as HTMLElement
    const gcControls = [...gcSection.querySelectorAll<HTMLInputElement>('input')]
    expect(gcControls).toHaveLength(3)

    await fireEvent.input(screen.getByLabelText('Delete sessions older than (days)'), {
      target: { value: '45' },
    })
    await fireEvent.click(within(gcSection).getByRole('button', { name: 'Save' }))

    expect(gcControls.map((input) => input.disabled)).toEqual([true, true, true])
    answer(new Response(JSON.stringify({ ...config, gc: { ...config.gc, max_age_days: 45 } })))
    await waitFor(() =>
      expect(gcControls.map((input) => input.disabled)).toEqual([false, false, false]),
    )
  })

  it('takes no Drive folder or sync change while its save is on its way', async () => {
    const answer = deferredFetch()
    const { container } = render(IntegrationsSettings, { config })
    const folder = screen.getByLabelText('Folder ID') as HTMLInputElement
    const sync = screen.getByLabelText(/Enable automatic sync/) as HTMLInputElement

    await fireEvent.input(folder, { target: { value: 'folder-2' } })
    const gdrive = container.querySelector('.gdrive-section') as HTMLElement
    await fireEvent.click(within(gdrive).getByRole('button', { name: 'Save' }))
    expect(folder.disabled).toBe(true)

    await fireEvent.click(sync)
    expect(sync.disabled).toBe(true)
    answer(new Response(JSON.stringify(config)))
    await waitFor(() => expect(folder.disabled || sync.disabled).toBe(false))
    expect(within(gdrive).getAllByText('Saved!')).toHaveLength(2)
  })
})

describe('SettingsPage', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetConfigState()
  })

  // The app loaded the config when it started; the unit's GC age changed
  // since, and the settings page loads the config again when it opens. A save
  // of the audio budget alone must not send the old age back.
  it('shows the config it loads, and a save reverts no change made since the app loaded', async () => {
    setConfig(config)
    const fetch = stubFetch({ config: { ...config, gc: { ...config.gc, max_age_days: 60 } } })
    const { container } = render(SettingsPage, { onBack: () => {} })
    await waitFor(() => expect(configState.config?.gc.max_age_days).toBe(60))

    expect(
      (screen.getByLabelText('Delete sessions older than (days)') as HTMLInputElement).value,
    ).toBe('60')
    await fireEvent.input(screen.getByLabelText('Audio budget (MB)'), { target: { value: '2048' } })
    const gcSection = container.querySelector('.gc-section') as HTMLElement
    await fireEvent.click(within(gcSection).getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(sent(fetch).filter((r) => r.method === 'PATCH')).toEqual([
        { url: '/api/config', method: 'PATCH', body: { gc: { max_audio_size_mb: 2048 } } },
      ]),
    )
  })
})
