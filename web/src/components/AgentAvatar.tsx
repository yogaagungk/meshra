import { Bot, Flower2, Hexagon, Orbit, Sparkles, Star, Zap } from "lucide-react";
import "./AgentAvatar.css";

const icons = [
  { id: "spark", label: "Spark", Icon: Sparkles },
  { id: "orbit", label: "Orbit", Icon: Orbit },
  { id: "bot", label: "Bot", Icon: Bot },
  { id: "star", label: "Star", Icon: Star },
  { id: "hex", label: "Hexagon", Icon: Hexagon },
  { id: "flower", label: "Flower", Icon: Flower2 },
  { id: "bolt", label: "Bolt", Icon: Zap },
] as const;

export const defaultAvatar = { icon: "spark", color: "#8b7cf6" };
export type AvatarValue = { icon: string; color: string };

export function AgentAvatar({ icon, color, size = 36 }: { icon?: string; color?: string; size?: number }) {
  const selected = icons.find((item) => item.id === icon) || icons[0];
  const safeColor = color && /^#[\da-f]{6}$/i.test(color) ? color : defaultAvatar.color;
  const Icon = selected.Icon;
  return <span className="agent-avatar" style={{ width: size, height: size, color: safeColor }} aria-hidden="true"><Icon className="agent-avatar-symbol" size={Math.round(size * 0.55)} strokeWidth={1.8} /></span>;
}

export function AvatarPicker({ value, onChange }: { value: AvatarValue; onChange: (value: AvatarValue) => void }) {
  return <fieldset className="avatar-picker">
    <legend>Agent icon</legend>
    <div className="avatar-picker-options" role="radiogroup" aria-label="Agent icon">
      {icons.map(({ id, label }) => <button key={id} type="button" role="radio" aria-checked={value.icon === id} aria-label={label} title={label} onClick={() => onChange({ ...value, icon: id })}><AgentAvatar icon={id} color={value.color} /></button>)}
    </div>
    <label className="avatar-color">Icon color <input type="color" value={/^#[\da-f]{6}$/i.test(value.color) ? value.color : defaultAvatar.color} onChange={(event) => onChange({ ...value, color: event.target.value })} /></label>
  </fieldset>;
}
