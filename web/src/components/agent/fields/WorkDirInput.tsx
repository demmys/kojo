/** The "File Storage" (work directory) field. */
export function WorkDirInput({
  workDir,
  setWorkDir,
}: {
  workDir: string;
  setWorkDir: (v: string) => void;
}) {
  return (
    <div>
      <label className="block text-sm text-neutral-400 mb-2">File Storage</label>
      <input
        type="text"
        value={workDir}
        onChange={(e) => setWorkDir(e.target.value)}
        placeholder="(default: agent data dir)"
        className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm font-mono focus:outline-none focus:border-neutral-500"
      />
      <p className="text-xs text-neutral-600 mt-1">Generated files are saved here.</p>
    </div>
  );
}
