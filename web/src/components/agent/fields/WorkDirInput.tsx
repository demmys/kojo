import { Field } from "../../ui/Field";
import { Input } from "../../ui/Input";
import { useT } from "../../../lib/i18n";

/** The "File Storage" (work directory) field. */
export function WorkDirInput({
  workDir,
  setWorkDir,
}: {
  workDir: string;
  setWorkDir: (v: string) => void;
}) {
  const t = useT();
  return (
    <Field label={t("field.fileStorage")} help={t("field.fileStorageHelp")}>
      <Input
        mono
        value={workDir}
        onChange={(e) => setWorkDir(e.target.value)}
        placeholder={t("field.workDirPlaceholder")}
      />
    </Field>
  );
}
