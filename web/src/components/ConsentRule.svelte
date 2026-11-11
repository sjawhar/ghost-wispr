<script lang="ts">
  let open = $state(false)

  function close(): void {
    open = false
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      close()
    }
  }
</script>

<svelte:window onkeydown={handleKeydown} />

<button
  class="recording-rules-button"
  type="button"
  aria-haspopup="dialog"
  aria-expanded={open}
  onclick={() => {
    open = true
  }}
>
  Recording rules
</button>

{#if open}
  <dialog class="consent-dialog" open aria-labelledby="recording-rules-title">
    <div class="dialog-heading">
      <h2 id="recording-rules-title">Recording rules</h2>
      <button class="close-button" type="button" aria-label="Close recording rules" onclick={close}>
        ×
      </button>
    </div>
    <p>
      Record only while the light is on. Anyone may pause it at any time without a reason.
      Transcripts are readable by Theorem staff only. A person can ask for a segment to be deleted.
    </p>
    <p>
      Tap the button on the recorder to pause or resume. To delete a segment, open the conversation
      and choose Delete on that segment.
    </p>
  </dialog>
{/if}

<style>
  .recording-rules-button,
  .close-button {
    border: 1px solid var(--line);
    background: none;
    border-radius: 0.4rem;
    color: var(--muted);
    cursor: pointer;
    font: inherit;
  }

  .recording-rules-button {
    padding: 0.3rem 0.5rem;
    font-size: 0.75rem;
  }

  .recording-rules-button:hover,
  .close-button:hover {
    border-color: var(--accent);
    color: var(--accent);
  }

  .consent-dialog {
    position: fixed;
    z-index: 10;
    inset: 0;
    width: min(32rem, calc(100% - 2rem));
    height: fit-content;
    margin: auto;
    border: 1px solid var(--line-strong);
    border-radius: 0.6rem;
    background: var(--panel);
    box-shadow: 0 1rem 3rem rgb(0 0 0 / 35%);
    padding: 1rem;
  }

  .consent-dialog::backdrop {
    background: rgb(0 0 0 / 55%);
  }

  .dialog-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
  }

  h2,
  p {
    margin: 0;
  }

  h2 {
    font-size: 1rem;
  }

  p {
    margin-top: 0.75rem;
    color: var(--ink);
  }

  .close-button {
    width: 2rem;
    height: 2rem;
    font-size: 1.25rem;
    line-height: 1;
  }
</style>
