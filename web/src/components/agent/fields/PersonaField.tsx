/**
 * The "Persona" field: a description textarea plus an inline "AI" prompt row
 * that regenerates/edits the persona. The per-caller differences (textarea
 * rows + placeholder, prompt placeholder, and the busy/spinner flags) are
 * threaded as props so each screen renders exactly as before.
 *
 * `busy` disables the prompt input's Enter shortcut and the button; `spinning`
 * swaps the button label for a spinner. AgentSettings passes the same flag for
 * both; AgentCreate disables on any generation but only spins for persona.
 */
export function PersonaField({
  persona,
  setPersona,
  textareaRows,
  textareaPlaceholder,
  personaPrompt,
  setPersonaPrompt,
  promptPlaceholder,
  busy,
  spinning,
  onGenerate,
}: {
  persona: string;
  setPersona: (v: string) => void;
  textareaRows: number;
  textareaPlaceholder?: string;
  personaPrompt: string;
  setPersonaPrompt: (v: string) => void;
  promptPlaceholder: string;
  busy: boolean;
  spinning: boolean;
  onGenerate: () => void;
}) {
  return (
    <div>
      <label className="block text-sm text-neutral-400 mb-2">
        Persona
      </label>
      <textarea
        value={persona}
        onChange={(e) => setPersona(e.target.value)}
        placeholder={textareaPlaceholder}
        rows={textareaRows}
        className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm resize-none focus:outline-none focus:border-neutral-500"
      />
      <div className="flex gap-2 mt-2">
        <input
          type="text"
          value={personaPrompt}
          onChange={(e) => setPersonaPrompt(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.nativeEvent.isComposing && !e.shiftKey && !busy) {
              e.preventDefault();
              onGenerate();
            }
          }}
          placeholder={promptPlaceholder}
          className="flex-1 px-3 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
        />
        <button
          onClick={onGenerate}
          disabled={busy || !personaPrompt.trim()}
          className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-xs disabled:opacity-40 flex items-center gap-1"
        >
          {spinning ? (
            <span className="animate-spin">↻</span>
          ) : (
            "✨ AI"
          )}
        </button>
      </div>
    </div>
  );
}
