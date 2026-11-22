<script lang="ts">
  import type { PresetDetail } from '../lib/config-api'
  import { SessionStatus, type SessionSummary, type Segment } from '../lib/types'
  import {
    ConfigRequestError,
    generatePreset,
    refinePreset,
    patchConfig,
    testPreset,
  } from '../lib/config-api'
  import { fetchDates, fetchSessions, fetchSession } from '../lib/api'
  import { setConfig } from '../lib/config.svelte'

  let {
    presets,
  }: {
    presets: Record<string, PresetDetail>
  } = $props()

  let expandedPreset = $state('')
  let editName = $state('')
  let editDesc = $state('')
  let editPrompt = $state('')
  let editTemplate = $state('')
  let editModel = $state('')
  let isNew = $state(false)
  let newStep = $state<'describe' | 'form'>('describe')
  let generating = $state(false)
  let saving = $state(false)
  let feedback = $state('')

  // Test modal state
  let testingPreset = $state('')
  let testSessionId = $state('')
  let testRunning = $state(false)
  let testResult = $state('')
  let testError = $state('')
  let testSessions = $state<SessionSummary[]>([])
  let testSessionsLoading = $state(false)
  let selectedSessionDetail = $state<{ summary: string; transcript: string } | null>(null)
  let selectedDetailLoading = $state(false)

  // Refine state
  let refiningPreset = $state('')
  let refineFeedback = $state('')
  let refineRunning = $state(false)

  // base is the open preset as the unit last held it, which its fields
  // started from. A save sends only the fields that differ from it: the unit
  // replaces each field sent, so a field sent unchanged would undo a change
  // made to it elsewhere since.
  let base: PresetDetail | undefined

  // A field the unit changes while its preset is open takes the unit's value;
  // the other fields keep what was typed.
  $effect(() => {
    const preset = presets[expandedPreset]
    if (isNew || !preset || !base) return
    if (preset.description !== base.description) editDesc = preset.description
    if (preset.system_prompt !== base.system_prompt) editPrompt = preset.system_prompt
    if (preset.user_template !== base.user_template) editTemplate = preset.user_template
    if (preset.model !== base.model) editModel = preset.model
    base = { ...preset }
  })

  // Whether the press that a click on a dialog's overlay ends began on the
  // overlay too: a press inside the dialog, such as one that selects text,
  // can end on the overlay, and must not close the dialog.
  let overlayPressed = false

  function expand(name: string): void {
    if (expandedPreset === name && !isNew) {
      expandedPreset = ''
      return
    }
    const preset = presets[name]
    expandedPreset = name
    editName = name
    editDesc = preset.description
    editPrompt = preset.system_prompt
    editTemplate = preset.user_template
    editModel = preset.model
    base = { ...preset }
    isNew = false
    feedback = ''
  }

  function discardChanges(): void {
    if (isNew) {
      expandedPreset = ''
      isNew = false
      return
    }
    const preset = presets[expandedPreset]
    if (preset) {
      editDesc = preset.description
      editPrompt = preset.system_prompt
      editTemplate = preset.user_template
      editModel = preset.model
      base = { ...preset }
      feedback = 'Changes discarded.'
    }
  }

  function startNew(): void {
    expandedPreset = '__new__'
    editName = ''
    editDesc = ''
    editPrompt = ''
    editTemplate = '{{transcript}}'
    editModel = ''
    isNew = true
    base = undefined
    newStep = 'describe'
    generating = false
    feedback = ''
  }

  function editManually(): void {
    newStep = 'form'
  }

  async function generateFromLLM(): Promise<void> {
    if (!editDesc.trim()) {
      feedback = 'Description is required'
      return
    }
    generating = true
    feedback = ''
    try {
      const result = await generatePreset(editDesc.trim())
      editPrompt = result.system_prompt
      editTemplate = result.user_template || '{{transcript}}'
      newStep = 'form'
    } catch (error) {
      feedback = error instanceof Error ? error.message : 'Generation failed'
    } finally {
      generating = false
    }
  }

  async function save(): Promise<void> {
    const name = isNew ? editName.trim() : expandedPreset
    if (!name) {
      feedback = 'Preset name is required'
      return
    }
    // A new preset sends every field, so one under a name already taken
    // would replace that preset whole.
    if (isNew && Object.hasOwn(presets, name)) {
      feedback = `A preset named "${name}" already exists.`
      return
    }
    if (!editPrompt.trim()) {
      feedback = 'System prompt is required'
      return
    }
    if (!editTemplate.trim()) {
      feedback = 'User template is required'
      return
    }

    const edited: PresetDetail = {
      description: editDesc,
      system_prompt: editPrompt,
      user_template: editTemplate,
      model: editModel,
    }
    const fields: Partial<PresetDetail> = {}
    for (const key of Object.keys(edited) as (keyof PresetDetail)[]) {
      if (isNew || !base || edited[key] !== base[key]) {
        fields[key] = edited[key]
      }
    }
    if (Object.keys(fields).length === 0) {
      feedback = 'No changes to save.'
      return
    }

    saving = true
    feedback = ''
    try {
      const updated = await patchConfig({
        summarization: { presets: { [name]: fields } },
      })
      setConfig(updated)
      const saved = updated.summarization.presets[name]
      if (saved) {
        base = { ...saved }
      }
      feedback = 'Saved!'
      if (isNew) {
        isNew = false
        expandedPreset = name
      }
    } catch (error) {
      // The unit answers 404 to a change to a preset it does not hold that
      // does not create it: another tab or the unit deleted it.
      if (!isNew && error instanceof ConfigRequestError && error.status === 404) {
        feedback = 'This preset was deleted elsewhere.'
      } else {
        feedback = error instanceof Error ? error.message : 'Save failed'
      }
    } finally {
      saving = false
    }
  }

  async function remove(name: string): Promise<void> {
    if (name === 'default') return
    saving = true
    try {
      const updated = await patchConfig({
        summarization: { presets: { [name]: null } },
      })
      setConfig(updated)
      expandedPreset = ''
    } catch (error) {
      feedback = error instanceof Error ? error.message : 'Delete failed'
    } finally {
      saving = false
    }
  }

  function openRefine(name: string): void {
    refiningPreset = name
    refineFeedback = ''
  }

  async function runRefine(): Promise<void> {
    if (!refineFeedback.trim()) return
    refineRunning = true
    feedback = ''
    try {
      const result = await refinePreset(refiningPreset, refineFeedback.trim())
      editDesc = result.description
      editPrompt = result.system_prompt
      editTemplate = result.user_template || '{{transcript}}'
      refiningPreset = ''
      feedback = 'Refined! Review the changes and Save.'
    } catch (error) {
      feedback = error instanceof Error ? error.message : 'Refine failed'
    } finally {
      refineRunning = false
    }
  }

  async function openTest(name: string): Promise<void> {
    testingPreset = name
    testSessionId = ''
    testResult = ''
    testError = ''
    testSessions = []
    selectedSessionDetail = null

    // Load recent sessions for the dropdown.
    testSessionsLoading = true
    try {
      const dates = await fetchDates()
      const recentDates = dates.slice(0, 5)
      const allSessions: SessionSummary[] = []
      const sessionArrays = await Promise.all(recentDates.map((date) => fetchSessions(date)))
      allSessions.push(...sessionArrays.flat())
      // Most recent first, only completed sessions with transcripts.
      testSessions = allSessions
        .filter((s) => s.status === SessionStatus.Ended)
        .sort((a, b) => b.id.localeCompare(a.id))
        .slice(0, 20)
    } catch {
      testError = 'Failed to load sessions'
    } finally {
      testSessionsLoading = false
    }
  }

  async function selectTestSession(sessionId: string): Promise<void> {
    testSessionId = sessionId
    selectedSessionDetail = null
    if (!sessionId) return

    selectedDetailLoading = true
    try {
      const detail = await fetchSession(sessionId)
      const transcript = detail.segments
        .map((s: Segment) => s.text)
        .filter((t: string) => t.trim())
        .join('\n')
      selectedSessionDetail = {
        summary: detail.session.summary || '(no summary)',
        transcript: transcript || '(no transcript)',
      }
    } catch {
      selectedSessionDetail = { summary: '(failed to load)', transcript: '(failed to load)' }
    } finally {
      selectedDetailLoading = false
    }
  }

  async function runTest(): Promise<void> {
    if (!testSessionId.trim()) return
    testRunning = true
    testResult = ''
    testError = ''
    try {
      const resp = await testPreset(testingPreset, testSessionId.trim())
      if (resp.error) {
        testError = resp.error
      } else {
        testResult = resp.summary
      }
    } catch (error) {
      testError = error instanceof Error ? error.message : 'Test failed'
    } finally {
      testRunning = false
    }
  }
</script>

<div class="preset-editor" data-testid="preset-editor">
  <div class="preset-actions">
    <button class="add-btn" type="button" onclick={startNew}>+ Add Preset</button>
  </div>

  {#each Object.entries(presets) as [name, preset] (name)}
    <div class="preset-card" class:expanded={expandedPreset === name}>
      <button class="preset-header" type="button" onclick={() => expand(name)}>
        <strong>{name}</strong>
        <span class="preset-desc">{preset.description}</span>
      </button>

      {#if expandedPreset === name}
        <div class="preset-form">
          <div class="field">
            <label for="preset-name">Name</label>
            <input id="preset-name" type="text" value={name} disabled />
          </div>
          <div class="field">
            <label for="preset-description">Description</label>
            <input id="preset-description" type="text" bind:value={editDesc} />
          </div>
          <div class="field">
            <label for="preset-system-prompt">System Prompt</label>
            <textarea id="preset-system-prompt" rows="6" bind:value={editPrompt}></textarea>
          </div>
          <div class="field">
            <label for="preset-user-template">User Template</label>
            <textarea id="preset-user-template" rows="3" bind:value={editTemplate}></textarea>
            <span class="hint">Use {'{{transcript}}'} as placeholder</span>
          </div>
          <div class="field">
            <label for="preset-model">Model Override (optional)</label>
            <input
              id="preset-model"
              type="text"
              bind:value={editModel}
              placeholder="provider/model_name"
            />
          </div>
          <div class="form-actions">
            <button class="save-btn" type="button" onclick={save} disabled={saving}>
              {saving ? 'Saving...' : 'Save'}
            </button>
            <button class="test-btn" type="button" onclick={() => openTest(name)}> Test </button>
            <button class="refine-btn" type="button" onclick={() => openRefine(name)}>
              Refine
            </button>
            {#if name !== 'default'}
              <button
                class="delete-btn"
                type="button"
                onclick={() => remove(name)}
                disabled={saving}
              >
                Delete
              </button>
            {/if}
            <button class="cancel-btn" type="button" onclick={discardChanges}> Discard </button>
            {#if feedback}
              <span
                class="feedback"
                class:error={feedback !== 'Saved!' &&
                  feedback !== 'Changes discarded.' &&
                  feedback !== 'No changes to save.' &&
                  feedback !== 'Refined! Review the changes and Save.'}>{feedback}</span
              >
            {/if}
          </div>
        </div>
      {/if}
    </div>
  {/each}

  {#if isNew && expandedPreset === '__new__'}
    <div class="preset-card expanded">
      <div class="preset-form">
        <div class="field">
          <label for="new-preset-name">Name</label>
          <input id="new-preset-name" type="text" bind:value={editName} placeholder="my-preset" />
        </div>
        <div class="field">
          <label for="new-preset-description">Description</label>
          <input
            id="new-preset-description"
            type="text"
            bind:value={editDesc}
            placeholder="What kind of summary do you want?"
          />
        </div>

        {#if newStep === 'describe'}
          <div class="form-actions">
            <button
              class="generate-btn"
              type="button"
              onclick={generateFromLLM}
              disabled={generating || !editDesc.trim()}
            >
              {generating ? 'Generating...' : 'Generate with LLM'}
            </button>
            <button class="cancel-btn" type="button" onclick={editManually}> Edit manually </button>
            <button
              class="cancel-btn"
              type="button"
              onclick={() => {
                expandedPreset = ''
                isNew = false
              }}
            >
              Cancel
            </button>
            {#if feedback}
              <span class="feedback error">{feedback}</span>
            {/if}
          </div>
        {:else}
          <div class="field">
            <label for="new-preset-system-prompt">System Prompt</label>
            <textarea id="new-preset-system-prompt" rows="6" bind:value={editPrompt}></textarea>
          </div>
          <div class="field">
            <label for="new-preset-user-template">User Template</label>
            <textarea id="new-preset-user-template" rows="3" bind:value={editTemplate}></textarea>
            <span class="hint">Use {'{{transcript}}'} as placeholder</span>
          </div>
          <div class="field">
            <label for="new-preset-model">Model Override (optional)</label>
            <input
              id="new-preset-model"
              type="text"
              bind:value={editModel}
              placeholder="provider/model_name"
            />
          </div>
          <div class="form-actions">
            <button class="save-btn" type="button" onclick={save} disabled={saving}>
              {saving ? 'Creating...' : 'Create'}
            </button>
            <button
              class="cancel-btn"
              type="button"
              onclick={() => {
                expandedPreset = ''
                isNew = false
              }}
            >
              Cancel
            </button>
            {#if feedback}
              <span class="feedback" class:error={feedback !== 'Saved!'}>{feedback}</span>
            {/if}
          </div>
        {/if}
      </div>
    </div>
  {/if}

  {#if testingPreset}
    <!-- A press and click on the overlay itself, outside the dialog, closes it. -->
    <div
      class="test-modal-overlay"
      onpointerdown={(e) => {
        overlayPressed = e.target === e.currentTarget
      }}
      onclick={(e) => {
        if (overlayPressed && e.target === e.currentTarget) {
          testingPreset = ''
        }
      }}
      role="presentation"
    >
      <div class="test-modal" role="dialog">
        <h4>Test Preset: {testingPreset}</h4>

        <div class="field">
          {#if testSessionsLoading || testSessions.length === 0}
            <span class="field-label">Select Session</span>
            <p class="hint">{testSessionsLoading ? 'Loading sessions...' : 'No sessions found'}</p>
          {:else}
            <label for="test-session-select">Select Session</label>
            <select
              id="test-session-select"
              value={testSessionId}
              onchange={(e) => selectTestSession((e.target as HTMLSelectElement).value)}
            >
              <option value="">Choose a session...</option>
              {#each testSessions as session (session.id)}
                <option value={session.id}>
                  {new Date(session.started_at).toLocaleString()} — {session.summary
                    ? session.summary.slice(0, 60) + (session.summary.length > 60 ? '...' : '')
                    : '(no summary)'}
                </option>
              {/each}
            </select>
          {/if}
        </div>

        {#if selectedDetailLoading}
          <p class="hint">Loading session detail...</p>
        {/if}

        {#if selectedSessionDetail}
          <div class="session-preview">
            <div class="preview-section">
              <h5>Current Summary</h5>
              <pre class="preview-text">{selectedSessionDetail.summary}</pre>
            </div>
            <div class="preview-section">
              <h5>Transcript</h5>
              <pre class="preview-text transcript">{selectedSessionDetail.transcript}</pre>
            </div>
          </div>
        {/if}

        <div class="form-actions">
          <button
            class="save-btn"
            type="button"
            onclick={runTest}
            disabled={testRunning || !testSessionId}
          >
            {testRunning ? 'Running...' : 'Run Test'}
          </button>
          <button
            class="cancel-btn"
            type="button"
            onclick={() => {
              testingPreset = ''
            }}
          >
            Close
          </button>
        </div>

        {#if testError}
          <p class="test-error">{testError}</p>
        {/if}
        {#if testResult}
          <div class="test-result">
            <h5>Generated Summary:</h5>
            <pre>{testResult}</pre>
          </div>
        {/if}
      </div>
    </div>
  {/if}

  {#if refiningPreset}
    <!-- A press and click on the overlay itself, outside the dialog, closes it. -->
    <div
      class="test-modal-overlay"
      onpointerdown={(e) => {
        overlayPressed = e.target === e.currentTarget
      }}
      onclick={(e) => {
        if (overlayPressed && e.target === e.currentTarget) {
          refiningPreset = ''
        }
      }}
      role="presentation"
    >
      <div class="test-modal" role="dialog">
        <h4>Refine Preset: {refiningPreset}</h4>
        <div class="field">
          <label for="refine-feedback">What would you like to change?</label>
          <textarea
            id="refine-feedback"
            rows="4"
            bind:value={refineFeedback}
            placeholder="e.g. Make it more concise, focus on action items, add a section for follow-ups..."
          ></textarea>
        </div>
        <div class="form-actions">
          <button
            class="generate-btn"
            type="button"
            onclick={runRefine}
            disabled={refineRunning || !refineFeedback.trim()}
          >
            {refineRunning ? 'Refining...' : 'Refine with LLM'}
          </button>
          <button
            class="cancel-btn"
            type="button"
            onclick={() => {
              refiningPreset = ''
            }}
          >
            Cancel
          </button>
        </div>
      </div>
    </div>
  {/if}
</div>

<style>
  .preset-editor {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }

  .preset-actions {
    margin-bottom: 0.5rem;
  }

  .add-btn {
    background: var(--accent-soft);
    color: var(--accent);
    border: 1px solid var(--accent);
    border-radius: 6px;
    padding: 0.4rem 1rem;
    cursor: pointer;
    font-family: inherit;
    font-weight: 600;
  }

  .preset-card {
    border: 1px solid var(--line);
    border-radius: 8px;
    overflow: hidden;
    background: var(--panel);
  }

  .preset-card.expanded {
    border-color: var(--accent);
  }

  .preset-header {
    display: flex;
    align-items: center;
    gap: 1rem;
    width: 100%;
    padding: 0.75rem 1rem;
    background: none;
    border: none;
    cursor: pointer;
    text-align: left;
    font-family: inherit;
  }

  .preset-header:hover {
    background: var(--accent-soft);
  }

  .preset-desc {
    color: var(--muted);
    font-size: 0.85rem;
  }

  .preset-form {
    padding: 1rem;
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    border-top: 1px solid var(--line);
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }

  .field label,
  .field-label {
    font-weight: 600;
    font-size: 0.85rem;
  }

  .field input,
  .field textarea {
    padding: 0.5rem;
    border: 1px solid var(--line);
    border-radius: 6px;
    font-family: var(--font-mono);
    font-size: 0.85rem;
    background: var(--card);
    resize: vertical;
  }

  .hint {
    font-size: 0.75rem;
    color: var(--muted);
  }

  .form-actions {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-wrap: wrap;
  }

  .save-btn {
    background: var(--accent);
    color: white;
    border: none;
    border-radius: 6px;
    padding: 0.4rem 1rem;
    cursor: pointer;
    font-family: inherit;
    font-weight: 600;
  }

  .save-btn:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }

  .generate-btn {
    background: var(--accent);
    color: white;
    border: none;
    border-radius: 6px;
    padding: 0.4rem 1rem;
    cursor: pointer;
    font-family: inherit;
    font-weight: 600;
  }

  .generate-btn:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }

  .test-btn {
    background: var(--accent-soft);
    color: var(--accent);
    border: 1px solid var(--accent);
    border-radius: 6px;
    padding: 0.4rem 1rem;
    cursor: pointer;
    font-family: inherit;
  }

  .refine-btn {
    background: var(--accent-soft);
    color: var(--accent);
    border: 1px solid var(--accent);
    border-radius: 6px;
    padding: 0.4rem 1rem;
    cursor: pointer;
    font-family: inherit;
  }

  .delete-btn {
    background: none;
    color: var(--danger);
    border: 1px solid var(--danger);
    border-radius: 6px;
    padding: 0.4rem 1rem;
    cursor: pointer;
    font-family: inherit;
  }

  .cancel-btn {
    background: none;
    border: 1px solid var(--line);
    border-radius: 6px;
    padding: 0.4rem 1rem;
    cursor: pointer;
    font-family: inherit;
  }

  .feedback {
    font-size: 0.85rem;
    color: var(--accent);
  }

  .feedback.error {
    color: var(--danger);
  }

  .test-modal-overlay {
    position: fixed;
    top: 0;
    left: 0;
    right: 0;
    bottom: 0;
    background: rgba(0, 0, 0, 0.6);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 100;
  }

  .test-modal {
    background: var(--card);
    border-radius: 0.6rem;
    padding: 1rem;
    width: 90%;
    max-width: 700px;
    max-height: 80vh;
    overflow-y: auto;
  }

  .test-modal h4 {
    margin: 0 0 0.75rem;
  }

  .test-error {
    color: var(--danger);
    margin: 0.5rem 0;
  }

  .test-result {
    margin-top: 1rem;
    border-top: 1px solid var(--line);
    padding-top: 0.75rem;
  }

  .test-result h5 {
    margin: 0 0 0.5rem;
  }

  .test-result pre {
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 6px;
    padding: 0.75rem;
    white-space: pre-wrap;
    font-size: 0.85rem;
    max-height: 300px;
    overflow-y: auto;
  }

  select {
    padding: 0.5rem;
    border: 1px solid var(--line);
    border-radius: 6px;
    font-family: inherit;
    font-size: 0.85rem;
    background: var(--panel);
    width: 100%;
  }

  .session-preview {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    margin: 0.75rem 0;
  }

  .preview-section h5 {
    margin: 0 0 0.25rem;
    font-size: 0.85rem;
    color: var(--muted);
  }

  .preview-text {
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 6px;
    padding: 0.5rem;
    white-space: pre-wrap;
    font-size: 0.8rem;
    max-height: 150px;
    overflow-y: auto;
    margin: 0;
  }

  .preview-text.transcript {
    max-height: 100px;
    color: var(--muted);
  }
</style>
