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
  import { unitField, type UnitField } from '../lib/unit-field.svelte'
  import KeptNote from './KeptNote.svelte'
  import ReplacedNote from './ReplacedNote.svelte'

  let {
    presets,
  }: {
    presets: Record<string, PresetDetail>
  } = $props()

  let expandedPreset = $state('')
  // The new-preset form's own fields, sent whole on Create.
  let newName = $state('')
  let newDesc = $state('')
  let newPrompt = $state('')
  let newTemplate = $state('')
  let newModel = $state('')
  let isNew = $state(false)
  let newStep = $state<'describe' | 'form'>('describe')
  let generating = $state(false)
  let saving = $state(false)
  let feedback = $state('')

  // The open preset's fields, as the other settings sections' fields: each
  // shows the preset as the config the page last took holds it, or what the
  // person typed over it, and a save sends the fields the person edited.
  const openPreset = (): PresetDetail | undefined => presets[expandedPreset]
  const fields: Record<keyof PresetDetail, UnitField<string>> = {
    description: unitField(
      () => openPreset()?.description ?? '',
      () => presets,
    ),
    system_prompt: unitField(
      () => openPreset()?.system_prompt ?? '',
      () => presets,
    ),
    user_template: unitField(
      () => openPreset()?.user_template ?? '',
      () => presets,
    ),
    model: unitField(
      () => openPreset()?.model ?? '',
      () => presets,
    ),
  }

  function resetFields(): void {
    for (const field of Object.values(fields)) field.reset()
  }

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
  // refineRuns counts the refinements started and the Refine dialogs opened or
  // closed, so an answer applies only to the refinement still waiting in its
  // dialog: one the person cancelled, or a dialog opened again, is over.
  let refineRuns = 0

  // Whether the press that a click on a dialog's overlay ends began on the
  // overlay too: a press inside the dialog, such as one that selects text,
  // can end on the overlay, and must not close the dialog.
  let overlayPressed = false

  // opens counts the forms opened, so a save's answer can tell whether its
  // form is still the one open: the same preset opened again, or a second
  // new-preset form, is not.
  let opens = 0

  function expand(name: string): void {
    if (expandedPreset === name && !isNew) {
      expandedPreset = ''
      return
    }
    opens++
    expandedPreset = name
    isNew = false
    resetFields()
    feedback = ''
  }

  function discardChanges(): void {
    if (isNew) {
      expandedPreset = ''
      isNew = false
      return
    }
    if (openPreset()) {
      resetFields()
      feedback = 'Changes discarded.'
    }
  }

  function startNew(): void {
    opens++
    expandedPreset = '__new__'
    newName = ''
    newDesc = ''
    newPrompt = ''
    newTemplate = '{{transcript}}'
    newModel = ''
    isNew = true
    newStep = 'describe'
    generating = false
    feedback = ''
  }

  function editManually(): void {
    newStep = 'form'
  }

  async function generateFromLLM(): Promise<void> {
    if (!newDesc.trim()) {
      feedback = 'Description is required'
      return
    }
    generating = true
    feedback = ''
    try {
      const result = await generatePreset(newDesc.trim())
      newPrompt = result.system_prompt
      newTemplate = result.user_template || '{{transcript}}'
      newStep = 'form'
    } catch (error) {
      feedback = error instanceof Error ? error.message : 'Generation failed'
    } finally {
      generating = false
    }
  }

  async function save(): Promise<void> {
    const name = isNew ? newName.trim() : expandedPreset
    if (!name) {
      feedback = 'Preset name is required'
      return
    }
    const prompt = isNew ? newPrompt : fields.system_prompt.value
    const template = isNew ? newTemplate : fields.user_template.value
    if (!prompt.trim()) {
      feedback = 'System prompt is required'
      return
    }
    if (!template.trim()) {
      feedback = 'User template is required'
      return
    }
    // A new preset sends every field, so one under a name already taken
    // would replace that preset whole.
    if (isNew && Object.hasOwn(presets, name)) {
      feedback = `A preset named "${name}" already exists.`
      return
    }

    // A new preset sends every field; an open one, the fields the person
    // edited, each acknowledged once the unit takes it.
    const sent: Partial<PresetDetail> = {}
    const carried: UnitField<string>[] = []
    if (isNew) {
      Object.assign(sent, {
        description: newDesc,
        system_prompt: newPrompt,
        user_template: newTemplate,
        model: newModel,
      })
    } else {
      for (const key of Object.keys(fields) as (keyof PresetDetail)[]) {
        if (fields[key].edited) {
          sent[key] = fields[key].value
          carried.push(fields[key])
        }
      }
      if (carried.length === 0) {
        feedback = 'No changes to save.'
        return
      }
    }

    // The person can open another form while the save is on its way; its
    // answer then changes the stored config and no form.
    const opened = isNew ? '__new__' : name
    const opening = opens
    const stillOpen = () => opens === opening && expandedPreset === opened
    saving = true
    feedback = ''
    try {
      const updated = await patchConfig({
        summarization: { presets: { [name]: sent } },
      })
      const take = setConfig(updated)
      if (!stillOpen()) return
      for (const field of carried) field.acknowledge(take)
      feedback = 'Saved!'
      if (isNew) {
        isNew = false
        expandedPreset = name
        resetFields()
      }
    } catch (error) {
      if (!stillOpen()) return
      // The unit answers 404 to a save of fields to a preset it does not
      // hold: another tab or the unit deleted it while it was open here.
      if (opened !== '__new__' && error instanceof ConfigRequestError && error.status === 404) {
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
    refineRuns++
    refiningPreset = name
    refineFeedback = ''
  }

  // Cancel, or a click outside the dialog: a refinement still on its way is
  // abandoned, and its answer changes nothing.
  function closeRefine(): void {
    refineRuns++
    refiningPreset = ''
    refineRunning = false
  }

  async function runRefine(): Promise<void> {
    if (!refineFeedback.trim()) return
    const run = ++refineRuns
    const name = refiningPreset
    refineRunning = true
    feedback = ''
    try {
      const result = await refinePreset(name, refineFeedback.trim())
      if (run !== refineRuns) return
      // The refinement starts from the preset as the unit held it (from): a
      // field it returns as it was there is no edit, and what the person typed
      // there stays, with a note saying the refinement's text was kept out.
      const from = result.from
      fields.description.propose(result.description, from?.description)
      fields.system_prompt.propose(result.system_prompt, from?.system_prompt)
      fields.user_template.propose(result.user_template || '{{transcript}}', from?.user_template)
      refiningPreset = ''
      feedback = 'Refined! Review the changes and Save.'
    } catch (error) {
      if (run !== refineRuns) return
      feedback = error instanceof Error ? error.message : 'Refine failed'
    } finally {
      if (run === refineRuns) refineRunning = false
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
            <input
              id="preset-description"
              type="text"
              bind:value={fields.description.value}
              disabled={saving}
            />
            <ReplacedNote shown={fields.description.replaced} />
            <KeptNote shown={fields.description.kept} />
          </div>
          <div class="field">
            <label for="preset-system-prompt">System Prompt</label>
            <textarea
              id="preset-system-prompt"
              rows="6"
              bind:value={fields.system_prompt.value}
              disabled={saving}
            ></textarea>
            <ReplacedNote shown={fields.system_prompt.replaced} />
            <KeptNote shown={fields.system_prompt.kept} />
          </div>
          <div class="field">
            <label for="preset-user-template">User Template</label>
            <textarea
              id="preset-user-template"
              rows="3"
              bind:value={fields.user_template.value}
              disabled={saving}
            ></textarea>
            <ReplacedNote shown={fields.user_template.replaced} />
            <KeptNote shown={fields.user_template.kept} />
            <span class="hint">Use {'{{transcript}}'} as placeholder</span>
          </div>
          <div class="field">
            <label for="preset-model">Model Override (optional)</label>
            <input
              id="preset-model"
              type="text"
              bind:value={fields.model.value}
              disabled={saving}
              placeholder="provider/model_name"
            />
            <ReplacedNote shown={fields.model.replaced} />
          </div>
          <div class="form-actions">
            <button
              class="save-btn"
              type="button"
              onclick={save}
              disabled={saving || refineRunning}
            >
              {saving ? 'Saving...' : 'Save'}
            </button>
            <button class="test-btn" type="button" onclick={() => openTest(name)}> Test </button>
            <button
              class="refine-btn"
              type="button"
              onclick={() => openRefine(name)}
              disabled={saving}
            >
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
          <input
            id="new-preset-name"
            type="text"
            bind:value={newName}
            disabled={saving}
            placeholder="my-preset"
          />
        </div>
        <div class="field">
          <label for="new-preset-description">Description</label>
          <input
            id="new-preset-description"
            type="text"
            bind:value={newDesc}
            disabled={saving}
            placeholder="What kind of summary do you want?"
          />
        </div>

        {#if newStep === 'describe'}
          <div class="form-actions">
            <button
              class="generate-btn"
              type="button"
              onclick={generateFromLLM}
              disabled={generating || !newDesc.trim()}
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
            <textarea
              id="new-preset-system-prompt"
              rows="6"
              bind:value={newPrompt}
              disabled={saving}
            ></textarea>
          </div>
          <div class="field">
            <label for="new-preset-user-template">User Template</label>
            <textarea
              id="new-preset-user-template"
              rows="3"
              bind:value={newTemplate}
              disabled={saving}
            ></textarea>
            <span class="hint">Use {'{{transcript}}'} as placeholder</span>
          </div>
          <div class="field">
            <label for="new-preset-model">Model Override (optional)</label>
            <input
              id="new-preset-model"
              type="text"
              bind:value={newModel}
              disabled={saving}
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
          closeRefine()
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
          <button class="cancel-btn" type="button" onclick={closeRefine}> Cancel </button>
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
