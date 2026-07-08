import { SectionCard } from "../ui/SectionCard";
import { Toggle } from "../ui/Toggle";
import { useT } from "../../lib/i18n";

interface Props {
  enterSends: boolean;
  setEnterSends: (v: boolean) => void;
}

/** Chat preferences section — Enter key send behavior toggle. */
export function ChatPreferencesSection({ enterSends, setEnterSends }: Props) {
  const t = useT();
  return (
    <SectionCard title={t("gs.chat")}>
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="text-[13px] text-ink">{t("gs.sendWithEnter")}</div>
          <div className="mt-0.5 text-[12px] text-ink-faint">
            {enterSends ? t("gs.enterSendsHelp") : t("gs.ctrlEnterSendsHelp")}
          </div>
        </div>
        <Toggle checked={enterSends} onChange={setEnterSends} aria-label={t("gs.sendWithEnter")} />
      </div>
    </SectionCard>
  );
}
