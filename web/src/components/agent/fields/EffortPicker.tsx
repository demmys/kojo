import {
  defaultEffortForModel,
  effortLevelsForModel,
  supportsEffort,
  type EffortLevel,
} from "../../../lib/toolModels";

/**
 * The "Effort" field. Rendered only for backends that support an effort
 * selector; the option list + default label are derived from the current
 * model.
 */
export function EffortPicker({
  tool,
  effort,
  setEffort,
  model,
}: {
  tool: string;
  effort: EffortLevel | "";
  setEffort: (e: EffortLevel | "") => void;
  model: string;
}) {
  if (!supportsEffort(tool)) return null;
  return (
    <div>
      <label className="block text-sm text-neutral-400 mb-2">Effort</label>
      <select
        value={effort}
        onChange={(e) => setEffort(e.target.value as EffortLevel | "")}
        className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
      >
        <option value="">default ({defaultEffortForModel(model)})</option>
        {effortLevelsForModel(model).map((e) => (
          <option key={e} value={e}>
            {e}
          </option>
        ))}
      </select>
    </div>
  );
}
