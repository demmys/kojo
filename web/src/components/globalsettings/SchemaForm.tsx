import type { JSONSchema, JSONSchemaProperty } from "../../lib/extensionsApi";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Select } from "../ui/Select";
import { Textarea } from "../ui/Textarea";
import { Toggle } from "../ui/Toggle";

/**
 * Renders the flat subset of JSON Schema an extension package may ship
 * as its settings form. Extensions cannot contribute React — the web
 * bundle is built ahead of time — so a declarative schema is the whole
 * configuration surface.
 *
 * Supported: a top-level object whose properties are string, number,
 * integer, boolean, or a string enum. Unknown property types render a
 * disabled placeholder rather than disappearing, so a package author
 * sees immediately that the field is unsupported instead of wondering
 * why their setting never saves.
 */
export function SchemaForm({
  schema,
  value,
  onChange,
  disabled = false,
  idPrefix = "schemaform",
}: {
  schema: JSONSchema;
  value: Record<string, unknown>;
  onChange: (next: Record<string, unknown>) => void;
  disabled?: boolean;
  idPrefix?: string;
}) {
  const props = schema.properties ?? {};
  const keys = Object.keys(props);
  if (keys.length === 0) {
    return <p className="text-sm text-ink-faint">{schema.description ?? ""}</p>;
  }
  const required = new Set(schema.required ?? []);

  const set = (key: string, next: unknown) => {
    onChange({ ...value, [key]: next });
  };

  return (
    <div className="space-y-3">
      {keys.map((key) => {
        const prop = props[key];
        const id = `${idPrefix}-${key}`;
        const label = (prop.title ?? key) + (required.has(key) ? " *" : "");
        return (
          <Field key={key} label={label} htmlFor={id} help={prop.description}>
            {renderControl(id, key, prop, value[key], set, disabled)}
          </Field>
        );
      })}
    </div>
  );
}

function renderControl(
  id: string,
  key: string,
  prop: JSONSchemaProperty,
  current: unknown,
  set: (key: string, next: unknown) => void,
  disabled: boolean,
) {
  if (prop.enum && prop.enum.length > 0) {
    return (
      <Select
        id={id}
        value={typeof current === "string" ? current : ""}
        disabled={disabled}
        onChange={(e) => set(key, e.target.value)}
      >
        <option value="" />
        {prop.enum.map((opt) => (
          <option key={opt} value={opt}>
            {opt}
          </option>
        ))}
      </Select>
    );
  }

  switch (prop.type) {
    case "boolean":
      return (
        <Toggle
          id={id}
          checked={current === true}
          disabled={disabled}
          onChange={(next) => set(key, next)}
        />
      );
    case "number":
    case "integer":
      return (
        <Input
          id={id}
          type="number"
          value={typeof current === "number" ? String(current) : ""}
          disabled={disabled}
          onChange={(e) => {
            // An empty box means "unset", not 0 — coercing "" to 0
            // would silently write a value the operator never chose.
            const raw = e.target.value;
            if (raw === "") {
              set(key, undefined);
              return;
            }
            const parsed = Number(raw);
            set(key, Number.isNaN(parsed) ? undefined : parsed);
          }}
        />
      );
    case "string":
      if (prop.multiline) {
        return (
          <Textarea
            id={id}
            rows={4}
            value={typeof current === "string" ? current : ""}
            disabled={disabled}
            onChange={(e) => set(key, e.target.value)}
          />
        );
      }
      return (
        <Input
          id={id}
          type={prop.format === "password" ? "password" : "text"}
          mono={prop.format === "uri" || prop.format === "password"}
          value={typeof current === "string" ? current : ""}
          disabled={disabled}
          onChange={(e) => set(key, e.target.value)}
        />
      );
    default:
      return (
        <Input id={id} value="" disabled readOnly placeholder={`unsupported type: ${prop.type ?? "?"}`} />
      );
  }
}
