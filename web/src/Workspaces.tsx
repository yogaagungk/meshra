import { useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowUp,
  FileText,
  Folder,
  FolderOpen,
  GitBranch,
  MessageSquare,
  Plus,
  RefreshCw,
} from "lucide-react";
import { api } from "./api";
import { Empty, Notice } from "./App";
import { DirectoryPicker } from "./DirectoryPicker";
import { Dialog } from "./components/ui/dialog";
import { Button } from "./components/ui/button";
import type { Workspace } from "./types";

type Entry = { name: string; path: string; directory: boolean };
type Tree = {
  path: string;
  entries: Entry[];
  hasMore: boolean;
  nextAfter: string;
};
type Git = { isRepository: boolean; branch?: string; status?: string };
export function Workspaces() {
  const client = useQueryClient();
  const workspaces = useQuery({
    queryKey: ["workspaces"],
    queryFn: () => api<Workspace[]>("workspaces"),
  });
  const [selected, setSelected] = useState("");
  const [adding, setAdding] = useState(false);
  const [picker, setPicker] = useState(false);
  const [folder, setFolder] = useState("");
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const current =
    workspaces.data?.find((w) => w.id === selected) || workspaces.data?.[0];
  async function add(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const w = await api<Workspace>("workspaces/add", { name, path: folder });
      await client.invalidateQueries({ queryKey: ["workspaces"] });
      setSelected(w.id);
      setAdding(false);
      setFolder("");
      setName("");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="workspace-page">
      <div className="workspace-toolbar">
        <label>
          Project
          <select
            value={current?.id || ""}
            onChange={(e) => setSelected(e.target.value)}
            disabled={!workspaces.data?.length}
          >
            <option value="" disabled>
              Select a workspace
            </option>
            {workspaces.data?.map((w) => (
              <option key={w.id} value={w.id}>
                {w.name}
              </option>
            ))}
          </select>
        </label>
        <Button
          onClick={() => {
            setError("");
            setAdding(true);
          }}
        >
          <Plus size={15} />
          Add workspace
        </Button>
      </div>
      {workspaces.isError && <Notice>{workspaces.error.message}</Notice>}
      {workspaces.isPending ? (
        <p role="status">Loading workspaces…</p>
      ) : current ? (
        <WorkspaceBrowser key={current.id} workspace={current} />
      ) : (
        !workspaces.isError && (
          <Empty icon={FolderOpen} title="Bring a project into Meshra">
            Add an existing directory to browse its files, review Git changes,
            and start an AI chat in that project.
          </Empty>
        )
      )}
      <Dialog
        open={adding}
        onOpenChange={setAdding}
        title="Add a coding workspace"
        description="Choose an existing project on the machine running Meshra."
      >
        <form className="personal-form" onSubmit={add}>
          <label>
            Workspace name
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              maxLength={80}
              placeholder="Defaults to the folder name"
            />
          </label>
          <label>
            Project directory
            <div className="workspace-folder-input">
              <input
                required
                value={folder}
                onChange={(e) => setFolder(e.target.value)}
                placeholder="/path/to/project"
              />
              <Button
                type="button"
                variant="outline"
                onClick={() => setPicker(true)}
              >
                <FolderOpen size={15} />
                Browse folders
              </Button>
            </div>
          </label>
          {error && <Notice>{error}</Notice>}
          <div className="personal-actions">
            <Button disabled={busy || !folder.trim()}>
              {busy ? "Adding…" : "Add project"}
            </Button>
          </div>
        </form>
      </Dialog>
      <DirectoryPicker
        open={picker}
        onOpenChange={setPicker}
        onSelect={(path) => {
          setFolder(path);
          setPicker(false);
        }}
      />
    </div>
  );
}
function WorkspaceBrowser({ workspace }: { workspace: Workspace }) {
  const [path, setPath] = useState(".");
  const [after, setAfter] = useState("");
  const [file, setFile] = useState("");
  const [view, setView] = useState("files");
  const [staged, setStaged] = useState(false);
  const tree = useQuery({
    queryKey: ["workspace-tree", workspace.id, path, after],
    queryFn: () =>
      api<Tree>("workspace/tree", {
        workspaceId: workspace.id,
        path,
        afterName: after,
      }),
    gcTime: 0,
    retry: false,
  });
  const preview = useQuery({
    queryKey: ["workspace-file", workspace.id, file],
    queryFn: () =>
      api<{ content: string; truncated: boolean }>("workspace/file", {
        workspaceId: workspace.id,
        path: file,
      }),
    enabled: !!file && view === "files",
    gcTime: 0,
    retry: false,
  });
  const git = useQuery({
    queryKey: ["workspace-git", workspace.id],
    queryFn: () => api<Git>("workspace/git", { workspaceId: workspace.id }),
    gcTime: 0,
    retry: false,
  });
  const diff = useQuery({
    queryKey: ["workspace-diff", workspace.id, staged, file],
    queryFn: () =>
      api<{ content: string }>("workspace/diff", {
        workspaceId: workspace.id,
        path: file,
        status: staged ? "staged" : "",
      }),
    enabled: view === "changes" && !!git.data?.isRepository,
    gcTime: 0,
    retry: false,
  });
  function navigate(value: string) {
    setPath(value || ".");
    setAfter("");
    setFile("");
  }
  function refresh() {
    void tree.refetch();
    void git.refetch();
    if (file && view === "files") void preview.refetch();
    if (view === "changes" && git.data?.isRepository) void diff.refetch();
  }
  const changes: { status: string; path: string }[] = [];
  const raw = (git.data?.status || "").split("\0");
  for (let i = 0; i < raw.length; i++) {
    const row = raw[i];
    if (!row) continue;
    changes.push({ status: row.slice(0, 2), path: row.slice(3) });
    if (/[RC]/.test(row.slice(0, 2))) i++;
  }
  return (
    <section className="workspace-browser">
      <div className="workspace-project-heading">
        <div>
          <h2>{workspace.name}</h2>
          <p className="workspace-path">{workspace.path}</p>
        </div>
        <div className="workspace-project-actions">
          <Button
            variant="outline"
            size="icon"
            aria-label="Refresh project"
            onClick={refresh}
          >
            <RefreshCw size={16} />
          </Button>
          <Button
            onClick={() =>
              window.dispatchEvent(
                new CustomEvent("meshra:project-chat", {
                  detail: workspace.id,
                }),
              )
            }
          >
            <MessageSquare size={15} />
            Chat in this project
          </Button>
        </div>
      </div>
      <div className="workspace-view-tabs">
        <div role="group" aria-label="Workspace view">
          <button
            aria-pressed={view === "files"}
            onClick={() => setView("files")}
          >
            Files
          </button>
          <button
            aria-pressed={view === "changes"}
            onClick={() => setView("changes")}
          >
            Changes{changes.length > 0 && " · " + changes.length}
          </button>
        </div>
        {git.data?.isRepository && (
          <span>
            <GitBranch size={14} />
            {git.data.branch || "Detached HEAD"}
          </span>
        )}
      </div>
      {git.isError && <Notice>{git.error.message}</Notice>}
      <div className="workspace-split">
        <div className="workspace-tree">
          {view === "files" ? (
            <>
              <div className="workspace-tree-heading">
                <Button
                  size="icon"
                  variant="ghost"
                  aria-label="Parent project directory"
                  disabled={path === "."}
                  onClick={() =>
                    navigate(path.split("/").slice(0, -1).join("/"))
                  }
                >
                  <ArrowUp size={15} />
                </Button>
                <span>{path === "." ? "Project files" : path}</span>
              </div>
              {tree.isPending && <p role="status">Loading files…</p>}
              {tree.isError && <Notice>{tree.error.message}</Notice>}
              <ul>
                {tree.data?.entries.map((entry) => (
                  <li key={entry.path}>
                    <button
                      aria-pressed={!entry.directory && file === entry.path}
                      title={entry.name}
                      onClick={() =>
                        entry.directory
                          ? navigate(entry.path)
                          : setFile(entry.path)
                      }
                    >
                      {entry.directory ? (
                        <Folder size={15} />
                      ) : (
                        <FileText size={15} />
                      )}
                      <span>{entry.name}</span>
                    </button>
                  </li>
                ))}
              </ul>
              {tree.data && !tree.data.entries.length && (
                <p>This directory is empty.</p>
              )}
              <div className="workspace-page-controls">
                {after && (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => setAfter("")}
                  >
                    First page
                  </Button>
                )}
                {tree.data?.hasMore && (
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setAfter(tree.data!.nextAfter)}
                  >
                    Next files
                  </Button>
                )}
              </div>
            </>
          ) : (
            <>
              <div className="workspace-tree-heading">
                <span>Git changes</span>
              </div>
              {git.isPending ? (
                <p role="status">Checking Git…</p>
              ) : git.data && !git.data.isRepository ? (
                <p>This directory is not a Git repository.</p>
              ) : (
                <>
                  <button
                    className="workspace-all-changes"
                    aria-pressed={!file}
                    onClick={() => setFile("")}
                  >
                    All changed files
                  </button>
                  <ul>
                    {changes.map((change, i) => (
                      <li key={change.path + i}>
                        <button
                          aria-pressed={file === change.path}
                          onClick={() => setFile(change.path)}
                        >
                          <code>{change.status}</code>
                          <span>{change.path}</span>
                        </button>
                      </li>
                    ))}
                  </ul>
                  {!changes.length && <p>Working tree is clean.</p>}
                </>
              )}
            </>
          )}
        </div>
        <div className="workspace-preview">
          {view === "files" ? (
            !file ? (
              <div className="workspace-preview-empty">
                <FileText size={24} />
                <p>Select a file to preview its contents.</p>
                <small>
                  Text previews are limited to 64 KiB. Files stay on this
                  machine.
                </small>
              </div>
            ) : (
              <>
                <div className="workspace-preview-heading">
                  <strong>{file}</strong>
                </div>
                {preview.isPending && <p role="status">Opening file…</p>}
                {preview.isError && <Notice>{preview.error.message}</Notice>}
                {preview.data && (
                  <>
                    {preview.data.truncated && (
                      <p role="status">Showing the first 64 KiB.</p>
                    )}
                    <pre tabIndex={0} aria-label="File contents">
                      {preview.data.content}
                    </pre>
                  </>
                )}
              </>
            )
          ) : (
            <>
              <div className="workspace-preview-heading">
                <strong>{file || "All changes"}</strong>
                <label>
                  <input
                    type="checkbox"
                    checked={staged}
                    onChange={(e) => setStaged(e.target.checked)}
                  />
                  Staged
                </label>
              </div>
              {diff.isFetching && <p role="status">Loading diff…</p>}
              {diff.isError && <Notice>{diff.error.message}</Notice>}
              {diff.data &&
                (diff.data.content ? (
                  <pre tabIndex={0} aria-label="Git diff">
                    {diff.data.content}
                  </pre>
                ) : (
                  <p>
                    No {staged ? "staged" : "unstaged"} diff. Untracked file
                    contents are available under Files.
                  </p>
                ))}
            </>
          )}
        </div>
      </div>
    </section>
  );
}
