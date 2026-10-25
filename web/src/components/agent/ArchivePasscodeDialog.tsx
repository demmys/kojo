import { useState } from "react";
import { Button } from "../ui/Button";
import { Banner } from "../ui/Banner";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { useT } from "../../lib/i18n";

/** Must match agent.MinArchivePasscodeLen on the server. */
export const MIN_ARCHIVE_PASSCODE_LEN = 8;

interface Props {
  /** "archive" asks twice and enforces the minimum length; "unarchive" asks once. */
  mode: "archive" | "unarchive";
  title: string;
  description: string;
  submitLabel: string;
  busyLabel: string;
  busy: boolean;
  error: string;
  onSubmit: (passcode: string) => void;
  onCancel: () => void;
}

/**
 * Passcode prompt for sealed agent archives. Archive encrypts the agent
 * directory with the passcode; unarchive needs the same passcode back.
 */
export function ArchivePasscodeDialog({
  mode,
  title,
  description,
  submitLabel,
  busyLabel,
  busy,
  error,
  onSubmit,
  onCancel,
}: Props) {
  const t = useT();
  const [passcode, setPasscode] = useState("");
  const [confirm, setConfirm] = useState("");
  const tooShort = [...passcode].length < MIN_ARCHIVE_PASSCODE_LEN;
  const mismatch = mode === "archive" && confirm !== "" && confirm !== passcode;
  const canSubmit =
    !busy &&
    passcode !== "" &&
    (mode === "unarchive" || (!tooShort && confirm === passcode));

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (canSubmit) onSubmit(passcode);
  };

  return (
    <div
      role="dialog"
      aria-modal="true"
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 outline-none"
      onClick={(e) => {
        if (e.target === e.currentTarget && !busy) onCancel();
      }}
      onKeyDown={(e) => {
        if (e.key === "Escape" && !busy) onCancel();
      }}
    >
      <form
        onSubmit={submit}
        className="w-[22rem] max-w-[calc(100vw-2rem)] rounded-[10px] border border-hairline bg-raised p-5 shadow-xl shadow-black/50"
      >
        <h3 className="mb-2 text-[14px] font-semibold text-ink">{title}</h3>
        <p className="mb-4 whitespace-pre-line text-[12px] text-ink-faint">{description}</p>
        <Field
          label={t("archive.passcode")}
          className="mb-3"
          error={
            mode === "archive" && passcode !== "" && tooShort
              ? t("archive.passcodeTooShort", { n: String(MIN_ARCHIVE_PASSCODE_LEN) })
              : undefined
          }
        >
          <Input
            type="password"
            autoComplete={mode === "archive" ? "new-password" : "current-password"}
            value={passcode}
            onChange={(e) => setPasscode(e.target.value)}
            disabled={busy}
            invalid={mode === "archive" && passcode !== "" && tooShort}
            autoFocus
          />
        </Field>
        {mode === "archive" && (
          <Field
            label={t("archive.passcodeConfirm")}
            className="mb-3"
            error={mismatch ? t("archive.passcodeMismatch") : undefined}
          >
            <Input
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              disabled={busy}
              invalid={mismatch}
            />
          </Field>
        )}
        {error && (
          <div className="mb-3">
            <Banner tone="error">{error}</Banner>
          </div>
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" onClick={onCancel} disabled={busy}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="primary" disabled={!canSubmit}>
            {busy ? busyLabel : submitLabel}
          </Button>
        </div>
      </form>
    </div>
  );
}
