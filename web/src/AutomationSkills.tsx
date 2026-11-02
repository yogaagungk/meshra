import { useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Sparkles } from "lucide-react";
import { automation, type Skill } from "./automation-api";
import { Empty, Notice } from "./App";
import { Button } from "./components/ui/button";
import { Dialog } from "./components/ui/dialog";
import { Markdown } from "./Markdown";
import { useProfiles } from "./automationShared";

type Binding = { profileId: string; skillId: string; version: number; enabled: boolean; revision: number };

/** Which profiles pin this skill, read per profile because bindings are stored per profile. */
function useSkillBindings(skillId: string | undefined) {
  const profiles = useProfiles();
  const query = useQuery({
    queryKey: ["skill-bindings", skillId, profiles.items.map((p) => p.id).join(",")],
    enabled: !!skillId && profiles.items.length > 0,
    queryFn: async () => {
      const pairs = await Promise.all(
        profiles.items.map(async (profile) => {
          const result = await automation("skills.bindings", { profileId: profile.id });
          return result.items.find((b) => b.skillId === skillId);
        }),
      );
      return pairs.filter((b): b is Binding => !!b);
    },
  });
  return { profiles, bindings: query.data || [], pending: query.isPending && !!skillId };
}

export function SkillsPanel({ focus }: { focus?: string }) {
  const client = useQueryClient();
  const [selectedId, setSelectedId] = useState<string | undefined>(focus);
  const [creating, setCreating] = useState(false);
  const skills = useQuery({
    queryKey: ["skills"],
    queryFn: () => automation("skills.list", { limit: 100 }),
  });
  const items = skills.data?.items || [];
  const selected = items.find((s) => s.id === selectedId);

  return (
    <>
      <div className="section-toolbar">
        <span className="muted">
          Reviewed instructions you pin to a profile. A skill is text the agent reads. It never runs and never grants access.
        </span>
        <Button onClick={() => setCreating(true)}>
          <Plus size={16} />
          New skill
        </Button>
      </div>
      {skills.isError && <Notice>{skills.error.message}</Notice>}
      {skills.isPending ? (
        <p role="status">Loading skills…</p>
      ) : !items.length ? (
        <Empty
          icon={Sparkles}
          title="No skills yet"
          action={
            <Button onClick={() => setCreating(true)}>
              <Plus size={16} />
              Write your first skill
            </Button>
          }
        >
          Write how you want an agent to work, for example a review checklist or release routine, then pin it to the profiles that should follow it.
        </Empty>
      ) : (
        <div className="automation-split">
          <ul className="automation-pick-list" aria-label="Skills">
            {items.map((skill) => (
              <li key={skill.id}>
                <button
                  aria-pressed={skill.id === selectedId}
                  onClick={() => setSelectedId(skill.id)}
                >
                  <strong>{skill.name}</strong>
                  <small className="muted">{skill.description || "No description"} · v{skill.version}</small>
                </button>
              </li>
            ))}
          </ul>
          {selected ? (
            <SkillEditor key={selected.id + ":" + selected.revision} skill={selected} onSaved={() => client.invalidateQueries({ queryKey: ["skills"] })} />
          ) : (
            <p className="muted automation-split-hint">Select a skill to read or edit it.</p>
          )}
        </div>
      )}
      <NewSkillDialog
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={async (skill) => {
          await client.invalidateQueries({ queryKey: ["skills"] });
          setSelectedId(skill.id);
        }}
      />
    </>
  );
}

function SkillEditor({ skill, onSaved }: { skill: Skill; onSaved: () => Promise<unknown> }) {
  const client = useQueryClient();
  const [name, setName] = useState(skill.name || "");
  const [description, setDescription] = useState(skill.description || "");
  const [markdown, setMarkdown] = useState(skill.markdown || "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const { profiles, bindings, pending } = useSkillBindings(skill.id);
  const dirty = name !== skill.name || description !== (skill.description || "") || markdown !== (skill.markdown || "");
  const bytes = new TextEncoder().encode(markdown).length;

  async function save() {
    setBusy(true);
    setError("");
    try {
      await automation("skills.save", {
        id: skill.id,
        expectedRevision: skill.revision,
        name,
        description,
        markdown,
      });
      await onSaved();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function toggle(profileId: string, bound: Binding | undefined, enabled: boolean) {
    setBusy(true);
    setError("");
    try {
      await automation("skills.bind", {
        profileId,
        skillId: skill.id!,
        version: skill.version || skill.revision || 1,
        enabled,
        expectedRevision: bound?.revision ?? 0,
      });
      await client.invalidateQueries({ queryKey: ["skill-bindings"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="automation-card" aria-label="Skill editor">
      {error && <Notice>{error}</Notice>}
      <label htmlFor="skill-name">Name</label>
      <input id="skill-name" value={name} maxLength={128} onChange={(e) => setName(e.target.value)} />
      <label htmlFor="skill-description">Short description</label>
      <input id="skill-description" value={description} maxLength={512} onChange={(e) => setDescription(e.target.value)} />
      <label htmlFor="skill-markdown">Instructions</label>
      <textarea id="skill-markdown" rows={12} value={markdown} onChange={(e) => setMarkdown(e.target.value)} />
      <small className="muted">{bytes}/65536 bytes · saving creates version {(skill.revision || 0) + 1}; profiles keep the version they pinned until you re-pin.</small>
      {markdown && (
        <details className="automation-preview-toggle">
          <summary>Preview</summary>
          <div className="automation-preview"><Markdown text={markdown} /></div>
        </details>
      )}
      <div className="automation-sticky-actions">
        <Button disabled={busy || !dirty || !name.trim() || !markdown.trim()} onClick={() => void save()}>
          Save new version
        </Button>
        {!dirty && <span className="muted">All changes saved</span>}
      </div>
      <fieldset className="automation-used-by">
        <legend>Used by profiles</legend>
        {profiles.isPending || pending ? (
          <p role="status" className="muted">Checking profiles…</p>
        ) : !profiles.items.length ? (
          <p className="muted">Create a profile first, then pin this skill to it.</p>
        ) : (
          <ul>
            {profiles.items
              .filter((p) => !p.archived)
              .map((profile) => {
                const bound = bindings.find((b) => b.profileId === profile.id);
                const active = !!bound?.enabled;
                const stale = active && bound && bound.version !== (skill.version || skill.revision);
                return (
                  <li key={profile.id}>
                    <label>
                      <input
                        type="checkbox"
                        checked={active}
                        disabled={busy}
                        onChange={(e) => void toggle(profile.id, bound, e.target.checked)}
                      />
                      {profile.name}
                    </label>
                    {stale && (
                      <Button size="sm" variant="outline" disabled={busy} onClick={() => void toggle(profile.id, bound, true)}>
                        Update to v{skill.version || skill.revision} (pinned v{bound!.version})
                      </Button>
                    )}
                  </li>
                );
              })}
          </ul>
        )}
      </fieldset>
    </section>
  );
}

function NewSkillDialog({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: (skill: Skill) => Promise<void>;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [markdown, setMarkdown] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function create(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const skill = await automation("skills.save", { name, description, markdown });
      setName("");
      setDescription("");
      setMarkdown("");
      await onCreated(skill);
      onClose();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={() => !busy && onClose()} title="New skill" description="Plain instructions an agent reads before it works. Secrets are redacted on save." wide>
      <form className="form-stack" onSubmit={create}>
        {error && <Notice>{error}</Notice>}
        <label htmlFor="new-skill-name">Name</label>
        <input id="new-skill-name" required maxLength={128} value={name} onChange={(e) => setName(e.target.value)} placeholder="Pull request review" />
        <label htmlFor="new-skill-description">Short description</label>
        <input id="new-skill-description" maxLength={512} value={description} onChange={(e) => setDescription(e.target.value)} placeholder="How we review changes before merging" />
        <label htmlFor="new-skill-markdown">Instructions</label>
        <textarea id="new-skill-markdown" required rows={10} value={markdown} onChange={(e) => setMarkdown(e.target.value)} placeholder={"## When to use\nUse this when reviewing a pull request.\n\n## Steps\n1. Read the diff first.\n2. List risks before style notes."} />
        <div className="dialog-actions">
          <Button type="button" variant="outline" disabled={busy} onClick={onClose}>Cancel</Button>
          <Button type="submit" disabled={busy || !name.trim() || !markdown.trim()}>{busy ? "Saving…" : "Create skill"}</Button>
        </div>
      </form>
    </Dialog>
  );
}

/** Pin or unpin skills from inside the profile editor, so no one has to find the Skills tab first. */
export function ProfileSkills({ profileId }: { profileId: string }) {
  const client = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const skills = useQuery({ queryKey: ["skills"], queryFn: () => automation("skills.list", { limit: 100 }) });
  const bindings = useQuery({
    queryKey: ["profile-skill-bindings", profileId],
    queryFn: () => automation("skills.bindings", { profileId }),
  });
  const items = skills.data?.items || [];
  async function toggle(skill: Skill, enabled: boolean) {
    const bound = bindings.data?.items.find((b) => b.skillId === skill.id);
    setBusy(true);
    setError("");
    try {
      await automation("skills.bind", {
        profileId,
        skillId: skill.id!,
        version: skill.version || skill.revision || 1,
        enabled,
        expectedRevision: bound?.revision ?? 0,
      });
      await client.invalidateQueries({ queryKey: ["profile-skill-bindings", profileId] });
      await client.invalidateQueries({ queryKey: ["skill-bindings"] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <fieldset className="automation-used-by">
      <legend>Skills this profile follows</legend>
      {error && <Notice>{error}</Notice>}
      {skills.isPending || bindings.isPending ? (
        <p role="status" className="muted">Loading skills…</p>
      ) : !items.length ? (
        <p className="muted">
          No skills yet. <a href="#automation/skills">Write one</a>, then pin it here.
        </p>
      ) : (
        <ul>
          {items.map((skill) => (
            <li key={skill.id}>
              <label>
                <input
                  type="checkbox"
                  disabled={busy}
                  checked={!!bindings.data?.items.find((b) => b.skillId === skill.id)?.enabled}
                  onChange={(e) => void toggle(skill, e.target.checked)}
                />
                {skill.name}
              </label>
              <small className="muted">{skill.description}</small>
            </li>
          ))}
        </ul>
      )}
      <small className="muted">At most 16 skills per profile. Changes apply to the next message.</small>
    </fieldset>
  );
}
