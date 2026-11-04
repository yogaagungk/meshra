import { useEffect, useState, type ReactNode } from "react";
import {
  Activity,
  Cpu,
  Globe,
  KeyRound,
  MessagesSquare,
  Palette,
  Plug,
  Server,
  Sparkles,
  Waypoints,
  type LucideIcon,
} from "lucide-react";
import { go } from "./App";
import { TabBar, TabPanel } from "./components/Tabs";
import { AIStatusPanel } from "./AIStatus";
import { MCPConnections, NetworkGrants, ProviderConnections, RuntimeReadiness } from "./Connections";
import { MessagingConnections } from "./MessagingConnections";
import { ModelsUsage, PersonalControls } from "./Personal";
import "./settings.css";

type Section = { id: string; label: string; icon: LucideIcon; group: string };

/** Settings, grouped by what a person is trying to do. */
const SECTIONS: Section[] = [
  { id: "appearance", label: "Appearance", icon: Palette, group: "General" },
  { id: "access", label: "Personal access", icon: KeyRound, group: "General" },
  { id: "providers", label: "Providers", icon: Plug, group: "AI" },
  { id: "models", label: "Models & usage", icon: Sparkles, group: "AI" },
  { id: "ai-status", label: "AI status", icon: Activity, group: "AI" },
  { id: "network", label: "Network access", icon: Globe, group: "Connections" },
  { id: "mcp", label: "MCP servers", icon: Waypoints, group: "Connections" },
  { id: "messaging", label: "Messaging", icon: MessagesSquare, group: "Connections" },
  { id: "resources", label: "Resources", icon: Cpu, group: "System" },
  { id: "runtime", label: "Runtime", icon: Server, group: "System" },
];
const ORDER = SECTIONS.map((s) => s.id);

function useWide() {
  const query = "(min-width: 900px)";
  const [wide, setWide] = useState(() => matchMedia(query).matches);
  useEffect(() => {
    const m = matchMedia(query);
    const on = () => setWide(m.matches);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, []);
  return wide;
}

export function Settings({ section, appearance }: { section?: string; appearance: ReactNode }) {
  const current = ORDER.includes(section || "") ? (section as string) : "appearance";
  const wide = useWide();
  const items = SECTIONS.map((s, i) => ({
    id: s.id,
    label: s.label,
    icon: <s.icon size={16} aria-hidden="true" />,
    heading: i === 0 || SECTIONS[i - 1].group !== s.group ? s.group : undefined,
  }));
  return (
    <div className="settings">
      <div className="settings-nav">
        <TabBar
          label="Settings sections"
          value={current}
          onChange={(id) => go("settings", id)}
          items={items}
          orientation={wide ? "vertical" : "horizontal"}
          variant="pill"
        />
      </div>
      <div className="settings-body">
        <TabPanel value={current} order={ORDER}>
          {current === "appearance" ? (
            appearance
          ) : current === "access" || current === "resources" ? (
            <PersonalControls access={current === "access"} />
          ) : current === "providers" ? (
            <ProviderConnections />
          ) : current === "models" ? (
            <ModelsUsage />
          ) : current === "ai-status" ? (
            <AIStatusPanel />
          ) : current === "network" ? (
            <NetworkGrants />
          ) : current === "mcp" ? (
            <MCPConnections />
          ) : current === "messaging" ? (
            <MessagingConnections />
          ) : (
            <RuntimeReadiness />
          )}
        </TabPanel>
      </div>
    </div>
  );
}
