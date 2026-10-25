import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { automation, uploadAttachment, downloadAttachment } from "./automation-api";
import { Button } from "./components/ui/button";
import { Notice } from "./App";

export function Attachments({ conversationId, selected, onSelect, enabled = true }: { conversationId: string; selected: string[]; onSelect: (ids: string[]) => void; enabled?: boolean }) {
  const client = useQueryClient();
  const [after, setAfter] = useState<string[]>([""]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const controller = useRef<AbortController | null>(null);
  const files = useQuery({ queryKey: ["automation", "attachments", conversationId, after.at(-1)], queryFn: () => automation("attachments.list", { conversationId, after: after.at(-1), limit: 50 }), gcTime: 0 });
  useEffect(() => () => controller.current?.abort(), []);
  async function upload(file: File) {
    setBusy(true); setError(""); controller.current = new AbortController();
    try {
      const attachment = await uploadAttachment(file, file.name, conversationId, controller.current.signal);
      onSelect([...selected, attachment.id]);
      setAfter([""]);
      await client.invalidateQueries({ queryKey: ["automation", "attachments", conversationId] });
    } catch (e) { if (!controller.current.signal.aborted) setError((e as Error).message); }
    finally { setBusy(false); }
  }
  async function download(id: string, name: string) {
    setError("");
    try {
      const blob = await downloadAttachment(id);
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a"); link.href = url; link.download = name; link.click();
      window.setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (e) { setError((e as Error).message); }
  }
  async function remove(id: string) {
    if (!window.confirm("Delete this file? Historical references will show unavailable.")) return;
    try { await automation("attachments.remove", { id, confirm: true }); onSelect(selected.filter((value) => value !== id)); await files.refetch(); }
    catch (e) { setError((e as Error).message); }
  }
  return <section className="automation-card"><strong>Conversation files</strong>
    <p className="muted">Select files for the next queued message. Files are data, not permission grants.</p>
    <input type="file" aria-label="Upload conversation file" disabled={busy || !enabled} onChange={(e) => { const file = e.target.files?.[0]; if (file) void upload(file); e.target.value = ""; }} />
    {!enabled && <p className="muted">This recipient has attachments disabled.</p>}
    {busy && <p role="status">Uploading…</p>}{error && <Notice>{error}</Notice>}{files.isError && <Notice>{files.error.message}</Notice>}
    <ul className="automation-task-list">{files.data?.map((file) => <li key={file.id}><label><input type="checkbox" checked={selected.includes(file.id)} disabled={file.unavailable || !enabled} onChange={() => onSelect(selected.includes(file.id) ? selected.filter((id) => id !== file.id) : [...selected, file.id])} /> {file.name} · {file.size} bytes {file.unavailable ? "(unavailable)" : ""}</label><div className="automation-actions"><Button type="button" variant="outline" disabled={file.unavailable} onClick={() => void download(file.id, file.name)}>Download</Button><Button type="button" variant="ghost" disabled={file.unavailable} onClick={() => void remove(file.id)}>Delete</Button></div></li>)}</ul>
    <div className="automation-actions"><Button type="button" variant="outline" disabled={after.length < 2} onClick={() => setAfter((pages) => pages.slice(0, -1))}>Previous files</Button><Button type="button" variant="outline" disabled={files.data?.length !== 50} onClick={() => setAfter((pages) => [...pages, files.data!.at(-1)!.id])}>Next files</Button></div>
  </section>;
}
